package service

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"
	"gorm.io/gorm"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/dtmstub"
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/model"
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/repository"
)

// TestMain 把 dtm 的 barrier 表名设成本域的 "barrier"。
//
// 必须设：官方默认是跨库的 dtm_barrier.barrier，而本域的 barrier 表落在业务库内
// （examples/micro/scripts/schema-micro.sql）。app/setup.go 里的分支面装配做的是
// 同一件事 —— 测试不跟着设，测的就不是真机上的那条路径，而「barrier 行有没有插在
// 业务库」正是这里最要紧的断言。
func TestMain(m *testing.M) {
	dtmx.SetBarrierTableName("barrier")
	os.Exit(m.Run())
}

// dtm 分支业务方法的单测（纯 Go 假驱动 dtmstub + 内存真值库）。
//
// 两个方法都必须同时守住三类：
//  1. 正常写成功 —— 销量真的动了，且 barrier 行与业务写同事务提交；
//  2. 业务失败   —— 账不平 / 记录不存在 → 带官方哨兵 + 2xxx 业务码，
//     dtm 读到哨兵立刻转补偿，不做退避重试；
//  3. barrier 幂等 —— dtm 重试或空补偿时，业务写一次都不能多。
//
// 第 3 条是本文件最要紧的。反向探针：把 dtmBranch 里的 barrier.Call 换成
// 直接 db.BeginTx + write + Commit，本文件的 TestDtmBranchReplayIsIdempotent
// 必红 —— 实测 AddSales 重放把销量推到 112、ReduceSales 重放压到 91。
//
// **branch_id 的取法照抄 dtm server**（dtmsvr/trans_type_saga.go 的 GenBranches）：
// 一个 saga 步骤的 action 与 compensate 共用同一个 branch_id，且同 gid 下同
// branch_id 只会有一次 action。barrier 判空补偿正是靠这个共用 id 去探「同一步的
// 正向行在不在」，所以补偿用例必须先跑正向，否则测到的是空补偿而不是补偿。

// dtmMethod 一个分支方法的用例骨架。
type dtmMethod struct {
	// name 与 model.DtmXxxBranch 一致：编排侧按这个名字点火失败注入，
	// 也是 busi.proto 里的方法名（两者恒等，见 dtm 包的 handler）
	name string
	// op dtm 带的操作名
	op string
	// prereq 前置正向分支（补偿分支才需要；nil 表示自身即正向）
	prereq *dtmMethod
	// call 调 service 的分支方法
	call func(s *ProductService, ctx context.Context, cmd model.DtmSalesCmd) error
}

func addSalesMethod() *dtmMethod {
	return &dtmMethod{
		name: model.DtmAddSalesBranch, op: "action",
		call: func(s *ProductService, ctx context.Context, c model.DtmSalesCmd) error { return s.DtmAddSales(ctx, c) },
	}
}

func reduceSalesMethod() *dtmMethod {
	fwd := addSalesMethod()
	return &dtmMethod{
		name: model.DtmReduceSalesBranch, op: "compensate", prereq: fwd,
		call: func(s *ProductService, ctx context.Context, c model.DtmSalesCmd) error {
			return s.DtmReduceSales(ctx, c)
		},
	}
}

// allMethods 正向 + 补偿两个分支。
func allMethods() []*dtmMethod {
	return []*dtmMethod{addSalesMethod(), reduceSalesMethod()}
}

// newDtmSvc 造一条只挂 dbx 句柄的 service（cache/kv/lock 全 nil：分支面不需要，
// 详情缓存与限流是 RPC 面才走的路）。
func newDtmSvc(t *testing.T, db *dtmstub.DB) *ProductService {
	t.Helper()
	gdb, err := db.GormDB()
	if err != nil {
		t.Fatalf("开夹具库失败：%v", err)
	}
	return NewDtmSvcForTest(NewProductService(repository.NewProductRepository(gdb, false), nil, nil, nil, false, nil))
}

// NewDtmSvcForTest 显式声明这个 service 是给分支面测试用的：nil cache/kv/lock 在
// 分支面上是合法降级（分支面不读缓存、不走限流、不需要上下架互斥），但在 RPC 面
// 上是装配错误 —— 用一个命名函数把这个区别钉住，免得后来者以为可以随便传 nil。
func NewDtmSvcForTest(s *ProductService) *ProductService { return s }

func dtmCmd() model.DtmSalesCmd {
	return model.DtmSalesCmd{OrderNo: "SO-DTM-1", ProductID: 42, Quantity: 3}
}

// dtmCtx 造一个带 dtm 分支 metadata 的 ctx。
//
// key 前缀 dtm- 是官方约定（dtmgrpc/dtmgimp：const dtmpre = "dtm-"），四个字段缺
// 一个 barrier 就构造不出来 —— 这正是「取不到 barrier 必须报错」那条纪律的由来。
func dtmCtx(gid, branchID, op string) context.Context {
	md := metadata.New(map[string]string{
		"dtm-gid":        gid,
		"dtm-trans_type": "saga",
		"dtm-branch_id":  branchID,
		"dtm-op":         op,
	})
	return metadata.NewIncomingContext(context.Background(), md)
}

// arrive 让一个分支到达（补偿分支先跑它的正向），返回假库句柄。
func arrive(t *testing.T, db *dtmstub.DB, m *dtmMethod, gid string) *ProductService {
	t.Helper()
	svc := newDtmSvc(t, db)
	if m.prereq != nil {
		if err := m.prereq.call(svc, dtmCtx(gid, "01", m.prereq.op), dtmCmd()); err != nil {
			t.Fatalf("前置正向分支 %s 应成功：%v", m.prereq.name, err)
		}
	}
	return svc
}

// TestDtmBranchWritesInsideBarrier 正常路径：销量真的动了，且带 barrier 行。
func TestDtmBranchWritesInsideBarrier(t *testing.T) {
	const gid = "gid-1"
	for _, m := range allMethods() {
		t.Run(m.name, func(t *testing.T) {
			mem := dtmstub.New()
			mem.Seed(42, 100)
			svc := arrive(t, mem, m, gid)
			// arrive 之后：正向分支自己还没跑（before=100）；补偿分支的前置正向
			// 已经到达（before=103）。两者都必须是「账上确有 100 这个基线」。
			before := mem.SalesOf(42).Sold
			if before != 100 && before != 103 {
				t.Fatalf("前置状态异常，销量 = %d", before)
			}

			if err := m.call(svc, dtmCtx(gid, "01", m.op), dtmCmd()); err != nil {
				t.Fatalf("分支写应成功：%v", err)
			}
			if mem.Barriers() == 0 {
				t.Fatal("barrier 行必须由 dtmx.Barrier.Call 插（与业务写同事务）")
			}
			// 正向分支把销量推到 103；补偿分支把 103 精确拉回基线 100。
			want := int64(103)
			if m.op == "compensate" {
				want = 100
			}
			if got := mem.SalesOf(42); got.Sold != want {
				t.Fatalf("%s 之后销量应为 %d，实际 %d —— 业务写没发生或方向反了", m.name, want, got.Sold)
			}
		})
	}
}

// TestDtmCompensationRestoresSalesToBaseline：**专属判据**。service 层跑完
// 「正向 +n → 补偿 -n」之后，销量必须精确回到基线。
//
// 为什么不把它只放在 repository 层：repository 的两条断言验的是 SQL 与守卫，
// 这一条验的是**service 的编排没有把两个方向搞混** —— 比如补偿时把 Quantity
// 取反又传进 DecreaseSales，那样的 SQL 在单测里仍然「sold -= -3」跑得通，
// 只有端到端的值才抓得住。
//
// 反向探针：把 DtmReduceSales 里的 write 从 s.repo.DtmReduceSales 换成
// s.repo.DtmAddSales（本域最容易犯的方向错），本用例必红 —— 补偿会把销量
// 再加一遍。
func TestDtmCompensationRestoresSalesToBaseline(t *testing.T) {
	const baseline = 100
	for i, qty := range []int64{2, 11} {
		t.Run("qty="+strconv.FormatInt(qty, 10), func(t *testing.T) {
			mem := dtmstub.New()
			mem.Seed(42, baseline)
			gdb, err := mem.GormDB()
			if err != nil {
				t.Fatalf("开夹具库失败：%v", err)
			}
			svc := NewDtmSvcForTest(NewProductService(repository.NewProductRepository(gdb, false), nil, nil, nil, false, nil))
			gid := "gid-baseline-" + strconv.Itoa(i)
			c := model.DtmSalesCmd{OrderNo: "SO-B", ProductID: 42, Quantity: qty}

			if err := svc.DtmAddSales(dtmCtx(gid, "01", "action"), c); err != nil {
				t.Fatalf("正向: %v", err)
			}
			if got := mem.SalesOf(42); got.Sold != baseline+qty {
				t.Fatalf("正向��� sold=%d，want %d", got.Sold, baseline+qty)
			}
			if err := svc.DtmReduceSales(dtmCtx(gid, "01", "compensate"), c); err != nil {
				t.Fatalf("补偿: %v", err)
			}
			if got := mem.SalesOf(42); got.Sold != baseline {
				t.Fatalf("补偿后必须精确回到基线 %d，实际 %d —— 差 1 分就是首页销量与订单数永久对不上", baseline, got.Sold)
			}
		})
	}
}

// TestDtmBusinessFailuresCarryCodeAndOfficialSentinel 业务失败同时带 2xxx 业务码
// 与 dtm 官方哨兵：前者给人和编排侧读，后者决定 dtm 会不会重试。少任何一个的
// 代价都写在各自注释里。
func TestDtmBusinessFailuresCarryCodeAndOfficialSentinel(t *testing.T) {
	cases := []struct {
		name string
		// seed 造出前置的库存形态
		seed int64
		// addQty 前置正向加多少
		addQty int64
		// compQty 补偿要减多少（> 账面即账不平）
		compQty int64
		// target 补偿指向的商品（42 = 有行；43 = 从未建行）
		target uint64
		// wantCode 期望的商品域业务码
		wantCode int64
		// wantMsg 文案里应出现的成因关键词（用于人工核对分类没有串）
		wantMsg string
	}{
		{"imbalance", 1, 1, 5, 42, codes.CodeProductWriteFailed, "imbalance"},
		{"sales row missing", 100, 3, 3, 43, codes.CodeProductNotFound, "not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mem := dtmstub.New()
			mem.Seed(42, c.seed)
			gdb, err := mem.GormDB()
			if err != nil {
				t.Fatalf("开夹具库失败：%v", err)
			}
			svc := NewDtmSvcForTest(NewProductService(repository.NewProductRepository(gdb, false), nil, nil, nil, false, nil))
			gid := "gid-bizfail-" + c.name
			// 前置正向必须成功，否则 barrier 判成空补偿，压根走不到写失败那一层。
			if err := svc.DtmAddSales(dtmCtx(gid, "01", "action"), model.DtmSalesCmd{
				OrderNo: "SO-BF", ProductID: 42, Quantity: c.addQty,
			}); err != nil {
				t.Fatalf("前置正向: %v", err)
			}
			// 两种形态都是「重试一万次也不会好」：
			//   imbalance —— 要减的比账面多（编排侧金额算错 / 有人手改过 sold）
			//   missing    —— 补偿指向一个从未建过销量行的商品（载荷串了 product_id）
			err = svc.DtmReduceSales(dtmCtx(gid, "01", "compensate"), model.DtmSalesCmd{
				OrderNo: "SO-BF", ProductID: c.target, Quantity: c.compQty,
			})
			if err == nil {
				t.Fatal("账已不平 / 行不存在必须报错，不能返回成功")
			}
			if !dtmx.IsBusinessFailure(err) {
				t.Fatalf("必须是业务失败（dtm 立刻转补偿），实际 %v", err)
			}
			be, ok := codes.As(err)
			if !ok {
				t.Fatalf("业务失败必须带业务码，实际 %v", err)
			}
			if be.Code != c.wantCode {
				t.Fatalf("业务码 = %d，want %d", be.Code, c.wantCode)
			}
			if !strings.Contains(strings.ToLower(be.Message), c.wantMsg) {
				t.Fatalf("文案应点明成因 %q，实际 %q", c.wantMsg, be.Message)
			}
		})
	}
}

// TestDtmInfraFailureIsNotBusinessFailure 基础设施故障不得被标成业务失败。
//
// 标了 dtm 就不重试，一次 DB 抖动会把整条 Saga 判死并触发回补 —— 而那笔账本来
// 并没有任何问题。这里用「句柄取不到 *sql.DB」模拟基础设施不可用（真实部署里
// 的对应物是 dbx 句柄未装配 / 换驱动后 ConnPool 不是 *sql.DB）。
//
// 反向探针：把 dtmBranch 里 RawDB 失败那一行的
// `return fmt.Errorf("dtm 分支取不到业务库连接: %w", err)` 改成
// `return dtmx.BusinessFailure(...)`，本用例必红（实测两个分支都红）。
// 注意**删掉 dtmFail 里的 `!IsBusinessFailure` 早返回并不会让本用例红** ——
// 句柄取不到这条路径压根走不到 dtmFail；判据盯的是「句柄故障会不会被标成业务
// 失败」这件事本身，入口在哪一层不影响。
func TestDtmInfraFailureIsNotBusinessFailure(t *testing.T) {
	svc := NewDtmSvcForTest(NewProductService(
		repository.NewProductRepository(&gorm.DB{Config: &gorm.Config{}}, false), nil, nil, nil, false, nil))

	for _, m := range allMethods() {
		t.Run(m.name, func(t *testing.T) {
			err := m.call(svc, dtmCtx("gid-infra", "01", m.op), dtmCmd())
			if err == nil {
				t.Fatal("句柄不可用时必须报错")
			}
			if dtmx.IsBusinessFailure(err) {
				t.Fatalf("基础设施故障不得标成业务失败（否则 dtm 不重试、整条 Saga 直接判死）：%v", err)
			}
			if be, ok := codes.As(err); ok &&
				(be.Code == codes.CodeProductWriteFailed || be.Code == codes.CodeProductNotFound) {
				t.Fatalf("句柄故障不应被翻译成商品域业务码 %d", be.Code)
			}
		})
	}
}

// TestDtmMethodsRejectRequestsWithoutBarrierMetadata 没有 dtm metadata 时必须
// 报错，且一条业务写都不许发生。
//
// 「忽略它继续往下写」是这条路径最贵的写法：没有 barrier 就没有空补偿 / 重复
// 回调 / 悬挂三条守卫，重复回调会重复记销量、补偿先到会凭空加销量，而函数返回
// 成功、日志干净 —— 事后无从追责。
func TestDtmMethodsRejectRequestsWithoutBarrierMetadata(t *testing.T) {
	for _, m := range allMethods() {
		t.Run(m.name, func(t *testing.T) {
			mem := dtmstub.New()
			mem.Seed(42, 100)
			svc := newDtmSvc(t, mem)

			err := m.call(svc, context.Background(), dtmCmd())
			if err == nil {
				t.Fatal("没有 dtm metadata 时必须报错，不能无屏障落库")
			}
			if !strings.Contains(err.Error(), "barrier") {
				t.Fatalf("错误文案须点明 barrier 便于 dtm 侧排障，实际：%v", err)
			}
			if got := mem.SalesOf(42); got.Sold != 100 {
				t.Fatalf("取不到 barrier 时销量不许变动，实际 %d", got.Sold)
			}
		})
	}
}

// TestDtmBranchReplayIsIdempotent 幂等：dtm 重复回调同一分支时，第二次不得产生
// 任何业务写（barrier 的 insert ignore 影响 0 行 → 官方语义直接跳过业务回调）。
func TestDtmBranchReplayIsIdempotent(t *testing.T) {
	for _, m := range allMethods() {
		t.Run(m.name, func(t *testing.T) {
			const gid = "gid-replay"
			mem := dtmstub.New()
			mem.Seed(42, 100)
			svc := arrive(t, mem, m, gid)

			if err := m.call(svc, dtmCtx(gid, "01", m.op), dtmCmd()); err != nil {
				t.Fatalf("首次到达应成功：%v", err)
			}
			first := mem.SalesOf(42).Sold

			for i := 0; i < 3; i++ {
				if err := m.call(svc, dtmCtx(gid, "01", m.op), dtmCmd()); err != nil {
					t.Fatalf("barrier 判重必须回成功（返回 error 会让 dtm 无限重试）：%v", err)
				}
			}
			if got := mem.SalesOf(42).Sold; got != first {
				t.Fatalf("重放后销量应仍为 %d，实际 %d —— barrier 幂等失效，同一笔被重复记账", first, got)
			}
		})
	}
}

// TestDtmMethodsRejectBadParams 参数非法同样是业务失败（重试一万次也不会变好），
// 且必须排在 barrier 之前判 —— 否则每次重试都会留下一行 barrier，把一笔永远
// 不会成功的分支判成「已落过账」。
func TestDtmMethodsRejectBadParams(t *testing.T) {
	cases := []struct {
		name string
		cmd  model.DtmSalesCmd
	}{
		{"no product", model.DtmSalesCmd{OrderNo: "SO-1", Quantity: 1}},
		{"zero qty", model.DtmSalesCmd{OrderNo: "SO-1", ProductID: 1}},
		{"negative qty", model.DtmSalesCmd{OrderNo: "SO-1", ProductID: 1, Quantity: -1}},
		{"no order_no", model.DtmSalesCmd{ProductID: 1, Quantity: 1}},
	}
	for _, m := range allMethods() {
		for _, c := range cases {
			t.Run(m.name+"/"+c.name, func(t *testing.T) {
				mem := dtmstub.New()
				mem.Seed(42, 100)
				svc := newDtmSvc(t, mem)

				err := m.call(svc, dtmCtx("gid-bad", "01", m.op), c.cmd)
				if err == nil {
					t.Fatal("参数非法必须报错")
				}
				if !dtmx.IsBusinessFailure(err) {
					t.Fatalf("参数非法是业务失败（不重试），实际 %v", err)
				}
				be, ok := codes.As(err)
				if !ok || be.Code != codes.CodeInvalidParam {
					t.Fatalf("业务码应是 1001，实际 %v", err)
				}
				if mem.Barriers() != 0 {
					t.Fatalf("参数校验必须在开事务之前，实际留下 %d 行 barrier", mem.Barriers())
				}
				if got := mem.SalesOf(42); got.Sold != 100 {
					t.Fatalf("参数非法时销量不许变动，实际 %d", got.Sold)
				}
			})
		}
	}
}

// TestDtmNegativeQuantityCannotInflateSales **专属判据的入口守卫**：负数量若被
// 放行，ReduceSales 的 `sold >= -1` 守卫恒成立，「减 -1」会变成「加 1」——
// 一次补偿凭空给销量加 1，且所有分支都报成功。
//
// 反向探针：把 validateDtmCmd 里的 `cmd.Quantity <= 0` 那一档整个删掉，
// 本用例必红 —— 实测销量会从 105 变成 110（补偿一次凭空加了 5）；
// TestDtmMethodsRejectBadParams/zero_qty 与 /negative_qty 会跟着一起红。
// 注意只把 `<=` 放宽成 `<` **不会**让本用例红（负数照样被拒），所以这条
// 判据必须盯住"整档删掉"这个方向。
func TestDtmNegativeQuantityCannotInflateSales(t *testing.T) {
	mem := dtmstub.New()
	mem.Seed(42, 100)
	gdb, err := mem.GormDB()
	if err != nil {
		t.Fatalf("开夹具库失败：%v", err)
	}
	svc := NewDtmSvcForTest(NewProductService(repository.NewProductRepository(gdb, false), nil, nil, nil, false, nil))
	gid := "gid-negqty"

	if err := svc.DtmAddSales(dtmCtx(gid, "01", "action"), model.DtmSalesCmd{
		OrderNo: "SO-N", ProductID: 42, Quantity: 5,
	}); err != nil {
		t.Fatalf("前置正向: %v", err)
	}
	err = svc.DtmReduceSales(dtmCtx(gid, "01", "compensate"), model.DtmSalesCmd{
		OrderNo: "SO-N", ProductID: 42, Quantity: -5,
	})
	if err == nil {
		t.Fatalf("负数量必须被拒，实际返回成功且销量变成 %d", mem.SalesOf(42).Sold)
	}
	if !errors.Is(err, repository.ErrSalesImbalance) && !dtmx.IsBusinessFailure(err) {
		t.Fatalf("负数量必须判业务失败，实际 %v", err)
	}
	if got := mem.SalesOf(42); got.Sold != 105 {
		t.Fatalf("负数量被放行后销量应仍为 105，实际 %d —— 补偿一次凭空加了销量", got.Sold)
	}
}
