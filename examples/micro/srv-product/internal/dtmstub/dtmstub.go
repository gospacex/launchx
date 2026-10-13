// Package dtmstub 是 srv-product dtm 分支面**单测专用**的内存 database/sql 驱动。
//
// 为什么需要它，而不能沿用仓内既有的 sqlite 夹具：examples/micro 下既有
// repository 单测用的 gorm.io/driver/sqlite，而那是 mattn/go-sqlite3 —— 需要
// CGO_ENABLED=1。本分支面的验收命令固定是 CGO_ENABLED=0（不带它本机 go build
// 会掉进 cgo 的交互式 TUI），于是 sqlite 这条路在验收口径下直接不可用。
// 加一个纯 Go 的 sqlite 依赖（modernc.org/sqlite）能解决，但那是全仓共享的
// go.mod/go.sum —— 四个域的分支面在并行落地，动它等于在别人正在改的清单上
// 落刀。故这里自己写一个只覆盖本分支面 SQL 面的内存驱动。
//
// 与 srv-stock/internal/dbmock 的分工（两者都是纯 Go 假驱动，选型不同）：
//   - dbmock 只按编排返回 affected 行数，验的是「代码有没有发出正确的语句、
//     业务失败有没有翻对」，**看不见 sold 的值**；
//   - 本驱动是**真内存库**：update 的 where 守卫被真的求值、事务是真回滚。
//
// 商品域要的专属判据恰好是值层面的 —— 「补偿回退后销量精确回到基线」与
// 「ReduceSales 不得扣成负数」两条断言，没有真值就只能靠 expect 列表假装成立。
//
// 它求值的语句只有四类（前三类是业务写，第四类是 dtm 自己发的那条 barrier 插入），
// 且**遇到任何未登记的语句直接返回错误**：夹具漏跟一条新 SQL 时测试会红，而不是
// 让假绿通过。这是本包能替代真库的唯一前提 —— 若它「什么都接受」，那么它验证的
// 就不是业务逻辑，而只是「代码跑完了」。
//
// 事务是真事务：Begin 时整库快照、Rollback 时整体丢弃。所以「ReduceSales 判定
// 账不平后，同事务里的诊断读也不许留下痕迹」这类断言在夹具上成立。
//
// 真库口径另有一次复核的入口留给将来：夹具的列是本包自己写的，只有真 MySQL 能
// 证明 dtmstore.go 里的列名与 SQL 都不是虚构的（那份复核在 dtmstore_test.go 的
// TestDtmStoreSQLUsesOnlySchemaColumns 里以「逐字抄 schema」的静态判据先兜一层）。
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

// table 内存表。unique 声明唯一键涉及的列序（barrier 的 uniq_barrier 是四列
// (gid, branch_id, op, barrier_id)，product_sales 的主键是单列 product_id）。
// 索引是本夹具唯一「懂」数据库语义的地方：幂等断言全部靠它。
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

type dataset map[string]*table

func (d dataset) clone() dataset {
	out := make(dataset, len(d))
	for k, v := range d {
		out[k] = v.clone()
	}
	return out
}

// ---------- 对外句柄 ----------

// DB 一份内存库。两张表的列**逐字取自**
// examples/micro/scripts/schema-micro.sql 的 product_db 段：
//   - product_sales：product_id 主键、sold、updated_at —— Saga 销量分支唯一的落点
//   - barrier       ：(gid, branch_id, op, barrier_id) 唯一键，dtm 官方的幂等裁决依据
type DB struct {
	mu   sync.Mutex
	key  string
	data dataset
}

var seq atomic.Int64

// New 造一份空库（无销量行、无 barrier 行）。
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
		"product_sales": {
			cols:   []string{"product_id", "sold", "updated_at"},
			unique: [][2]string{{"product_id", "product_id"}},
		},
		"barrier": {
			cols:   []string{"id", "trans_type", "gid", "branch_id", "op", "barrier_id", "reason"},
			unique: [][2]string{{"gid", "branch_id"}, {"op", "barrier_id"}},
		},
	}
}

// Seed 铺一个商品的销量行（等价于测试前置：商品已产生过历史销量）。
func (d *DB) Seed(productID, sold int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.data["product_sales"]
	s.rows = append(s.rows, map[string]any{
		"product_id": productID, "sold": sold, "updated_at": int64(0),
	})
}

// Sales 销量快照（sold）。Found=false 表示该商品还没有销量行 —— 与
// sold=0 是两回事：后者是「卖过但恰好归零」，前者是「从没落过账」。
type Sales struct {
	ProductID int64
	Sold      int64
	Found     bool
}

// SalesOf 直读内存态，**不经过 SQL 求值**：让断言与被测逻辑不共用同一套 SQL，
// 否则夹具的求值 bug 会同时骗过被测代码和断言。
func (d *DB) SalesOf(productID int64) Sales {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.data["product_sales"]
	i := s.find("product_id", strconv.FormatInt(productID, 10))
	if i < 0 {
		return Sales{ProductID: productID}
	}
	return Sales{ProductID: productID, Sold: asInt(s.rows[i]["sold"]), Found: true}
}

// Barriers 返回 barrier 行数。
func (d *DB) Barriers() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.data["barrier"].rows)
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
// 已插的 barrier 行与销量行的改动一起消失。
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
	q := lower(query)
	switch {
	case strings.HasPrefix(q, "insert ignore into ") && tableToken(q[len("insert ignore into "):]) == "barrier":
		return c.execBarrierInsert(a)
	case strings.HasPrefix(q, "insert into ") && tableToken(q[len("insert into "):]) == "product_sales":
		return c.execSalesUpsert(q, a)
	case strings.HasPrefix(q, "update ") && tableToken(q[len("update "):]) == "product_sales":
		return c.execSalesUpdate(q, a)
	default:
		return nil, fmt.Errorf("dtmstub: 不支持的写语句：%s（只认 barrier 的 insert ignore、product_sales 的 upsert 与 update）", query)
	}
}

func (c *stubConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	a := values(args)
	q := lower(query)
	if !strings.HasPrefix(q, "select ") || tableToken(q[strings.Index(q, " from ")+len(" from "):]) != "product_sales" {
		return nil, fmt.Errorf("dtmstub: 不支持的读语句：%s", query)
	}
	if !strings.HasPrefix(q, "select sold from product_sales where product_id = ?") {
		return nil, fmt.Errorf("dtmstub: 不支持的读语句：%s（本夹具只认「读一个商品的 sold」）", query)
	}
	if len(a) != 1 {
		return nil, fmt.Errorf("dtmstub: sold 查询应有 1 个实参，实际 %d", len(a))
	}
	cols := []string{"sold"}
	vals := [][]driver.Value{}
	s := c.view()["product_sales"]
	if i := s.find("product_id", asString(a[0])); i >= 0 {
		vals = append(vals, []driver.Value{asInt(s.rows[i]["sold"])})
	}
	return &stubRows{cols: cols, vals: vals}, nil
}

// tableToken 取语句开头那个表名（到空格或左括号为止），并剥掉库前缀。
//
// 去库前缀是有必要的：dtm 的 barrier 表名是**进程级全局量**，未设置时是官方的
// `dtm_barrier.barrier`（库名+表名），设了才是本域的 `barrier`。两种写法在本包
// 认作同一张表，于是「忘了调 dtmx.SetBarrierTableName」不会在夹具上炸掉 —— 而
// 真机上那会让 barrier 行插进**不存在的库**，事务当场回滚、分支永远失败。
// 该漏由 dtm 包的 TestSetupSetsBarrierTableName 守，不靠这里。
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
//
// 去重按**四列整体**（gid + branch_id + op + barrier_id），与 schema-micro.sql 里
// 的 uniq_barrier 完全一致 —— 而不是按 (gid,branch_id) 与 (op,barrier_id) 两个
// 二元组分别判。后者会跨全局事务误判：gid-b 的 compensate 探 (op,barrier_id)
// 时会撞上 gid-a 早已留下的一行，于是第二个全局事务的补偿被当成「重复请求」
// 而整个跳过 —— 销量永远停在 +n，而所有回调都报成功。
func (c *stubConn) execBarrierInsert(a []any) (driver.Result, error) {
	if len(a) != 6 {
		return nil, fmt.Errorf("dtmstub: barrier 插入应有 6 个实参，实际 %d", len(a))
	}
	d := c.view()
	tb := d["barrier"]
	transType, gid, branch := asString(a[0]), asString(a[1]), asString(a[2])
	op, barrierID, reason := asString(a[3]), asString(a[4]), asString(a[5])
	for _, r := range tb.rows {
		if asString(r["gid"]) == gid && asString(r["branch_id"]) == branch &&
			asString(r["op"]) == op && asString(r["barrier_id"]) == barrierID {
			return driver.RowsAffected(0), nil
		}
	}
	tb.rows = append(tb.rows, map[string]any{
		"id": int64(len(tb.rows) + 1), "trans_type": transType, "gid": gid,
		"branch_id": branch, "op": op, "barrier_id": barrierID, "reason": reason,
	})
	return driver.RowsAffected(1), nil
}

// upsertPrefix / upsertSuffix 是销量 upsert 语句的固定骨架。
//
// 之所以按**前后两段**匹配而不是整句相等：`on duplicate key update` 后面那段
// （`sold = sold + ?`）正是本域的核心判据，不能因为排版多一个空格就让夹具不再
// 求值它 —— 那等于把守卫从测试里悄悄摘掉。
const (
	upsertPrefix = "insert into product_sales (product_id, sold) values (?, ?)"
	upsertSuffix = "on duplicate key update sold = sold + ?"
)

// execSalesUpsert 求值「行不存在则建行、行存在则 sold += n」。
//
// 三个实参顺序是 (product_id, n, n)：VALUES 里那个 n 是**建行时的初值**，
// duplicate 分支那个 n 是**加量**。两者相等只因为本域两个分支都传同一个 q，
// 但语义不同 —— 把第三个实参写成 q-1 会让首次建行少记 1，而守卫（若照抄第一条
// 语句的断言）仍会绿。夹具按位置绑定正是为了让这种错在这里现形。
func (c *stubConn) execSalesUpsert(q string, a []any) (driver.Result, error) {
	if !strings.HasPrefix(q, upsertPrefix) || !strings.HasSuffix(q, upsertSuffix) {
		return nil, fmt.Errorf("dtmstub: 不支持的 product_sales upsert：%s（骨架应为 %s ... %s）", q, upsertPrefix, upsertSuffix)
	}
	if len(a) != 3 {
		return nil, fmt.Errorf("dtmstub: 销量 upsert 应有 3 个实参（product_id, 初值, 加量），实际 %d", len(a))
	}
	s := c.view()["product_sales"]
	if i := s.find("product_id", asString(a[0])); i >= 0 {
		s.rows[i]["sold"] = asInt(s.rows[i]["sold"]) + asInt(a[2])
		return driver.RowsAffected(2), nil // MySQL：命中 duplicate 时 affected=2
	}
	s.rows = append(s.rows, map[string]any{
		"product_id": asInt(a[0]), "sold": asInt(a[1]), "updated_at": int64(0),
	})
	return driver.RowsAffected(1), nil
}

// execSalesUpdate 求值 `update product_sales set ... where ...`。
// 支持的语法面（窄，但每一条都被真实语句用到）：
//
//	set col = col + ? | col = col - ? | col = ?
//	where col = ? [and col >= ?]
//
// 「不得扣成负数」的判据就是 WHERE 里的 `>=` 守卫：守卫不成立时受影响行数为
// 0，被测代码据此判业务失败并**事后**读一行 sold 定位成因。守卫写成 `>` 或漏写，
// 夹具照样求值（它只认语法），但真库上会把 sold 扣成负数 —— 而本文件
// TestReduceSalesNeverGoesNegative 会当场红。
func (c *stubConn) execSalesUpdate(q string, a []any) (driver.Result, error) {
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
	// 「先守卫后赋值」取参数，同一条语句里的减量与守卫额就会互换 ——
	// 守卫失效还会写成「扣成负数」，而断言只看最终 sold，很容易一起被骗。
	if len(a) != len(sets)+len(wheres) {
		return nil, fmt.Errorf("dtmstub: 实参数 %d 与占位符数 %d（SET %d + WHERE %d）不符：%s",
			len(a), len(sets)+len(wheres), len(sets), len(wheres), q)
	}
	setArgs, whereArgs := a[:len(sets)], a[len(sets):]

	s := c.view()["product_sales"]
	var affected int64
	for idx, r := range s.rows {
		ai := 0
		ok, err := matchWhere(r, wheres, whereArgs, &ai)
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
		ai = 0
		for _, s := range sets {
			if err := applySet(next, s, setArgs, &ai); err != nil {
				return nil, err
			}
		}
		s.rows[idx] = next
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
			return false, fmt.Errorf("dtmstub: 未知列 %q（product_sales 只有 product_id/sold/updated_at）", col)
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
// `col = <字面量>`，于是 `sold = sold - 3` 变成 `sold = 3` —— 销量被写成调用方
// 传的数，而断言只盯着「有没有变」，看起来一切正常。
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
