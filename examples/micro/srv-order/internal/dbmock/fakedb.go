// Package dbmock srv-order dtm 分支面测试用的纯 Go 假 database/sql 驱动。
//
// 为什么不用真库也不用第三方 mock 库：
//   - 真 MySQL 单测跑不动，CI 与本机都起不了；
//   - 仓内既有的 repository 夹具是 gorm.io/driver/sqlite，而那条驱动是
//     mattn/go-sqlite3 —— 要 cgo，本仓的验收命令固定 CGO_ENABLED=0；
//   - 引入 mock 库要改 go.mod，而 go.mod 是四个域并行改动的共享文件。
//
// 它与 srv-stock/internal/dbmock 的主要差别是：**orders / outbox /
// order_status_log / order_items 四张表是真的内存表**（那边只记录执行过的语句）。
// 订单域分支面最要紧的两条判据是「outbox 行只在 Confirm 之后出现」与
// 「作废补偿不重复落 outbox」，它们断言的是**行的有无**，不是语句的条数 ——
// 只记录语句的夹具只能证明「没调 insert」，证明不了「表里没有那一行」。
//
// 其余口径与 srv-stock 版一致，且都是照抄它的理由：
//   - barrier 表是真模拟的：dtm 的幂等裁决只认那条 `insert ignore` 影响了多少行
//     （0 即判重 / 空补偿，业务回调因此根本不被调用）。本驱动按 schema 的
//     uniq_barrier (gid, branch_id, op, barrier_id) 去重：新键回 1、重复键回 0。
//   - 事务是真事务：Begin 整库快照、Rollback 整体丢弃、Commit 才发布。
//     「业务失败必须把同事务里那条 outbox / 状态日志一起回滚」因此在夹具上
//     成立，而不是靠调用顺序假装成立。
//   - **遇到任何未登记的语句直接报错**：夹具漏跟一条新 SQL 时测试会红，
//     而不是让假绿通过。这是本包能替代真库的唯一前提 —— 若它「什么都接受」，
//     它验证的就只是「代码跑完了」。
package dbmock

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
	"testing"
)

// BarrierInsertPrefix dtm 官方插 barrier 行的语句前缀（dtmcli/dtmimp
// utils.go::InsertBarrier + mysqlDBSpecial.GetInsertIgnoreTemplate 拼出的
// `insert ignore into <表名>(trans_type, gid, branch_id, op, barrier_id, reason)`）。
const BarrierInsertPrefix = "insert ignore into barrier"

var dsnSeq atomic.Int64

// ---------- 内存表 ----------

// OrderRow orders 行。列名逐字取自 examples/mall/scripts/schema.sql 的 orders
// 建表语句 + schema-micro.sql 的 dtm_gid 增列。
type OrderRow struct {
	ID          int64
	UserID      int64
	OrderNo     string
	Status      string
	TotalAmount int64
	PaidAmount  int64
	DtmGID      string
}

// ItemRow order_items 行。
type ItemRow struct {
	ID          int64
	OrderID     int64
	ProductID   int64
	ProductName string
	Quantity    int64
	UnitPrice   int64
}

// StatusLogRow order_status_log 行。
type StatusLogRow struct {
	ID         int64
	OrderID    int64
	FromStatus string
	ToStatus   string
}

// OutboxRow outbox 行（列名取自 schema.sql 的 outbox）。
type OutboxRow struct {
	ID          int64
	Topic       string
	EventType   string
	Payload     string
	Traceparent string
	Status      int64
	RetryCount  int64
}

// barrierRow barrier 行（列定义见 schema-micro.sql 的 uniq_barrier）。
type barrierRow struct {
	TransType string
	Gid       string
	BranchID  string
	Op        string
	BarrierID string
	Reason    string
}

// dataset 整库快照。Begin 时克隆一份，Commit 才换上去。
type dataset struct {
	orders     []OrderRow
	items      []ItemRow
	statusLogs []StatusLogRow
	outboxes   []OutboxRow
	barriers   []barrierRow
	nextID     int64
}

func (d *dataset) clone() *dataset {
	cp := &dataset{
		orders:     append([]OrderRow(nil), d.orders...),
		items:      append([]ItemRow(nil), d.items...),
		statusLogs: append([]StatusLogRow(nil), d.statusLogs...),
		outboxes:   append([]OutboxRow(nil), d.outboxes...),
		barriers:   append([]barrierRow(nil), d.barriers...),
		nextID:     d.nextID,
	}
	return cp
}

func (d *dataset) nextIDFor() int64 {
	d.nextID++
	return d.nextID
}

// ---------- 对外句柄 ----------

// DB 一份内存库。
type DB struct {
	mu    sync.Mutex
	dsn   string
	data  *dataset
	stmts []string

	sql *sql.DB
}

// New 造一份空库（无订单、无事件、无 barrier 行），Cleanup 里关掉连接池。
func New(t testing.TB) *DB {
	t.Helper()
	d := &DB{data: &dataset{}}
	dsn := fmt.Sprintf("srv-order-dtmfake-%d", dsnSeq.Add(1))
	d.dsn = dsn
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

// BarrierCount 返回插过几次 barrier 行（两条：origin + current，见 dtmcli.BranchBarrier.Call）。
func (d *DB) BarrierCount() int { return d.ExecCount(BarrierInsertPrefix) }

// Orders 返回全部订单行。
func (d *DB) Orders() []OrderRow {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]OrderRow(nil), d.data.orders...)
}

// OrderByNo 按单号取一行。
func (d *DB) OrderByNo(orderNo string) (OrderRow, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, o := range d.data.orders {
		if o.OrderNo == orderNo {
			return o, true
		}
	}
	return OrderRow{}, false
}

// Outboxes 返回全部 outbox 行。
func (d *DB) Outboxes() []OutboxRow {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]OutboxRow(nil), d.data.outboxes...)
}

// Items 返回全部明细行。
func (d *DB) Items() []ItemRow {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]ItemRow(nil), d.data.items...)
}

// StatusLogs 返回全部状态流转行。
func (d *DB) StatusLogs() []StatusLogRow {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]StatusLogRow(nil), d.data.statusLogs...)
}

// SeedOrder 预置一行订单（用于构造「订单不存在 / 状态不是目标态」的起点）。
//
// 造数只从这里进：分支写路径若漏了 insert，Orders() 会是空的，用例立刻红，
// 而不会因为夹具替它把行补上而假绿。
func (d *DB) SeedOrder(o OrderRow) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if o.ID == 0 {
		o.ID = d.data.nextIDFor()
	}
	d.data.orders = append(d.data.orders, o)
}

// ResetLog 清空执行记录，**数据与 barrier 表保留**。
//
// 逆序分支的用例需要它：先让正向分支到达（留下 barrier 行与数据），再把记录
// 归零，于是断言只看见逆序那一次。若连 barrier 一起清，逆序分支会被判成空补偿
// —— 那条路径另有专门的用例，这里要测的是「该写的写了」。
func (d *DB) ResetLog() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stmts = nil
}

// ---------- 语句求值 ----------

// exec 是全部语句的唯一入口；data 决定写去哪（事务内是快照，事务外是活库）。
func (d *DB) exec(data *dataset, q string, args []driver.NamedValue) (affected, lastID int64, err error) {
	d.mu.Lock()
	d.stmts = append(d.stmts, q)
	d.mu.Unlock()

	n := norm(q)
	vals := values(args)
	switch {
	case strings.HasPrefix(n, BarrierInsertPrefix):
		return d.execBarrier(data, vals), 0, nil
	case n == "insert ignore into orders (user_id, order_no, status, total_amount, dtm_gid) values (?, ?, ?, ?, ?)":
		return d.insertOrder(data, vals)
	case n == "insert into order_items (order_id, product_id, product_name, quantity, unit_price) values (?, ?, ?, ?, ?)":
		if len(vals) != 5 {
			return 0, 0, fmt.Errorf("dbmock: order_items 插入期望 5 个参数，实得 %d", len(vals))
		}
		id := data.nextIDFor()
		data.items = append(data.items, ItemRow{
			ID: id, OrderID: asInt(vals[0]), ProductID: asInt(vals[1]),
			ProductName: asString(vals[2]), Quantity: asInt(vals[3]), UnitPrice: asInt(vals[4]),
		})
		return 1, id, nil
	case n == "insert into order_status_log (order_id, from_status, to_status) values (?, ?, ?)":
		if len(vals) != 3 {
			return 0, 0, fmt.Errorf("dbmock: order_status_log 插入期望 3 个参数，实得 %d", len(vals))
		}
		id := data.nextIDFor()
		data.statusLogs = append(data.statusLogs, StatusLogRow{
			ID: id, OrderID: asInt(vals[0]), FromStatus: asString(vals[1]), ToStatus: asString(vals[2]),
		})
		return 1, id, nil
	case n == "insert into outbox (topic, event_type, payload, traceparent, status, retry_count) values (?, ?, ?, ?, ?, ?)":
		if len(vals) != 6 {
			return 0, 0, fmt.Errorf("dbmock: outbox 插入期望 6 个参数，实得 %d", len(vals))
		}
		id := data.nextIDFor()
		data.outboxes = append(data.outboxes, OutboxRow{
			ID: id, Topic: asString(vals[0]), EventType: asString(vals[1]),
			Payload: asString(vals[2]), Traceparent: asString(vals[3]),
			Status: asInt(vals[4]), RetryCount: asInt(vals[5]),
		})
		return 1, id, nil
	case strings.HasPrefix(n, "update orders set "):
		return d.updateOrders(data, n, vals)
	default:
		return 0, 0, fmt.Errorf("dbmock: 未登记语句 %q —— 夹具漏跟一条新 SQL 时测试必须红，"+
			"而不是让假绿通过", q)
	}
}

// execBarrier 模拟 `insert ignore into barrier(...)`：op 为空官方直接返回 0
// （dtmcli/dtmimp/utils.go::InsertBarrier 的第一行），新键回 1、已存在回 0
// —— 这正是 dtm 判重 / 空补偿的唯一依据。
func (d *DB) execBarrier(data *dataset, vals []any) int64 {
	if len(vals) != 6 {
		return 0
	}
	row := barrierRow{
		TransType: asString(vals[0]), Gid: asString(vals[1]), BranchID: asString(vals[2]),
		Op: asString(vals[3]), BarrierID: asString(vals[4]), Reason: asString(vals[5]),
	}
	if row.Op == "" {
		return 0
	}
	for _, r := range data.barriers {
		if r.Gid == row.Gid && r.BranchID == row.BranchID && r.Op == row.Op && r.BarrierID == row.BarrierID {
			return 0
		}
	}
	data.barriers = append(data.barriers, row)
	return 1
}

// insertOrder `insert ignore into orders`：uk_order_no 命中即 0 行（幂等重放）。
func (d *DB) insertOrder(data *dataset, vals []any) (int64, int64, error) {
	if len(vals) != 5 {
		return 0, 0, fmt.Errorf("dbmock: orders 插入期望 5 个参数，实得 %d", len(vals))
	}
	orderNo := asString(vals[1])
	for _, o := range data.orders {
		if o.OrderNo == orderNo {
			// MySQL 的 insert ignore 命中唯一键时 LastInsertId 不可信（返回上一次
			// 的自增值或 0），故这里返回 0：调用点只应在 affected==1 时用它。
			return 0, 0, nil
		}
	}
	id := data.nextIDFor()
	data.orders = append(data.orders, OrderRow{
		ID: id, UserID: asInt(vals[0]), OrderNo: orderNo, Status: asString(vals[2]),
		TotalAmount: asInt(vals[3]), DtmGID: asString(vals[4]),
	})
	return 1, id, nil
}

// updateOrders 求值 `update orders set <sets> where <conds>`。
//
// 只认两种形态（就是本仓分支写实际用的两种）：
//   - set 子句 `col = ?`（取下一个参数）或 `col = col`（同行取值，如 paid_amount = total_amount）；
//   - where 子句 `col = ? and col = ? ...`（逐条消费参数，全部成立才命中）。
//
// 参数顺序按 SQL 里的出现顺序线性消费 —— 与 MySQL 的实参绑定一致，故断言
// 「条件更新命中 0 行」在夹具与真库上是同一件事。
func (d *DB) updateOrders(data *dataset, n string, vals []any) (int64, int64, error) {
	setPart, wherePart, ok := strings.Cut(n, " where ")
	if !ok {
		return 0, 0, fmt.Errorf("dbmock: update orders 必须带 where 判据（分支写不允许无条件更新）: %q", n)
	}
	sets := splitTop(strings.TrimPrefix(setPart, "update orders set "))
	conds := strings.Split(wherePart, " and ")

	// 参数按 SQL 里的**出现顺序**线性消费：set 子句的 ? 在 where 之前，
	// 所以判据的第一个参数下标是 set 里 ? 的个数。搞错这一处的现象是
	// 「值对不上 → 影响 0 行 → 一律被判业务失败」，很难往参数绑定上想。
	offset := 0
	for _, s := range sets {
		if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "")) != "" &&
			strings.Contains(s, " = ?") {
			offset++
		}
	}

	var affected int64
	for i := range data.orders {
		row := &data.orders[i]
		ci := offset
		hit := true
		for _, cond := range conds {
			col, rhs, perr := parsePredicate(strings.TrimSpace(cond))
			if perr != nil {
				return 0, 0, perr
			}
			if rhs != "?" || ci >= len(vals) {
				return 0, 0, fmt.Errorf("dbmock: 判据 %q 必须是 `col = ?`", cond)
			}
			if fieldOf(row, col) != asString(vals[ci]) {
				hit = false
				break
			}
			ci++
		}
		if !hit {
			continue
		}
		if err := applySets(row, sets, vals, 0); err != nil {
			return 0, 0, err
		}
		affected++
	}
	return affected, 0, nil
}

// applySets 按 `col = ?` / `col = col` 两种形态落 set 子句。
func applySets(row *OrderRow, sets []string, vals []any, from int) error {
	for _, s := range sets {
		col, expr, ok := strings.Cut(strings.TrimSpace(s), " = ")
		if !ok {
			return fmt.Errorf("dbmock: set 子句 %q 无法解析", s)
		}
		col, expr = strings.TrimSpace(col), strings.TrimSpace(expr)
		switch {
		case expr == "?":
			if from >= len(vals) {
				return fmt.Errorf("dbmock: set 子句 %q 缺参数", s)
			}
			setField(row, col, vals[from])
			from++
		case isIdent(expr):
			// 同行取值（paid_amount = total_amount）：MySQL 里右值取的是**更新前**
			// 的行，这里照抄该语义 —— 本仓用它把实收额对齐订单总额。
			setField(row, col, fieldOf(row, expr))
		default:
			return fmt.Errorf("dbmock: set 右值 %q 只支持 ? 或同名列", expr)
		}
	}
	return nil
}

func parsePredicate(s string) (col, placeholder string, err error) {
	c, rhs, ok := strings.Cut(s, " = ")
	if !ok {
		return "", "", fmt.Errorf("dbmock: 判据 %q 无法解析", s)
	}
	return strings.TrimSpace(c), strings.TrimSpace(rhs), nil
}

func fieldOf(o *OrderRow, col string) string {
	switch col {
	case "order_no":
		return o.OrderNo
	case "status":
		return o.Status
	case "dtm_gid":
		return o.DtmGID
	case "user_id":
		return strconv.FormatInt(o.UserID, 10)
	case "total_amount":
		return strconv.FormatInt(o.TotalAmount, 10)
	case "paid_amount":
		return strconv.FormatInt(o.PaidAmount, 10)
	default:
		return ""
	}
}

func setField(o *OrderRow, col string, v any) {
	switch col {
	case "status":
		o.Status = asString(v)
	case "dtm_gid":
		o.DtmGID = asString(v)
	case "paid_amount":
		o.PaidAmount = asInt(v)
	case "total_amount":
		o.TotalAmount = asInt(v)
	}
}

// selectOrders 求值本仓分支写用的唯一一条 SELECT：
// `select id from orders where order_no = ? and dtm_gid = ?`。
//
// 它的唯一用途是**回读刚落库的自增主键**（事件体与状态日志都要 order_id），
// 不参与任何判据 —— 判据一律在条件更新里。
func (d *DB) selectOrders(data *dataset, n string, vals []any) ([][]driver.Value, error) {
	_, wherePart, ok := strings.Cut(n, " where ")
	if !ok {
		return nil, fmt.Errorf("dbmock: 只支持带 where 的 select: %q", n)
	}
	if !strings.HasPrefix(strings.TrimSpace(n), "select id from orders") {
		return nil, fmt.Errorf("dbmock: 未登记的查询 %q —— 分支面只允许回读自增主键", n)
	}
	var out [][]driver.Value
	for _, o := range data.orders {
		ci := 0
		ok := true
		for _, cond := range strings.Split(wherePart, " and ") {
			col, rhs, perr := parsePredicate(strings.TrimSpace(cond))
			if perr != nil {
				return nil, perr
			}
			if rhs != "?" || ci >= len(vals) {
				return nil, fmt.Errorf("dbmock: 判据 %q 必须是 `col = ?`", cond)
			}
			if fieldOf(&o, col) != asString(vals[ci]) {
				ok = false
				break
			}
			ci++
		}
		if ok {
			out = append(out, []driver.Value{o.ID})
		}
	}
	return out, nil
}

// ---------- 小工具 ----------

// norm 归一化语句：小写 + 折叠空白。夹具按归一化后的整句匹配，故
// 「换个写法但语义相同」也会被判为未登记 —— 那是刻意的：改 SQL 必须改夹具。
func norm(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(q)), " ")
}

func values(args []driver.NamedValue) []any {
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = a.Value
	}
	return out
}

// rawValues 是 values 的 driver.Value 版本（走 Prepare 路径时 database/sql 给的
// 是前者，两者内容一致，故这里只是换个类型不改语义）。
func rawValues(args []driver.Value) []any {
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = a
	}
	return out
}

func asInt(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int32:
		return int64(t)
	case int:
		return int64(t)
	case uint64:
		return int64(t)
	case float64:
		return int64(t)
	case []byte:
		n, _ := strconv.ParseInt(string(t), 10, 64)
		return n
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	default:
		return 0
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
	default:
		return fmt.Sprint(v)
	}
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// splitTop 按顶层逗号切分（set 子句里没有嵌套括号，够用）。
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

// ---- driver ----

type fakeDriver struct{ db *DB }

func (d *fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{db: d.db}, nil }

// fakeConn 一条连接。
//
// tx 非 nil 表示这条连接正处在事务里 —— 这是本驱动能做出「真事务」的关键：
// database/sql 的 driver.Tx 接口**只有 Commit/Rollback**，事务里的语句仍然走
// conn 的 ExecContext/QueryContext。所以连接必须自己记住「我现在写到快照上去」。
// 忘了这一步的现象极其误导：Commit/Rollback 计数都对，而表已经脏了。
type fakeConn struct {
	db *DB
	tx *fakeTx
}

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return &fakeStmt{conn: c, q: query}, nil
}

func (c *fakeConn) Close() error { return nil }

func (c *fakeConn) Ping(context.Context) error { return nil }

func (c *fakeConn) Begin() (driver.Tx, error) {
	return c.beginTx(), nil
}

func (c *fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return c.beginTx(), nil
}

// beginTx 开一个真事务：整库快照，写落在快照上，Commit 才发布。
func (c *fakeConn) beginTx() driver.Tx {
	c.db.mu.Lock()
	snap := c.db.data.clone()
	c.db.mu.Unlock()
	c.tx = &fakeTx{conn: c, data: snap}
	return c.tx
}

// target 当前该往哪写：事务内写快照，事务外写活库。
func (c *fakeConn) target() *dataset {
	if c.tx != nil {
		return c.tx.data
	}
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	return c.db.data
}

func (c *fakeConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	return execResult(c.db.exec(c.target(), q, args))
}

func (c *fakeConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.db.selectOrders(c.target(), norm(q), values(args))
	if err != nil {
		return nil, err
	}
	return &stubRows{cols: []string{"id"}, rows: rows}, nil
}

func execResult(affected, lastID int64, err error) (driver.Result, error) {
	if err != nil {
		return nil, err
	}
	return execResultValue{affected: affected, lastID: lastID}, nil
}

type execResultValue struct{ affected, lastID int64 }

func (r execResultValue) LastInsertId() (int64, error) { return r.lastID, nil }
func (r execResultValue) RowsAffected() (int64, error) { return r.affected, nil }

// fakeStmt 走 Prepare 的那条路径（database/sql 在 ExecerContext 缺席时才用）。
// 写去哪仍由连接当时是否在事务里决定。
type fakeStmt struct {
	conn *fakeConn
	q    string
}

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }

func (s *fakeStmt) Exec(args []driver.Value) (driver.Result, error) {
	return execResult(s.conn.db.exec(s.conn.target(), s.q, namedValues(args)))
}

func (s *fakeStmt) Query(args []driver.Value) (driver.Rows, error) {
	rows, err := s.conn.db.selectOrders(s.conn.target(), norm(s.q), rawValues(args))
	if err != nil {
		return nil, err
	}
	return &stubRows{cols: []string{"id"}, rows: rows}, nil
}

// fakeTx 事务句柄：持有快照，收尾时决定发布还是丢弃。
type fakeTx struct {
	conn *fakeConn
	data *dataset
}

func (t *fakeTx) Commit() error {
	t.conn.db.mu.Lock()
	t.conn.db.data = t.data
	t.conn.db.mu.Unlock()
	t.conn.tx = nil
	return nil
}

// Rollback 直接丢弃快照：快照从头到尾没进过活库，回滚即「什么都没发生」。
func (t *fakeTx) Rollback() error {
	t.conn.tx = nil
	return nil
}

type stubRows struct {
	cols []string
	rows [][]driver.Value
	pos  int
}

func (r *stubRows) Columns() []string { return r.cols }
func (r *stubRows) Close() error      { return nil }

func (r *stubRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.pos])
	r.pos++
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
