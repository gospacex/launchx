// Package dtmstub 是 srv-points dtm 分支面**单测专用**的内存 database/sql 驱动。
//
// 为什么需要它，而不能沿用仓内既有的 sqlite 夹具：mesh/micro-srv 的 repository
// 单测用的是 gorm.io/driver/sqlite，而那条驱动是 mattn/go-sqlite3 —— 需要
// CGO_ENABLED=1。本分支面的验收命令固定是 CGO_ENABLED=0（不带它本机 go build
// 会掉进 cgo 的交互式 TUI），于是 sqlite 这条路在验收口径下直接不可用。
// 加一个纯 Go 的 sqlite 依赖（modernc.org/sqlite）能解决，但那是全仓共享的
// go.mod/go.sum —— 四个域的分支面在并行落地，动它等于在别人正在改的清单上
// 落刀。故这里自己写一个只覆盖本分支面 SQL 面的内存驱动。
//
// 它求值的语句只有三类（外加 dtm 自己发的那条 barrier 插入），且**遇到任何
// 未登记的语句直接返回错误**：夹具漏跟一条新 SQL 时测试会红，而不是让假绿
// 通过。这是本包能替代真库的唯一前提 —— 若它「什么都接受」，那么它验证的就
// 不是业务逻辑，而只是「代码跑完了」。
//
// 事务是真事务：Begin 时整库快照、Rollback 时整体丢弃。所以「余额不足必须把
// 同事务里那条流水一起回滚」这类断言在夹具上成立，而不是靠 dbmock 的
// expect 顺序假装成立。
//
// 真库口径另有一次复核：本包支持用真 MySQL 跑同一批测试（见 harness 里的
// HUBX_DTM_TEST_DSN），用来证明这里的列名与 SQL 都不是虚构的 —— 夹具的列是
// 本包自己写的，只有真库能作证。
package dtmstub

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// driverName 驱动名。**不能**叫 "sqlite"/"sqlite3"：那是仓内 gorm sqlite
// 夹具占着的名字，重复注册会 panic，测试之间会互相踩。
const driverName = "dtmstub"

func init() { sql.Register(driverName, stubDriver{}) }

// ---------- 内存表 ----------

// table 内存表。unique 声明**组合唯一键**的列序（对齐 schema 里的
// uk_pa_user / uk_ledger_order_type / uniq_barrier），索引是本夹具唯一
// 「懂」数据库语义的地方：幂等断言全部靠它。
type table struct {
	cols   []string
	unique [][2]string
	rows   []map[string]any
}

func (t *table) clone() *table {
	cp := &table{cols: t.cols, unique: t.unique, rows: make([]map[string]any, len(t.rows))}
	for i, r := range t.rows {
		nr := make(map[string]any, len(r))
		for k, v := range r {
			nr[k] = v
		}
		cp.rows[i] = nr
	}
	return cp
}

func (t *table) find(col, val string) int {
	for i, r := range t.rows {
		if asString(r[col]) == val {
			return i
		}
	}
	return -1
}

func (t *table) has(cols [2]string, key [2]string) bool {
	for _, r := range t.rows {
		if asString(r[cols[0]]) == key[0] && asString(r[cols[1]]) == key[1] {
			return true
		}
	}
	return false
}

type dataset map[string]*table

func (d dataset) clone() dataset {
	out := make(dataset, len(d))
	for k, v := range d {
		out[k] = v.clone()
	}
	return out
}

// ---------- 对外句柄 ----------

// DB 一份内存库。三张表的列**逐字取自** examples/micro/scripts/schema-micro.sql
// 与 examples/mall/scripts/schema.sql（points 段）：point_accounts.frozen 是
// TCC 增量列，point_ledgers 的 (order_no, type) 组合唯一键是账变幂等的第二道闸，
// barrier 的 (gid, branch_id, op, barrier_id) 是 dtm 官方的唯一键。
type DB struct {
	mu   sync.Mutex
	key  string
	data dataset
}

var seq atomic.Int64

// New 造一份空库（无账户、无流水、无 barrier 行）。
func New() *DB {
	key := "dtmstub-" + strconv.FormatInt(seq.Add(1), 10)
	d := &DB{key: key, data: newDataset()}
	storesMu.Lock()
	stores[key] = d
	storesMu.Unlock()
	return d
}

func newDataset() dataset {
	return dataset{
		"point_accounts": {
			cols:   []string{"id", "user_id", "balance", "frozen", "total_earned", "total_spent"},
			unique: [][2]string{{"user_id", "user_id"}},
		},
		"point_ledgers": {
			cols:   []string{"id", "user_id", "order_no", "type", "amount", "balance_after", "remark"},
			unique: [][2]string{{"order_no", "type"}},
		},
		"barrier": {
			cols:   []string{"id", "trans_type", "gid", "branch_id", "op", "barrier_id", "reason"},
			unique: [][2]string{{"gid", "branch_id"}, {"op", "barrier_id"}},
		},
	}
}

// Seed 插入一个积分账户（等价于测试前置：用户已开户）。
func (d *DB) Seed(userID, balance int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	acc := d.data["point_accounts"]
	acc.rows = append(acc.rows, map[string]any{
		"id": int64(len(acc.rows) + 1), "user_id": userID, "balance": balance,
		"frozen": int64(0), "total_earned": balance, "total_spent": int64(0),
	})
}

// Account 账户快照（balance / frozen / total_earned / total_spent）。
type Account struct {
	UserID      int64
	Balance     int64
	Frozen      int64
	TotalEarned int64
	TotalSpent  int64
	Found       bool
}

// Account 读账户当前值。走内存态直读，**不经过 SQL 求值**：让断言与被测逻辑
// 不共用同一套 SQL，否则夹具的求值 bug 会同时骗过被测代码和断言。
func (d *DB) Account(userID int64) Account {
	d.mu.Lock()
	defer d.mu.Unlock()
	acc := d.data["point_accounts"]
	i := acc.find("user_id", strconv.FormatInt(userID, 10))
	if i < 0 {
		return Account{UserID: userID}
	}
	r := acc.rows[i]
	return Account{
		UserID: userID, Balance: asInt(r["balance"]), Frozen: asInt(r["frozen"]),
		TotalEarned: asInt(r["total_earned"]), TotalSpent: asInt(r["total_spent"]),
		Found: true,
	}
}

// Ledger 流水快照。
type Ledger struct {
	ID           int64
	UserID       int64
	OrderNo      string
	Type         int64
	Amount       int64
	BalanceAfter int64
	Remark       string
}

// Ledgers 按插入序返回全部流水。
func (d *DB) Ledgers() []Ledger {
	d.mu.Lock()
	defer d.mu.Unlock()
	src := d.data["point_ledgers"].rows
	out := make([]Ledger, 0, len(src))
	for _, r := range src {
		out = append(out, Ledger{
			ID: asInt(r["id"]), UserID: asInt(r["user_id"]), OrderNo: asString(r["order_no"]),
			Type: asInt(r["type"]), Amount: asInt(r["amount"]), BalanceAfter: asInt(r["balance_after"]),
			Remark: asString(r["remark"]),
		})
	}
	return out
}

// Barriers 返回 barrier 行数（幂等/空补偿断言用）。
func (d *DB) Barriers() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.data["barrier"].rows)
}

// BarrierOps 返回 barrier 行里 (op, branch_id) 的清单，按插入序。
func (d *DB) BarrierOps() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.data["barrier"].rows))
	for _, r := range d.data["barrier"].rows {
		out = append(out, asString(r["op"])+"@"+asString(r["branch_id"]))
	}
	return out
}

// GormDB 用 gorm 包出 *gorm.DB（自定义 DriverName + 跳过版本探测）。
//
// 为什么非得是 gorm 而不是直接给 *sql.DB：被测的 RawDB 吃的是 *gorm.DB
// （从 launchx 的 dbx 句柄来），用 gorm.Open 打开才让这条「句柄来源」路径在
// 单测里也真的被走一遍。dialector 选 mysql 而不用 sqlite：只有 mysql
// dialector 支持 DriverName 覆写，而覆写驱动名正是把内存驱动接进 gorm 的入口。
func (d *DB) GormDB() (*gorm.DB, error) {
	return gorm.Open(mysql.New(mysql.Config{
		DriverName:                driverName,
		DSN:                       d.key,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
}

// SQL 直接返回裸连接（给不想过 gorm 的用例用）。
func (d *DB) SQL() *sql.DB { return mustOpen(d.key) }

func mustOpen(key string) *sql.DB {
	db, err := sql.Open(driverName, key)
	if err != nil {
		panic("dtmstub: open " + err.Error())
	}
	return db
}

// stores 注册表：驱动名是全局的（sql.Register 只允许一次），连接与库实例靠
// DSN 里带的 key 一一对应 —— 于是每个测试可以各开一份互不干扰的库。
var (
	storesMu sync.Mutex
	stores   = map[string]*DB{}
)

func lookup(key string) *DB {
	storesMu.Lock()
	defer storesMu.Unlock()
	return stores[key]
}

type stubDriver struct{}

func (stubDriver) Open(name string) (driver.Conn, error) {
	d := lookup(name)
	if d == nil {
		return nil, fmt.Errorf("dtmstub: unknown store %q", name)
	}
	return &stubConn{db: d}, nil
}

// ---------- 连接 / 事务 ----------

type stubConn struct {
	db *DB
	tx dataset // 非 nil 表示事务进行中：写落在这里，Commit 才并回库里
}

func (c *stubConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("dtmstub: 只支持直连 Exec/Query，不支持预编译（业务与 dtm 都走 db.Exec）")
}
func (c *stubConn) Close() error               { return nil }
func (c *stubConn) Ping(context.Context) error { return nil }

// Begin 整库快照。回滚语义由此成立：barrier 事务里业务写失败时，同事务里
// 已插的 barrier 行与流水行一起消失 —— 「补偿已落库、正向没落库」这类状态
// 在夹具上是真的造不出来，而不是靠断言口头保证。
func (c *stubConn) Begin() (driver.Tx, error) {
	if c.tx != nil {
		return nil, fmt.Errorf("dtmstub: 嵌套事务不被支持（本分支面不应出现嵌套）")
	}
	c.tx = c.db.snapshot()
	return &stubTx{c: c}, nil
}

type stubTx struct{ c *stubConn }

func (t *stubTx) Commit() error {
	if t.c.tx == nil {
		return fmt.Errorf("dtmstub: commit 无事务")
	}
	t.c.db.commit(t.c.tx)
	t.c.tx = nil
	return nil
}

func (t *stubTx) Rollback() error {
	t.c.tx = nil // 丢弃快照即回滚
	return nil
}

func (d *DB) snapshot() dataset {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.data.clone()
}

func (d *DB) commit(tx dataset) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data = tx
}

// view 当前写入视图：事务进行中落快照，否则直落库。
//
// 直落那条路径不加锁：夹具的并发面为零（每个用例单协程顺序调用），加锁只会
// 让「快照 = 视图」这条不变式更难读。真实并发语义由真库复核那次来证明。
func (c *stubConn) view() dataset {
	if c.tx != nil {
		return c.tx
	}
	return c.db.snapshotView()
}

func (d *DB) snapshotView() dataset {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.data
}

// ---------- 语句求值 ----------

// ExecContext / QueryContext 走 driver 的可选接口，database/sql 因此不必
// 走 Prepare —— 也就绕开了本包要额外实现的一整套预编译桩。
func (c *stubConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	a := values(args)
	verb, table := stmtShape(lower(query))
	switch {
	case verb == "insert ignore into" && table == "barrier":
		return c.execBarrierInsert(a)
	case verb == "insert into" && table == "point_ledgers":
		return c.execLedgerInsert(a)
	case verb == "update" && table == "point_accounts":
		return c.execAccountUpdate(lower(query), a)
	default:
		return nil, fmt.Errorf("dtmstub: 不支持的写语句：%s（只认 barrier 的 insert ignore、point_ledgers 的 insert、point_accounts 的 update）", query)
	}
}

func (c *stubConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	a := values(args)
	verb, table := stmtShape(lower(query))
	if verb != "select" {
		return nil, fmt.Errorf("dtmstub: 不支持的读语句：%s", query)
	}
	if table == "point_accounts" {
		return c.queryAccount(lower(query), a)
	}
	return nil, fmt.Errorf("dtmstub: 不支持的读表：%s", query)
}

// stmtShape 拆出「动词」与「表名（去掉库前缀）」。
//
// 去库前缀是有必要的：dtm 的 barrier 表名是**进程级全局量**，未设置时是官方的
// `dtm_barrier.barrier`（库名+表名），设了才是本域的 `barrier`。两种写法在本包
// 认作同一张表，于是「忘了调 dtmx.SetBarrierTableName」不会在夹具上炸掉 —
// 而真机上那会让 barrier 行插进**不存在的库**，事务当场回滚、分支永远失败。
// 该漏由 dtm 包的 TestSetupSetsBarrierTableName 守，不靠这里。
func stmtShape(q string) (verb, table string) {
	switch {
	case strings.HasPrefix(q, "insert ignore into "):
		return "insert ignore into", tableToken(q[len("insert ignore into "):])
	case strings.HasPrefix(q, "insert into "):
		return "insert into", tableToken(q[len("insert into "):])
	case strings.HasPrefix(q, "update "):
		return "update", tableToken(q[len("update "):])
	case strings.HasPrefix(q, "select "):
		// select 的表名在 " from " 之后，不是紧跟动词 —— 「动词后第一个 token」
		// 对 select 来说是取色列。
		i := strings.Index(q, " from ")
		if i < 0 {
			return "select", ""
		}
		return "select", tableToken(q[i+len(" from "):])
	}
	return "", ""
}

// tableToken 取语句开头那个表名（到空格或左括号为止），并剥掉库前缀。
func tableToken(s string) string {
	stop := strings.IndexAny(s, " (")
	if stop < 0 {
		stop = len(s)
	}
	name := s[:stop]
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// execBarrierInsert 是 dtm 官方的子事务屏障插入（`insert ignore into barrier`）。
//
// 「ignore」语义在这里必须是真的：dtm 靠 RowsAffected==0 判定「这一段已经
// 落过账 / 空补偿 / 悬挂」，若夹具在这里报错或返回 1，barrier 的三条守卫就全部
// 失灵，而测试仍然会绿。
func (c *stubConn) execBarrierInsert(a []any) (driver.Result, error) {
	if len(a) != 6 {
		return nil, fmt.Errorf("dtmstub: barrier 插入应有 6 个实参，实际 %d", len(a))
	}
	d := c.view()
	tb := d["barrier"]
	gid, branch, op, barrierID := asString(a[1]), asString(a[2]), asString(a[3]), asString(a[4])
	if tb.has([2]string{"gid", "branch_id"}, [2]string{gid, branch}) &&
		tb.has([2]string{"op", "barrier_id"}, [2]string{op, barrierID}) {
		return driver.RowsAffected(0), nil
	}
	tb.rows = append(tb.rows, map[string]any{
		"id": int64(len(tb.rows) + 1), "trans_type": asString(a[0]), "gid": gid,
		"branch_id": branch, "op": op, "barrier_id": barrierID, "reason": asString(a[5]),
	})
	return driver.RowsAffected(1), nil
}

// execLedgerInsert 求值「条件插流水」：
//
//	insert into point_ledgers (...) select ?,?,?,?,?,? from dual
//	  where not exists (select 1 from point_ledgers where order_no = ? and type = ?)
//
// 这是账变幂等的**第二道闸**（第一道是 dtm 的 barrier）：条件判断与插入在
// 同一条语句里完成，判据是表里当下有没有同 (order_no, type) 的行，而不是
// 「先查再插」那种有窗口的写法。MySQL 8 支持该写法，本包据此实现其语义。
func (c *stubConn) execLedgerInsert(a []any) (driver.Result, error) {
	if len(a) != 8 {
		return nil, fmt.Errorf("dtmstub: 流水插入应有 8 个实参（6 插入 + 2 去重判据），实际 %d", len(a))
	}
	d := c.view()
	lg := d["point_ledgers"]
	if lg.has([2]string{"order_no", "type"}, [2]string{asString(a[1]), asString(a[2])}) {
		return driver.RowsAffected(0), nil
	}
	lg.rows = append(lg.rows, map[string]any{
		"id": int64(len(lg.rows) + 1), "user_id": asInt(a[0]), "order_no": asString(a[1]),
		"type": asInt(a[2]), "amount": asInt(a[3]), "balance_after": asInt(a[4]),
		"remark": asString(a[5]),
	})
	return driver.RowsAffected(1), nil
}

// execAccountUpdate 求值账户改写。支持的语法面（窄，但每一条都被真实语句用到）：
//
//	set col = col + ? | col = col - ? | col = ?
//	where col = ? [and col >= ?]
//
// 「余额/冻结不足」的判据就是 WHERE 里的 `>=` 守卫：守卫不成立时受影响行数为
// 0，被测代码据此判业务失败。守卫写成 `<` 或漏写，夹具照样求值（它只认语法），
// 但真库复核会因「扣成负余额」或断言失败而暴露 —— 两层判据互补。
func (c *stubConn) execAccountUpdate(q string, a []any) (driver.Result, error) {
	const setMarker = "set "
	const whereMarker = " where "
	si := strings.Index(q, setMarker)
	wi := strings.Index(q, whereMarker)
	if si < 0 || wi < 0 || wi < si {
		return nil, fmt.Errorf("dtmstub: 认不出的 update 语句：%s", q)
	}
	sets := splitTop(q[si+len(setMarker) : wi])
	wheres := splitAnd(q[wi+len(whereMarker):])
	// 实参按**占位符在语句里出现的顺序**绑定：SET 在前、WHERE 在后。若按
	// 「先守卫后赋值」取参数，同一条语句里的余额与守卫额就会互换 ——
	// 守卫失效还会写成「扣成负余额」，而断言只看最终余额，很容易一起被骗。
	if len(a) != len(sets)+len(wheres) {
		return nil, fmt.Errorf("dtmstub: 实参数 %d 与占位符数 %d（SET %d + WHERE %d）不符：%s",
			len(a), len(sets)+len(wheres), len(sets), len(wheres), q)
	}
	setArgs, whereArgs := a[:len(sets)], a[len(sets):]

	acc := c.view()["point_accounts"]
	var affected int64
	for idx, r := range acc.rows {
		wi := 0
		ok, err := matchWhere(r, wheres, whereArgs, &wi)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // 守卫不过 = 这一行不受影响，且不得被改动
		}
		next := make(map[string]any, len(r))
		for k, v := range r {
			next[k] = v
		}
		si := 0
		for _, s := range sets {
			if err := applySet(next, s, setArgs, &si); err != nil {
				return nil, err
			}
		}
		acc.rows[idx] = next
		affected++
	}
	return driver.RowsAffected(affected), nil
}

func matchWhere(row map[string]any, clauses []string, a []any, i *int) (bool, error) {
	for _, c := range clauses {
		col, op, _, err := parsePredicate(c)
		if err != nil {
			return false, err
		}
		cur, ok := row[col]
		if !ok {
			return false, fmt.Errorf("dtmstub: 未知列 %q（本域表只有 user_id/balance/frozen/total_earned/total_spent）", col)
		}
		want := a[*i]
		*i++
		switch op {
		case "=":
			if asString(cur) != asString(want) {
				return false, nil
			}
		case ">=":
			if asInt(cur) < asInt(want) {
				return false, nil
			}
		default:
			return false, fmt.Errorf("dtmstub: 不支持的比较符 %q", op)
		}
	}
	return true, nil
}

func applySet(row map[string]any, clause string, a []any, i *int) error {
	col, op, _, err := parsePredicate(clause)
	if err != nil {
		return err
	}
	cur, ok := row[col]
	if !ok {
		return fmt.Errorf("dtmstub: 未知列 %q", col)
	}
	val := a[*i]
	*i++
	switch op {
	case "=":
		row[col] = val
	case "+":
		row[col] = asInt(cur) + asInt(val)
	case "-":
		row[col] = asInt(cur) - asInt(val)
	default:
		return fmt.Errorf("dtmstub: 不支持的赋值形式 %q（只支持 col = ? / col = col ± ?）", clause)
	}
	return nil
}

// parsePredicate 解析 `col = ?` / `col >= ?` / `col = col ± ?`。
//
// 判据是「右值以 ` + ?` / ` - ?` 结尾」，而不是「左右两边看起来一样」：后者在
// `col = col - ?` 上要先切成三段再比对，漏掉比较那一步就会把它当成
// `col = <字面量>`，于是 `balance = balance - 30` 变成 `balance = 30` ——
// 余额被写成调用方传的金额，而断言只盯着「有没有变」，看起来一切正常。
func parsePredicate(clause string) (col, op string, rhs string, err error) {
	c := strings.TrimSpace(clause)
	if i := strings.Index(c, " >= "); i >= 0 {
		return strings.TrimSpace(c[:i]), ">=", "?", nil
	}
	i := strings.Index(c, " = ")
	if i < 0 {
		return "", "", "", fmt.Errorf("dtmstub: 认不出的条件/赋值：%q", clause)
	}
	col, right := strings.TrimSpace(c[:i]), strings.TrimSpace(c[i+len(" = "):])
	switch {
	case strings.HasSuffix(right, " + ?"):
		return col, "+", "?", nil
	case strings.HasSuffix(right, " - ?"):
		return col, "-", "?", nil
	case right == "?":
		return col, "=", "?", nil
	default:
		return "", "", "", fmt.Errorf("dtmstub: 不支持的右值 %q（本夹具只认占位符，不求值字面量）", right)
	}
}

func (c *stubConn) queryAccount(q string, a []any) (driver.Rows, error) {
	// 唯一形态：select balance, frozen from point_accounts where user_id = ? [for update]
	// 取数列也要核对：SELECT 出错列时若照发 balance/frozen，被测代码就会拿到
	// 别的语义的值还测得过（例如误 select 了 total_spent 当余额）。
	if !strings.HasPrefix(q, "select balance, frozen from point_accounts where user_id = ?") {
		return nil, fmt.Errorf("dtmstub: 不支持的账户查询：%s", q)
	}
	if len(a) != 1 {
		return nil, fmt.Errorf("dtmstub: 账户查询应有 1 个实参，实际 %d", len(a))
	}
	cols := []string{"balance", "frozen"}
	vals := [][]driver.Value{}
	acc := c.view()["point_accounts"]
	if i := acc.find("user_id", asString(a[0])); i >= 0 {
		vals = append(vals, []driver.Value{asInt(acc.rows[i]["balance"]), asInt(acc.rows[i]["frozen"])})
	}
	return &stubRows{cols: cols, vals: vals}, nil
}

type stubRows struct {
	cols []string
	vals [][]driver.Value
	pos  int
}

func (r *stubRows) Columns() []string { return r.cols }
func (r *stubRows) Close() error      { return nil }
func (r *stubRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.vals) {
		return io.EOF
	}
	copy(dest, r.vals[r.pos])
	r.pos++
	return nil
}

// ---------- 小工具 ----------

func values(args []driver.NamedValue) []any {
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = a.Value
	}
	return out
}

func lower(s string) string { return strings.ToLower(s) }

func asInt(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case int32:
		return int64(t)
	case uint64:
		return int64(t)
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	default:
		panic(fmt.Sprintf("dtmstub: 期望整型实参，实际 %T", v))
	}
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// splitTop 按逗号切分 SET 子句。SET 与 WHERE 的分隔符不同（`,` vs `and`），
// 分成两个函数而不是一个：合成一个的话，「少写一个 and」这种错会被当成
// 另一种分隔符静默吃掉。
func splitTop(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitAnd 按 and 切分 WHERE 子句。
func splitAnd(s string) []string {
	parts := strings.Split(s, " and ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
