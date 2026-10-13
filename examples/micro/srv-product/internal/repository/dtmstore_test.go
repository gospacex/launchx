package repository

import (
	"context"
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/dtm-labs/client/dtmcli"
	"google.golang.org/grpc/metadata"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/dtmstub"
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/model"
)

// TestMain 把 dtm 的 barrier 表名设成本域的 "barrier"。
//
// 这是进程级全局量（dtmcli.SetBarrierTableName → dtmimp.BarrierTableName），
// 不设就是官方默认的 `dtm_barrier.barrier`：barrier 行会插进一个本域根本不
// 存在的库，事务当场回滚，分支永远失败 —— 而 dtm 侧看到的只是「分支一直
// 失败」，排查成本极高。生产路径上这一步在 internal/dtm 的装配里（app 启动
// 时调一次），由 dtm 包的 TestSetupSetsBarrierTableName 单独守住。
func TestMain(m *testing.M) {
	dtmx.SetBarrierTableName("barrier")
	os.Exit(m.Run())
}

// 本文件覆盖商品域 dtm 销量分支（AddSales / ReduceSales）的语义边界：
//
//	 1. 正常写成功 —— 销量按 +n / -n 推进，barrier 行与业务写同事务提交；
//	 2. 业务失败   —— 扣成负数（账不平）/ 记录不存在，带官方哨兵，dtm 立刻
//	                转补偿而不是退避重试；
//	 3. barrier 幂等重放 —— 同 gid 同分支第二次到达，一次业务写都不许多。
//
// 夹具是真事务 + 真值（dtmstub 的 Begin 是整库快照、where 守卫被真的求值），
// 所以「回退后精确回到基线」是断言出来的，不是假设出来的。
//
// **branch_id 的取法照抄 dtm server**（dtmsvr/trans_type_saga.go 的 GenBranches）：
// 一个 saga 步骤的 action 与 compensate 共用同一个 branch_id（"01"/"02"/…），
// 且同一个 gid 下同 branch_id 只会有一次 action。barrier 判空补偿正是靠这个
// 共用 id 去探「同一步的正向行在不在」，所以补偿用例必须先跑正向。

// dtmCtx 造一条 dtm server 回调的上下文：dtmgimp.TransBaseFromGrpc 从
// incoming metadata 里读 dtm- 前缀的四个键（gid / trans_type / branch_id /
// op），少一个 dtmx.BarrierFromCtx 就取不到 barrier。
func dtmCtx(t *testing.T, gid, branchID, op string) context.Context {
	t.Helper()
	return metadata.NewIncomingContext(t.Context(), metadata.Pairs(
		"dtm-gid", gid,
		"dtm-trans_type", "saga",
		"dtm-branch_id", branchID,
		"dtm-op", op,
	))
}

func newRepo(t *testing.T) (*ProductRepository, *dtmstub.DB) {
	t.Helper()
	mem := dtmstub.New()
	gdb, err := mem.GormDB()
	if err != nil {
		t.Fatalf("开夹具库失败：%v", err)
	}
	return NewProductRepository(gdb, false), mem
}

func cmd(orderNo string, productID uint64, qty int64) model.DtmSalesCmd {
	return model.DtmSalesCmd{OrderNo: orderNo, ProductID: productID, Quantity: qty}
}

// inBarrier 按 service 层 dtmBranch 的真实编排跑一次分支写：RawDB 取业务连接 →
// dtmx.BarrierFromCtx 取 barrier → barrier.Call 把 tx 递进来。
//
// 为什么仓库层测试也要经过 barrier，而不是自己 db.Begin() 造一个 tx：后者只能
// 验到 SQL 本身，验不到「这条写确实落在 barrier 事务里」—— 而那正是本文件
// 第 3 类判据的前提（barrier 行与销量写同事务提交/回滚）。
func inBarrier(t *testing.T, repo *ProductRepository, ctx context.Context,
	write func(ctx context.Context, tx *sql.Tx) error) error {
	t.Helper()
	db, err := RawDB(repo.DB())
	if err != nil {
		t.Fatalf("RawDB: %v", err)
	}
	bar, err := dtmx.BarrierFromCtx(ctx)
	if err != nil {
		t.Fatalf("取 barrier 失败：%v", err)
	}
	return bar.Call(ctx, db, write)
}

func addSales(t *testing.T, repo *ProductRepository, ctx context.Context, c model.DtmSalesCmd) error {
	t.Helper()
	return inBarrier(t, repo, ctx, func(_ context.Context, tx *sql.Tx) error { return repo.DtmAddSales(ctx, tx, c) })
}

func reduceSales(t *testing.T, repo *ProductRepository, ctx context.Context, c model.DtmSalesCmd) error {
	t.Helper()
	return inBarrier(t, repo, ctx, func(_ context.Context, tx *sql.Tx) error { return repo.DtmReduceSales(ctx, tx, c) })
}

// ---- RawDB ----

// TestRawDBRejectsNilHandle 钉住「nil 入参报错」。
//
// 反向探针：把这里的 nil 判据去掉（直接 gdb.DB()），本用例必红 —— 而生产上
// 它会变成 nil 解引用 panic，装配期问题被推迟到第一次分支回调才炸。
func TestRawDBRejectsNilHandle(t *testing.T) {
	if db, err := RawDB(nil); err == nil {
		t.Fatalf("RawDB(nil) 必须报错，实际返回了 %v —— 装配期的句柄缺失会被推迟成运行期 panic", db)
	}
}

// TestRawDBReturnsTheDbxConnection 合格句柄原样取出底层 *sql.DB。
func TestRawDBReturnsTheDbxConnection(t *testing.T) {
	repo, _ := newRepo(t)
	got, err := RawDB(repo.DB())
	if err != nil {
		t.Fatalf("RawDB: %v", err)
	}
	if got == nil {
		t.Fatal("RawDB 返回了 nil *sql.DB")
	}
	if err := got.Ping(); err != nil {
		t.Fatalf("取出的连接不可用：%v", err)
	}
}

// ---- AddSales（Saga 正向）----

// TestAddSalesAccumulatesOnExistingRow：已有销量行时 sold += n。
func TestAddSalesAccumulatesOnExistingRow(t *testing.T) {
	repo, mem := newRepo(t)
	mem.Seed(7, 100)
	ctx := dtmCtx(t, "gid-add", "01", "action")

	if err := addSales(t, repo, ctx, cmd("SO-1", 7, 5)); err != nil {
		t.Fatalf("DtmAddSales: %v", err)
	}
	if got := mem.SalesOf(7); !got.Found || got.Sold != 105 {
		t.Fatalf("AddSales 后应为 sold=105，实际 %+v", got)
	}
	if mem.Barriers() != 1 {
		t.Fatalf("应留下 1 行 barrier（正向 op=action），实际 %d 行", mem.Barriers())
	}
}

// TestAddSalesUpsertsMissingRow：销量行不存在时 upsert 建行。
//
// 这一条不是可有可无的健壮性：product_sales 是**销量专表**，一个从没卖过的
// 商品在表里就没有行。若正向只写 update，第一次下单会「什么都没扣」—— 销量
// 永久少 1，而四个分支全部报成功。
//
// 反向探针：把 sqlAddSales 换成 `update product_sales set sold = sold + ?
// where product_id = ?`，本用例必红。
func TestAddSalesUpsertsMissingRow(t *testing.T) {
	repo, mem := newRepo(t)
	ctx := dtmCtx(t, "gid-upsert", "01", "action")

	if err := addSales(t, repo, ctx, cmd("SO-2", 9, 2)); err != nil {
		t.Fatalf("DtmAddSales: %v", err)
	}
	got := mem.SalesOf(9)
	if !got.Found || got.Sold != 2 {
		t.Fatalf("无销量行时应 upsert 建行且 sold=2，实际 %+v —— update-only 写法会让首单永久少记", got)
	}
}

// TestAddSalesBarrierReplayIsIdempotent：同 gid 同分支重复回调只加一次。
//
// 这是 barrier 的核心契约（dtm 侧靠 (gid, branch_id, op, barrier_id) 唯一键
// 裁决）。若这条红了，表现是同一笔订单被重复记销量，且两次调用都「成功」。
func TestAddSalesBarrierReplayIsIdempotent(t *testing.T) {
	repo, mem := newRepo(t)
	mem.Seed(7, 10)
	ctx := dtmCtx(t, "gid-add-replay", "01", "action")

	for i := 0; i < 3; i++ {
		if err := addSales(t, repo, ctx, cmd("SO-3", 7, 4)); err != nil {
			t.Fatalf("第 %d 次 DtmAddSales: %v", i+1, err)
		}
	}
	if got := mem.SalesOf(7); got.Sold != 14 {
		t.Fatalf("重放后应仍 sold=14，实际 %d —— barrier 幂等失效，同一笔被重复记销量", got.Sold)
	}
}

// ---- ReduceSales（Saga 补偿）----

// TestReduceSalesRestoresExactlyToBaseline：**专属判据**。正向 +n、补偿 -n 之后，
// 销量必须精确回到基线 —— 不少一分（少的是别的订单），也不多一分（多扣的是
// 已经卖出去的货）。
//
// 为什么要精确而不是「大致不变」：sold 是商品域唯一的账变落点，补偿差 1 的症状
// 是首页销量与订单数永久对不上，而**没有任何一条日志会报这个错**。
func TestReduceSalesRestoresExactlyToBaseline(t *testing.T) {
	repo, mem := newRepo(t)
	const baseline = 100
	mem.Seed(7, baseline)

	// 两笔独立订单各自走一遍 正向 → 补偿。不同 gid = 不同全局事务；同一个 gid
	// 内同 branch_id 只允许一次 action（dtm 的语义如此，barrier 会判重）。
	for i, qty := range []int64{3, 7} {
		gid := "gid-baseline-" + strconv.Itoa(i)
		order := "SO-B" + strconv.Itoa(i)

		addCtx := dtmCtx(t, gid, "01", "action")
		if err := addSales(t, repo, addCtx, cmd(order, 7, qty)); err != nil {
			t.Fatalf("第 %d 笔正向: %v", i, err)
		}
		if got := mem.SalesOf(7); got.Sold != baseline+qty {
			t.Fatalf("第 %d 笔正向后应为 sold=%d，实际 %d", i, baseline+qty, got.Sold)
		}

		cpCtx := dtmCtx(t, gid, "01", "compensate")
		if err := reduceSales(t, repo, cpCtx, cmd(order, 7, qty)); err != nil {
			t.Fatalf("第 %d 笔补偿: %v", i, err)
		}
		if got := mem.SalesOf(7); got.Sold != baseline {
			t.Fatalf("第 %d 笔补偿后必须精确回到基线 %d，实际 %d", i, baseline, got.Sold)
		}
	}
}

// TestReduceSalesNeverGoesNegative：**专属判据**。扣减量大于当前销量时必须报
// 业务失败，且 sold 一个字节都不许动。
//
// 反向探针：把 sqlReduceSales 里的 `and sold >= ?` 去掉，本用例必红 ——
// 夹具照样求值（它只认语法），sold 会被扣成负数。
func TestReduceSalesNeverGoesNegative(t *testing.T) {
	repo, mem := newRepo(t)
	mem.Seed(7, 2)
	// 先补一次正向，让 barrier 判得到「这一步确实到过」，否则测到的是空补偿。
	addCtx := dtmCtx(t, "gid-neg", "01", "action")
	if err := addSales(t, repo, addCtx, cmd("SO-4", 7, 2)); err != nil {
		t.Fatalf("前置正向: %v", err)
	}
	// 编排侧金额算错（或人工改库）时就是这个形态：要减 5，账上只有 4。
	ctx := dtmCtx(t, "gid-neg", "01", "compensate")
	err := reduceSales(t, repo, ctx, cmd("SO-4", 7, 5))
	if !errors.Is(err, ErrSalesImbalance) {
		t.Fatalf("扣成负数须判为 ErrSalesImbalance，实际 err=%v", err)
	}
	if !errors.Is(err, dtmcli.ErrFailure) {
		t.Fatalf("须带官方哨兵 dtmcli.ErrFailure（账已不平，重试一万次也不会平），实际 err=%v", err)
	}
	if got := mem.SalesOf(7); got.Sold != 4 {
		t.Fatalf("账不平时 sold 不许有任何改动，实际 %+v —— 负销量不会被任何告警发现", got)
	}
	// barrier 行必须随事务回滚：否则下次 dtm 重试会被判成「已落过账」而静默成功。
	if got := mem.Barriers(); got != 1 {
		t.Fatalf("业务失败后 barrier 行应随事务回滚（只剩前置正向那一行），实际 %d 行", got)
	}
}

// TestReduceSalesDistinguishesMissingRowFromImbalance 两种 0 行成因必须可分辨：
// 「压根没这条销量行」与「有行但账不平」。
//
// 混成一个码的代价：调用方会把「商品从来没有销量记录」当成「销量账被改坏了」
// 去查数据漂移，前者其实只需要补一条行。判据来自 sqlSelectSold 的诊断读。
func TestReduceSalesDistinguishesMissingRowFromImbalance(t *testing.T) {
	// 对照：行存在且够扣 —— 正常路径，两种失败都不该发生。
	t.Run("row exists and enough: neither", func(t *testing.T) {
		repo, mem := newRepo(t)
		mem.Seed(7, 10)
		a := dtmCtx(t, "gid-enough", "01", "action")
		if err := addSales(t, repo, a, cmd("SO-5", 7, 1)); err != nil {
			t.Fatalf("前置正向: %v", err)
		}
		c := dtmCtx(t, "gid-enough", "01", "compensate")
		if err := reduceSales(t, repo, c, cmd("SO-5", 7, 1)); err != nil {
			t.Fatalf("行存在且够扣不该失败：%v", err)
		}
		if got := mem.SalesOf(7); got.Sold != 10 {
			t.Fatalf("应回到 sold=10，实际 %d", got.Sold)
		}
	})

	// 账不平：有行但 sold 不够扣 —— 只准报 ErrSalesImbalance。
	t.Run("balance short: imbalance only", func(t *testing.T) {
		repo, mem := newRepo(t)
		mem.Seed(7, 1)
		a := dtmCtx(t, "gid-short", "01", "action")
		if err := addSales(t, repo, a, cmd("SO-7", 7, 1)); err != nil {
			t.Fatalf("前置正向: %v", err)
		}
		c := dtmCtx(t, "gid-short", "01", "compensate")
		err := reduceSales(t, repo, c, cmd("SO-7", 7, 3))
		if !errors.Is(err, ErrSalesImbalance) {
			t.Fatalf("行存在但不够扣须判 ErrSalesImbalance，实际 err=%v", err)
		}
		if errors.Is(err, ErrSalesNotFound) {
			t.Fatal("行存在时不得报成「记录不存在」—— 处置完全不同（补行 vs 查数据漂移）")
		}
	})
}

// TestReduceSalesOnMissingSalesRowIsNotFound 真的去减一个从未建行的商品时，
// 必须判 ErrSalesNotFound 而不是 ErrSalesImbalance。
func TestReduceSalesOnMissingSalesRowIsNotFound(t *testing.T) {
	repo, mem := newRepo(t)
	// 正向落在商品 7 上，补偿却指向商品 8：编排侧载荷串了号的形态。
	// barrier 只按 (gid, branch_id, op) 判空补偿，管不到 payload 里的 product_id。
	addCtx := dtmCtx(t, "gid-wrong-pid", "01", "action")
	if err := addSales(t, repo, addCtx, cmd("SO-8", 7, 1)); err != nil {
		t.Fatalf("前置正向: %v", err)
	}
	ctx := dtmCtx(t, "gid-wrong-pid", "01", "compensate")
	err := reduceSales(t, repo, ctx, cmd("SO-8", 8, 1))
	if !errors.Is(err, ErrSalesNotFound) {
		t.Fatalf("销量行不存在须判 ErrSalesNotFound，实际 err=%v", err)
	}
	if errors.Is(err, ErrSalesImbalance) {
		t.Fatal("记录不存在不得被报成账不平：两者处置完全不同（补行 vs 查数据漂移）")
	}
	if got := mem.SalesOf(8); got.Found {
		t.Fatalf("失败后不得凭空建行，实际 %+v", got)
	}
}

// TestNullCompensationWritesNothing：空补偿（补偿先于正向到达）不得动销量。
//
// 这条守卫是 barrier 存在的主要理由：少它，补偿会凭空把销量往上加，而正向
// 从来没记过 —— 于是销量虚高，且所有分支都「成功」。
func TestNullCompensationWritesNothing(t *testing.T) {
	repo, mem := newRepo(t)
	mem.Seed(7, 50)
	// 故意不跑前置正向：barrier 探不到同 branch_id 的 action 行，官方语义判空补偿。
	ctx := dtmCtx(t, "gid-null", "01", "compensate")
	if err := reduceSales(t, repo, ctx, cmd("SO-9", 7, 5)); err != nil {
		t.Fatalf("空补偿必须回成功：%v", err)
	}
	if got := mem.SalesOf(7); got.Sold != 50 {
		t.Fatalf("空补偿不得改销量，实际 %d", got.Sold)
	}
	if mem.Barriers() == 0 {
		t.Fatal("空补偿也要记 barrier（否则每次重试都重判一次空补偿）")
	}
}

// TestReduceSalesBarrierReplayIsIdempotent：补偿段重复回调只回退一次。
//
// dtm 会重试补偿段（尤其在基础失败之后），照做一次就是销量被多减一次。
func TestReduceSalesBarrierReplayIsIdempotent(t *testing.T) {
	repo, mem := newRepo(t)
	mem.Seed(7, 50)

	addCtx := dtmCtx(t, "gid-ref-replay", "01", "action")
	if err := addSales(t, repo, addCtx, cmd("SO-10", 7, 6)); err != nil {
		t.Fatalf("正向: %v", err)
	}
	for i := 0; i < 3; i++ {
		c := dtmCtx(t, "gid-ref-replay", "01", "compensate")
		if err := reduceSales(t, repo, c, cmd("SO-10", 7, 6)); err != nil {
			t.Fatalf("第 %d 次补偿: %v", i+1, err)
		}
	}
	if got := mem.SalesOf(7); got.Sold != 50 {
		t.Fatalf("补偿重放后应精确回到 sold=50，实际 %d —— 重复回退等于销量被凭空扣掉", got.Sold)
	}
}

// TestBranchWritesRequireBarrierTx：脱离 barrier 事务的写必须报错。
//
// 反向探针：把两个方法里的 `tx == nil` 判断去掉，本用例必红；而真机上它会
// 变成 nil 解引用 panic，且发生在 dtm 回调线程里（进程直接崩）。
func TestBranchWritesRequireBarrierTx(t *testing.T) {
	repo, _ := newRepo(t)
	ctx := t.Context()
	var nilTx *sql.Tx

	if err := repo.DtmAddSales(ctx, nilTx, cmd("SO-11", 7, 1)); err == nil {
		t.Fatal("AddSales 在 nil 事务上必须报错，不能脱离 barrier 落库")
	}
	if err := repo.DtmReduceSales(ctx, nilTx, cmd("SO-11", 7, 1)); err == nil {
		t.Fatal("ReduceSales 在 nil 事务上必须报错，不能脱离 barrier 落库")
	}
}

// ---- 列名不得虚构（守卫）----

// schemaColumns 逐字抄自 examples/micro/scripts/schema-micro.sql 的 product_db 段。
// 抄一份在这里而不是去读脚本文件：脚本是部署产物、未必在本仓检出；判据必须
// 自己站得住。
var schemaColumns = map[string][]string{
	"product_sales": {"product_id", "sold", "updated_at"},
	"barrier":       {"id", "trans_type", "gid", "branch_id", "op", "barrier_id", "reason", "create_time", "update_time"},
}

// sqlKeywords 求值时要放过的词（非列名）。
var sqlKeywords = map[string]bool{
	"select": true, "from": true, "where": true, "insert": true, "into": true,
	"values": true, "update": true, "set": true, "and": true,
	"on": true, "duplicate": true, "key": true,
}

var identRe = regexp.MustCompile(`[a-z_][a-z0-9_]*`)

// TestDtmStoreSQLUsesOnlySchemaColumns 是「不得虚构列」这条纪律的可执行判据。
//
// 为什么它必须存在：本文件的 SQL 是裸 SQL，编译器不管列名，写错一个列
// （sale / sales_amount）要等到真机第一次执行才炸 —— 而真机上那一刻可能正
// 处理一笔真实订单。更糟的是 MySQL 的宽松模式会把部分拼错静默当成 0，销量就
// 悄悄错了。夹具（dtmstub）只认本包自己抄的列，它能挡住「夹具没抄到的列」，
// 挡不住「两边抄错同一个列」。
//
// 反向探针：把 sqlReduceSales 里的 sold 改成 sales，本用例必红。
func TestDtmStoreSQLUsesOnlySchemaColumns(t *testing.T) {
	// 逐个点名：常量改名/删除时这里立刻红。只写「至少取到 N 条」是不够的 ——
	// 新常量忘了登记会静默漏检。
	want := []string{"sqlAddSales", "sqlReduceSales", "sqlSelectSold"}
	stmts := extractSQLConsts(t, "dtmstore.go")
	for _, name := range want {
		if _, ok := stmts[name]; !ok {
			t.Fatalf("没从 dtmstore.go 取到 SQL 常量 %s —— 守卫失效（常量被改名或删除？）", name)
		}
	}
	for name, q := range stmts {
		table := sqlTable(q)
		if table == "" {
			t.Fatalf("%s：认不出作用表，SQL=%q", name, q)
		}
		cols, ok := schemaColumns[table]
		if !ok {
			t.Fatalf("%s：SQL 指向本域不存在的表 %q", name, table)
		}
		allowed := map[string]bool{table: true}
		for _, c := range cols {
			allowed[c] = true
		}
		for _, id := range identRe.FindAllString(strings.ToLower(q), -1) {
			if sqlKeywords[id] || allowed[id] {
				continue
			}
			t.Errorf("%s：列/表 %q 不在 schema-micro.sql 的 %s 里 —— 虚构列，真机上才会炸", name, id, table)
		}
	}
}

// extractSQLConsts 从本包源码里取出所有 SQL 常量字面量。
//
// 走 AST 而不是手抄一份清单：手抄的清单会与实际发出的 SQL 漂移（改了语句
// 忘了改清单，守卫就只验着旧的那几条），而漂移的方向恰好是守卫失效的方向。
func extractSQLConsts(t *testing.T, file string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s 失败：%v", file, err)
	}
	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, v := range vs.Values {
			val, ok := literalString(v)
			if !ok {
				continue
			}
			low := strings.ToLower(val)
			if !strings.HasPrefix(low, "select ") && !strings.HasPrefix(low, "insert ") &&
				!strings.HasPrefix(low, "update ") {
				continue
			}
			out[vs.Names[i].Name] = val
		}
		return true
	})
	return out
}

// literalString 取常量表达式里的字符串值（支持 `a` + `b` 拼接）。
func literalString(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		l, ok1 := literalString(e.X)
		r, ok2 := literalString(e.Y)
		return l + r, ok1 && ok2
	case *ast.ParenExpr:
		return literalString(e.X)
	default:
		return "", false
	}
}

// sqlTable 从语句里取作用表（insert 的表在 into 之后，update 的在动词之后）。
func sqlTable(q string) string {
	q = strings.ToLower(q)
	switch {
	case strings.HasPrefix(q, "insert ignore into "):
		return firstToken(q[len("insert ignore into "):])
	case strings.HasPrefix(q, "insert into "):
		return firstToken(q[len("insert into "):])
	case strings.HasPrefix(q, "update "):
		return firstToken(q[len("update "):])
	case strings.HasPrefix(q, "select "):
		i := strings.Index(q, " from ")
		if i < 0 {
			return ""
		}
		return firstToken(q[i+len(" from "):])
	}
	return ""
}

func firstToken(s string) string {
	stop := strings.IndexAny(s, " (")
	if stop < 0 {
		stop = len(s)
	}
	return strings.TrimSpace(s[:stop])
}
