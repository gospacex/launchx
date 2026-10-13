// Package dbmock dtm 分支面测试用的纯 Go 假 database/sql 驱动。
//
// 为什么不用真库也不用第三方 mock 库：
//   - 真 MySQL 单测跑不动，CI 与本机都起不了；
//   - mattn/go-sqlite3 要 cgo，而本仓的验收命令固定 CGO_ENABLED=0；
//   - 引入 mock 库要改 go.mod，而 go.mod 是四个域并行改动的共享文件。
//
// barrier 表是**真模拟**的：dtm 的幂等裁决只认一件事 —— 那条 `insert ignore`
// 影响了多少行（0 即「判重 / 空补偿」，业务回调因此根本不被调用）。所以本驱动
// 维护一张内存 barrier 表，按 (trans_type, gid, branch_id, op, barrier_id) 去重：
// 新键回 1、重复键回 0，与 MySQL 的 insert ignore 完全同形。
//
// 于是三条 dtm 语义不需要连库就能被钉死：
//   - 重复回调：同一 (gid, branch_id, op) 第二次到达 → 影响 0 行 → 不再执行业务写；
//   - 空补偿：compensate 到达时该 branch_id 的 action 行还不存在 → 影响 1 行
//     → 官方语义判空补偿，跳过业务写（注意：跳过时**照样**提交 barrier 行，
//     所以它判的是「正向从未到达」而不是「这次没写」）；
//   - 悬挂：try 崩溃后 cancel 到达，同一套判据把两边对齐。
//
// 其余语句按顺序记录，供断言 SQL 形状与业务语义。
//
// 刻意不支持 Query：分支面只允许「条件更新 + 流水插入」，一旦有人在事务里
// 读出来再算（即 read-modify-write，超卖的老路），本驱动当场报错而不是给个
// 假答案。
package dbmock

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// BarrierInsertPrefix dtm 官方插 barrier 行的语句前缀（dtmcli/dtmimp：
// mysqlDBSpecial.GetInsertIgnoreTemplate 把「表名+占位符」拼成 insert ignore into ...）。
const BarrierInsertPrefix = "insert ignore into barrier"

var dsnSeq atomic.Int64

// DB 假库句柄：记录执行过的语句，并按编排返回 affected 行数。
type DB struct {
	mu    sync.Mutex
	stmts []string
	// affects 查询片段 → affected 行数；取最长片段匹配，未命中默认 1
	affects map[string]int64
	// barrier 内存表：键为 (trans_type, gid, branch_id, op, barrier_id)
	barrier   map[string]struct{}
	commits   int
	rollbacks int

	sql *sql.DB
}

// New 建一个假库，Cleanup 里关掉连接池。
func New(t testing.TB) *DB {
	t.Helper()
	d := &DB{affects: map[string]int64{}, barrier: map[string]struct{}{}}
	dsn := fmt.Sprintf("srv-stock-dtmfake-%d", dsnSeq.Add(1))
	sql.Register(dsn, &fakeDriver{db: d})
	var err error
	d.sql, err = sql.Open(dsn, "")
	if err != nil {
		t.Fatalf("open fake db: %v", err)
	}
	t.Cleanup(func() { _ = d.sql.Close() })
	return d
}

// SQL 取出可交给 dtmx.Barrier.Call 的 *sql.DB。
func (d *DB) SQL() *sql.DB { return d.sql }

// Stmts 返回执行过的全部语句（测试失败时打印用）。
func (d *DB) Stmts() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.stmts...)
}

// ExecCount 统计执行过多少条包含 frag 的语句。
func (d *DB) ExecCount(frag string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, s := range d.stmts {
		if strings.Contains(s, frag) {
			n++
		}
	}
	return n
}

// BarrierCount 返回插过几次 barrier 行。
func (d *DB) BarrierCount() int { return d.ExecCount(BarrierInsertPrefix) }

// Commits / Rollbacks 事务收尾次数。barrier 的幂等语义依赖「业务失败时整笔
// 事务回滚、barrier 行也随之消失」（dtm 会重试），故这两个计数本身就是要断言的对象。
func (d *DB) Commits() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.commits
}

func (d *DB) Rollbacks() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rollbacks
}

// SetAffected 编排「语句含 frag 时」的 affected 行数。
//
// 库存不足用例靠它：把 update stocks 的 affected 设成 0，条件更新就判为
// 没命中，分支必须走业务失败而不是落一条错账。
func (d *DB) SetAffected(frag string, rows int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.affects[frag] = rows
}

// Reset 清空全部状态（含 barrier 表）。
func (d *DB) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stmts = nil
	d.commits, d.rollbacks = 0, 0
	d.barrier = map[string]struct{}{}
}

// ResetLog 只清空执行记录与事务计数，**保留 barrier 表**。
//
// 逆序分支的用例需要这个：先让正向分支到达（留下 barrier 行），再把记录归零，
// 然后只观察逆序那一次写了什么。若用 Reset 把 barrier 也清了，逆序分支会被
// 判成空补偿 —— 那条路径另有专门的用例，这里要测的是「该补的补到了」。
func (d *DB) ResetLog() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stmts = nil
	d.commits, d.rollbacks = 0, 0
}

func (d *DB) exec(q string, args []driver.NamedValue) (int64, error) {
	d.mu.Lock()
	d.stmts = append(d.stmts, q)
	d.mu.Unlock()
	if strings.HasPrefix(strings.TrimSpace(q), BarrierInsertPrefix) {
		return d.insertBarrier(args), nil
	}
	d.mu.Lock()
	best, n := "", int64(1)
	for frag, rows := range d.affects {
		if strings.Contains(q, frag) && len(frag) > len(best) {
			best, n = frag, rows
		}
	}
	d.mu.Unlock()
	return n, nil
}

// insertBarrier 模拟 `insert ignore into barrier(...)`：op 为空官方直接返回 0，
// 新键回 1、已存在回 0（这正是 dtm 判重 / 空补偿的唯一依据）。
func (d *DB) insertBarrier(args []driver.NamedValue) int64 {
	if len(args) < 5 {
		return 0
	}
	var op string
	if s, ok := args[3].Value.(string); ok {
		op = s
	}
	if op == "" {
		return 0
	}
	parts := make([]string, 5)
	for i := range parts {
		parts[i] = fmt.Sprint(args[i].Value)
	}
	key := strings.Join(parts, "|")
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dup := d.barrier[key]; dup {
		return 0
	}
	d.barrier[key] = struct{}{}
	return 1
}

// ---- driver ----

type fakeDriver struct{ db *DB }

func (d *fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{db: d.db}, nil }

type fakeConn struct{ db *DB }

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return &fakeStmt{db: c.db, q: query}, nil
}

func (c *fakeConn) Close() error { return nil }

func (c *fakeConn) Begin() (driver.Tx, error) { return &fakeTx{db: c.db}, nil }

func (c *fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return &fakeTx{db: c.db}, nil
}

func (c *fakeConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	n, err := c.db.exec(q, args)
	if err != nil {
		return nil, err
	}
	return driver.RowsAffected(n), nil
}

func (c *fakeConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	return nil, fmt.Errorf("dbmock: 分支面事务内不允许查询（收到 %q）—— 条件更新才是防超卖的那道闸", q)
}

type fakeStmt struct {
	db *DB
	q  string
}

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }

func (s *fakeStmt) Exec(args []driver.Value) (driver.Result, error) {
	n, err := s.db.exec(s.q, namedValues(args))
	if err != nil {
		return nil, err
	}
	return driver.RowsAffected(n), nil
}

func (s *fakeStmt) Query(_ []driver.Value) (driver.Rows, error) { return nil, io.EOF }

type fakeTx struct{ db *DB }

func (t *fakeTx) Commit() error {
	t.db.mu.Lock()
	t.db.commits++
	t.db.mu.Unlock()
	return nil
}

func (t *fakeTx) Rollback() error {
	t.db.mu.Lock()
	t.db.rollbacks++
	t.db.mu.Unlock()
	return nil
}

// namedValues 把 driver.Value 列表转成 ExecContext 用的 NamedValue 列表
// （走 Prepare 路径时 database/sql 给的是前者）。
func namedValues(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}
