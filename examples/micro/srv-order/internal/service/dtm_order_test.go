// dtm 分支业务方法的单测（纯 Go 假驱动，无 cgo、无真库）。
//
// 每个分支方法都必须同时守住三类：
//  1. 正常写成功 —— 账真的动了，且 barrier 行与业务写同事务提交；
//  2. 业务失败 → 带 codes 码 + dtm 官方哨兵 —— dtm 读到哨兵就不重试、立即转补偿；
//  3. barrier 幂等 —— 同一分支重复到达时，业务写一次都不能多。
//
// 第 3 条是本文件最要紧的。反向探针：把 dtmBranch 里的 barrier.Call 换成直接调
// write(ctx, tx, cmd)，TestBranchReplayIsIdempotent 必红 —— 没有 barrier 行，
// 重放会真的再建一张单、再写一条事件。
//
// **branch_id 的取法照抄 dtm server**（dtmsvr/trans_type_saga.go 的 GenBranches）：
// 一个 saga 步骤的 action 与 compensate 共用同一个 branch_id（"01"/"02"/…），
// TCC 的 try/confirm/cancel 同理（dtmsvr/api.go 的 svcRegisterBranch 复制同一个
// branch）。barrier 判空补偿正是靠这个共用 id 去探「同一步的正向行在不在」，
// 所以逆序用例必须先跑正向，否则测到的会是空补偿而不是补偿。
package service

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	"gorm.io/gorm"

	"github.com/gospacex/cachex"
	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/dbmock"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/model"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/repository"
)

// TestMain 先把 dtm 的 barrier 表名设成本部署用的那个。
//
// 必须设：官方默认是跨库的 dtm_barrier.barrier，而本域的 barrier 表落在业务库内
// （examples/micro/scripts/schema-micro.sql 的 order_db 段）。app/setup.go 的
// dtm.Setup 做的是同一件事 —— 测试不跟着设，测的就不是真机上的那条路径，而
// 「barrier 行有没有插在业务库」正是这里最要紧的断言。
//
// 不设的后果很隐蔽：正向分支照样跑通（barrier 行插不进去也不影响它执行业务写），
// 而补偿/回退分支会被官方语义判成「空补偿」直接返回 —— 正向账建了、补偿账没回，
// 两边都报成功。
func TestMain(m *testing.M) {
	dtmx.SetBarrierTableName("barrier")
	os.Exit(m.Run())
}

// sagaBranchID / tccBranchID：一个 saga 步骤 / 一对 tcc 分支在 dtm server 里的
// branch_id。正向与逆向共用同一个，是 barrier 判空补偿的前提。
const (
	sagaBranchID = "01"
	tccBranchID  = "01"
)

// dtmMethod 一个分支方法的用例骨架。
type dtmMethod struct {
	// name 与 model.DtmXxxBranch 一致：编排侧按这个名字点火失败注入，
	// 也是 busi.proto 里的方法名（两者恒等，见 dtm 包的 handler）
	name string
	// branchID 该分支在 dtm server 里的 branch_id
	branchID string
	// op dtm 带的操作名
	op string
	// prereq 前置正向分支（逆序分支才需要；nil 表示自身即正向）
	prereq *dtmMethod
	// call 调 service 的分支方法
	call func(s *OrderService, ctx context.Context, cmd model.DtmOrderCmd) error
	// prereqCmd 前置分支用的载荷（默认与本次相同）
	prereqCmd *model.DtmOrderCmd
}

// forwardMethods 正向分支（自带 barrier 语义，可单独到达）。
//
// ConfirmFreeze 的 prereq 是 TryPlace：它把**占位单原地转成正式单**，而占位单
// 只可能由 TryPlace 建出来。与库存域的 Confirm 不同（那边 stocks 行早就在，
// Confirm 只是改列），订单域这里没这一步就是「定位不到单」。
func forwardMethods() []*dtmMethod {
	tryPlace := &dtmMethod{name: model.DtmTryPlaceBranch, branchID: tccBranchID, op: "try",
		call: func(s *OrderService, ctx context.Context, c model.DtmOrderCmd) error {
			return s.DtmTryPlace(ctx, c)
		}}
	return []*dtmMethod{
		{name: model.DtmCreateOrderBranch, branchID: sagaBranchID, op: "action",
			call: func(s *OrderService, ctx context.Context, c model.DtmOrderCmd) error {
				return s.DtmCreateOrder(ctx, c)
			}},
		tryPlace,
		{name: model.DtmConfirmFreezeBranch, branchID: tccBranchID, op: "confirm", prereq: tryPlace,
			call: func(s *OrderService, ctx context.Context, c model.DtmOrderCmd) error {
				return s.DtmConfirmFreeze(ctx, c)
			}},
	}
}

// inverseMethods 逆序分支：Saga 补偿与 TCC 的回退。
//
// 它们由 dtm 异步发起，载荷里没有 traceparent，且**必须**有同 branch_id 的正向
// 行先落地，否则被 barrier 判成空补偿（那另有专门用例）。
func inverseMethods() []*dtmMethod {
	fwd := forwardMethods()
	return []*dtmMethod{
		{name: model.DtmCancelOrderBranch, branchID: sagaBranchID, op: "compensate",
			prereq: fwd[0],
			call: func(s *OrderService, ctx context.Context, c model.DtmOrderCmd) error {
				return s.DtmCancelOrder(ctx, c)
			}},
		{name: model.DtmCancelFreezeBranch, branchID: tccBranchID, op: "cancel",
			prereq: fwd[1],
			call: func(s *OrderService, ctx context.Context, c model.DtmOrderCmd) error {
				return s.DtmCancelFreeze(ctx, c)
			}},
	}
}

func allMethods() []*dtmMethod { return append(forwardMethods(), inverseMethods()...) }

// fakeCache 记录 Delete 的缓存替身（分支面唯一的缓存动作就是失效详情缓存）。
type fakeCache struct{ deleted []string }

func (c *fakeCache) Get(context.Context, string) (string, error)              { return "", nil }
func (c *fakeCache) Set(context.Context, string, string, time.Duration) error { return nil }
func (c *fakeCache) Delete(_ context.Context, key string) error {
	c.deleted = append(c.deleted, key)
	return nil
}
func (c *fakeCache) Exists(context.Context, string) (bool, error)   { return false, nil }
func (c *fakeCache) Keys(context.Context, string) ([]string, error) { return nil, nil }
func (c *fakeCache) Close() error                                   { return nil }
func (c *fakeCache) HealthCheck(context.Context) error              { return nil }

// newDtmSvc 造一条只挂 dbx 句柄的 service（cache/kv/lock 全 nil：分支面不需要，
// 下单互斥那套是 HTTP 面才走的路；故 cache 不给，失效路径由单独的用例覆盖）。
func newDtmSvc(t *testing.T, db *dbmock.DB) *OrderService {
	t.Helper()
	gdb := &gorm.DB{Config: &gorm.Config{ConnPool: db.SQL()}}
	return NewOrderService(repository.NewOrderRepository(gdb, false), nil, nil, nil, false, nil)
}

func newDtmSvcWithCache(db *dbmock.DB, c cachex.Cache) *OrderService {
	gdb := &gorm.DB{Config: &gorm.Config{ConnPool: db.SQL()}}
	return NewOrderService(repository.NewOrderRepository(gdb, false), c, nil, nil, false, nil)
}

func dtmCmd() model.DtmOrderCmd {
	return model.DtmOrderCmd{
		UserID:  7,
		OrderNo: "O-DTM-1",
		DtmGID:  "gid-1",
		Items: []model.OrderItemCmd{
			{ProductID: 11, Name: "sku-11", Quantity: 2, UnitPrice: 150},
		},
		Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}
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

// arrive 让一个分支到达（逆序分支先跑它的正向），并把执行记录归零，
// 使断言只看见逆序那一次。
func arrive(t *testing.T, db *dbmock.DB, m *dtmMethod, gid string) *OrderService {
	t.Helper()
	svc := newDtmSvc(t, db)
	if m.prereq == nil {
		return svc
	}
	cmd := dtmCmd()
	if m.prereqCmd != nil {
		cmd = *m.prereqCmd
	}
	if err := m.prereq.call(svc, dtmCtx(gid, m.prereq.branchID, m.prereq.op), cmd); err != nil {
		t.Fatalf("前置正向分支 %s 应成功：%v", m.prereq.name, err)
	}
	db.ResetLog()
	return svc
}

// TestDtmBranchWritesInsideBarrier 正常路径：barrier 行与业务写在同一事务里提交。
func TestDtmBranchWritesInsideBarrier(t *testing.T) {
	for _, m := range allMethods() {
		t.Run(m.name, func(t *testing.T) {
			const gid = "gid-1"
			db := dbmock.New(t)
			svc := arrive(t, db, m, gid)

			if err := m.call(svc, dtmCtx(gid, m.branchID, m.op), dtmCmd()); err != nil {
				t.Fatalf("分支写应成功：%v", err)
			}
			if db.BarrierCount() == 0 {
				t.Fatalf("barrier 行必须由 dtmx.Barrier.Call 插入（屏障行不在同一事务就没有幂等）：%v",
					db.Stmts())
			}
		})
	}
}

// TestBranchReplayIsIdempotent barrier 幂等：同一 (gid, branch_id, op) 第二次到达
// → barrier 判重 → 业务回调根本不被调用，账一次都不能多动。
//
// 反向探针：把 dtmBranch 里的 barrier.Call 换成直接调 write(ctx, tx, cmd)，
// 本用例必红（订单变 2 张 / 事件变 2 条）。
func TestBranchReplayIsIdempotent(t *testing.T) {
	for _, m := range forwardMethods() {
		t.Run(m.name, func(t *testing.T) {
			const gid = "gid-replay"
			db := dbmock.New(t)
			svc := arrive(t, db, m, gid)
			ctx := dtmCtx(gid, m.branchID, m.op)

			if err := m.call(svc, ctx, dtmCmd()); err != nil {
				t.Fatalf("首次到达应成功：%v", err)
			}
			// 必须真的经过 barrier：建单类分支光靠 uk_order_no 也扛得住重放
			//（第二次 insert ignore 命中唯一键），所以「账没多动」这一条**单独
			// 证明不了** barrier 在场。把「插过 barrier 行」也钉上，绕过
			// dtmx.Barrier.Call 时五个分支一起红。
			if db.BarrierCount() == 0 {
				t.Fatalf("分支写必须经 barrier（没有屏障行，空补偿/重复回调/悬挂三条守卫全失效）：%v", join(db))
			}
			before := stateOf(db)
			if err := m.call(svc, ctx, dtmCmd()); err != nil {
				t.Fatalf("重放必须报成功（dtm 认为已处理）：%v", err)
			}
			if got := stateOf(db); got != before {
				t.Fatalf("重放不得再动账：%s → %s\n执行序列：\n  %s", before, got, join(db))
			}
		})
	}
}

// stateOf 把库里四张关键表的行数拼成一个可比串（重放前后必须逐字相等）。
func stateOf(db *dbmock.DB) string {
	return strings.Join([]string{
		"orders=" + strconv.Itoa(len(db.Orders())),
		"items=" + strconv.Itoa(len(db.Items())),
		"outbox=" + strconv.Itoa(len(db.Outboxes())),
		"logs=" + strconv.Itoa(len(db.StatusLogs())),
	}, " ")
}

func join(db *dbmock.DB) string { return strings.Join(db.Stmts(), "\n  ") }

// TestNullCompensationSkipsBusinessWrite 空补偿：补偿分支到达时同 branch_id 的
// 正向行还不存在 → dtm 官方语义判空补偿 → **业务回调不被调用**，只记 barrier 行。
//
// 这条守卫是 barrier 存在的全部理由：没有它，一次「正向还没跑到就被判死」的
// 补偿会把订单回滚成「不存在」，而正向随后到达时照样建单 —— 两边都报成功。
func TestNullCompensationSkipsBusinessWrite(t *testing.T) {
	cases := []struct {
		m    *dtmMethod
		desc string
	}{
		{m: inverseMethods()[0], desc: model.DtmCancelOrderBranch},
		{m: inverseMethods()[1], desc: model.DtmCancelFreezeBranch},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			const gid = "gid-null"
			db := dbmock.New(t)
			svc := newDtmSvc(t, db)

			// 不跑正向，直接发补偿。
			if err := tc.m.call(svc, dtmCtx(gid, tc.m.branchID, tc.m.op), dtmCmd()); err != nil {
				t.Fatalf("空补偿应报成功（官方语义：没正向就没有要回的东西）：%v", err)
			}
			if state := stateOf(db); state != "orders=0 items=0 outbox=0 logs=0" {
				t.Fatalf("空补偿不得写任何业务数据，实得 %s\n执行序列：\n  %s", state, join(db))
			}
			if db.BarrierCount() == 0 {
				t.Fatal("空补偿也要记 barrier 行（否则它会被判空补偿无数次，而正向到达后无从察觉")
			}
		})
	}
}

// TestDtmBranchDeficitIsBusinessFailure 定位不到单 / 状态不允许时必须报业务失败，
// 且同时带 codes 码与 dtm 官方哨兵。
//
// 少了哨兵：dtm 把「订单状态不对」当基础设施故障，按退避重试一整天，把一次
// 正常拒单放大成重试风暴。少了码：日志里只有一个英文错误，排障要回表反推。
func TestDtmBranchDeficitIsBusinessFailure(t *testing.T) {
	cases := []struct {
		name string
		run  func(s *OrderService, ctx context.Context) error
		want int64
	}{
		{
			name: "确认不到占位单",
			run: func(s *OrderService, ctx context.Context) error {
				return s.DtmConfirmFreeze(ctx, dtmCmd())
			},
			want: codes.CodeOrderStatus,
		},
		{
			name: "作废不到正式单",
			run: func(s *OrderService, ctx context.Context) error {
				return s.DtmCancelOrder(ctx, dtmCmd())
			},
			// 定位不到与状态不允许在 SQL 层是同一种现象（条件更新 0 行），
			// 故同一个 3003；见 dtmMapErr 的注释。
			want: codes.CodeOrderStatus,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := dbmock.New(t)
			svc := newDtmSvc(t, db)

			err := tc.run(svc, dtmCtx("gid-def", sagaBranchID, "action"))
			if err == nil {
				t.Fatal("必须报错")
			}
			if !dtmx.IsBusinessFailure(err) {
				t.Fatalf("业务失败必须包 dtmx.BusinessFailure（= dtmcli.ErrFailure），否则 dtm 会退避重试：%v", err)
			}
			be, ok := codes.As(err)
			if !ok {
				t.Fatalf("必须带 codes 码：%v", err)
			}
			if be.Code != tc.want {
				t.Fatalf("码 = %d, want %d（%v）", be.Code, tc.want, be)
			}
		})
	}
}

// TestMethodsRejectRequestsWithoutBarrierMetadata 没有 dtm metadata 时必须报错，
// 且不得当成业务失败 —— 否则一次装配错位会伪装成「订单状态不对」。
//
// 反向探针：把 dtmBranch 里的 `if err != nil { return fmt.Errorf(...) }` 改成
// 忽略（barrier 为 nil 时继续跑），本用例必红（无屏障落库）。
func TestMethodsRejectRequestsWithoutBarrierMetadata(t *testing.T) {
	for _, m := range allMethods() {
		t.Run(m.name, func(t *testing.T) {
			db := dbmock.New(t)
			svc := newDtmSvc(t, db)

			err := m.call(svc, context.Background(), dtmCmd())
			if err == nil {
				t.Fatal("缺 dtm metadata 必须报错，不能无屏障落库")
			}
			if !strings.Contains(err.Error(), "barrier") {
				t.Fatalf("错误文案须点明 barrier，实际：%v", err)
			}
			if dtmx.IsBusinessFailure(err) {
				t.Fatal("缺 barrier 是装配错位（可重试），不得报成业务失败 —— 那会让全局事务被立即判死")
			}
			if state := stateOf(db); state != "orders=0 items=0 outbox=0 logs=0" {
				t.Fatalf("缺 barrier 时不得写任何数据，实得 %s", state)
			}
		})
	}
}

// TestValidationRejectsBadPayloadBeforeBarrier 坏载荷必须在**取 barrier 之前**
// 被拒，且不留 barrier 行：重试一万次也还是坏，而 barrier 行会让它看起来像
// 「已经处理过」。
func TestValidationRejectsBadPayloadBeforeBarrier(t *testing.T) {
	cases := []struct {
		name string
		cmd  model.DtmOrderCmd
		want int64
	}{
		{name: "明细为空", cmd: model.DtmOrderCmd{UserID: 7, OrderNo: "O-1", DtmGID: "g"}, want: codes.CodeOrderItemsEmpty},
		{name: "没有单号", cmd: model.DtmOrderCmd{UserID: 7, DtmGID: "g", Items: dtmCmd().Items}, want: codes.CodeInvalidParam},
		{name: "没有 gid", cmd: model.DtmOrderCmd{UserID: 7, OrderNo: "O-1", Items: dtmCmd().Items}, want: codes.CodeInvalidParam},
		{name: "没有用户", cmd: model.DtmOrderCmd{OrderNo: "O-1", DtmGID: "g", Items: dtmCmd().Items}, want: codes.CodeInvalidParam},
		{name: "数量非法", cmd: model.DtmOrderCmd{
			UserID: 7, OrderNo: "O-1", DtmGID: "g",
			Items: []model.OrderItemCmd{{ProductID: 1, Quantity: 0, UnitPrice: 1}},
		}, want: codes.CodeInvalidParam},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := dbmock.New(t)
			svc := newDtmSvc(t, db)

			err := svc.DtmCreateOrder(dtmCtx("gid-bad", sagaBranchID, "action"), tc.cmd)
			if err == nil {
				t.Fatal("坏载荷必须报错")
			}
			if !dtmx.IsBusinessFailure(err) {
				t.Fatalf("坏载荷是业务失败（编排侧 payload 生成有问题，重试无用）：%v", err)
			}
			if be, ok := codes.As(err); !ok || be.Code != tc.want {
				t.Fatalf("码 = %v, want %d", be, tc.want)
			}
			if db.BarrierCount() != 0 {
				t.Fatalf("校验排在 barrier 之前：坏载荷不得留下 barrier 行（实际 %d 条）", db.BarrierCount())
			}
		})
	}
}

// TestInfraFailureIsNotBusinessFailure 句柄不可用属基础设施故障，原样上抛让 dtm
// 退避重试 —— 标成业务失败等于告诉 dtm「别重试了，去回滚」，一次装配错位会变成
// 一次真实回滚（把已成功的订单回补掉）。
func TestInfraFailureIsNotBusinessFailure(t *testing.T) {
	// 没有 ConnPool 的裸 gorm 句柄：RawDB 报错 → 取不到 *sql.DB。
	bad := NewOrderService(repository.NewOrderRepository(&gorm.DB{Config: &gorm.Config{}}, false),
		nil, nil, nil, false, nil)

	err := bad.DtmCreateOrder(dtmCtx("gid-infra", sagaBranchID, "action"), dtmCmd())
	if err == nil {
		t.Fatal("句柄不可用必须报错")
	}
	if dtmx.IsBusinessFailure(err) {
		t.Fatalf("基础设施故障不得带业务失败哨兵（dtm 会不重试而直接转补偿）：%v", err)
	}
}

// TestOnlyCreateAndConfirmWriteOutbox 口径②：五个分支里只有 CreateOrder 与
// ConfirmFreeze 写 outbox。
//
// 反向探针：把 DtmTryPlace 也调仓库的 outbox 写入，本用例必红
// （TryPlace 之后 outbox=1）。
func TestOnlyCreateAndConfirmWriteOutbox(t *testing.T) {
	const gid = "gid-outbox"
	cmd := dtmCmd()

	// TCC 链：Try → Confirm。
	tcc := dbmock.New(t)
	svc := newDtmSvc(t, tcc)
	if err := svc.DtmTryPlace(dtmCtx(gid, tccBranchID, "try"), cmd); err != nil {
		t.Fatalf("TryPlace: %v", err)
	}
	if n := len(tcc.Outboxes()); n != 0 {
		t.Fatalf("TryPlace 不得写 outbox，实际 %d 条", n)
	}
	if err := svc.DtmConfirmFreeze(dtmCtx(gid, tccBranchID, "confirm"), cmd); err != nil {
		t.Fatalf("ConfirmFreeze: %v", err)
	}
	if n := len(tcc.Outboxes()); n != 1 {
		t.Fatalf("ConfirmFreeze 必须写 1 条 outbox，实际 %d 条", n)
	}

	// Saga 链：Create → Cancel。
	saga := dbmock.New(t)
	svc2 := newDtmSvc(t, saga)
	if err := svc2.DtmCreateOrder(dtmCtx(gid, sagaBranchID, "action"), cmd); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if n := len(saga.Outboxes()); n != 1 {
		t.Fatalf("CreateOrder 必须写 1 条 outbox，实际 %d 条", n)
	}
	if err := svc2.DtmCancelOrder(dtmCtx(gid, sagaBranchID, "compensate"), cmd); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if n := len(saga.Outboxes()); n != 1 {
		t.Fatalf("作废补偿不得新增 outbox 行，实际 %d 条（下游会重复消费）", n)
	}
}

// TestBranchInvalidatesOrderCache 分支改动订单后必须失效详情缓存：
// 详情缓存 key 是 (user_id, order_id)，而 ConfirmFreeze 改了 status ——
// 不失效的话 GetOrder 会把缓存里的 presale_locked 一直返回下去。
//
// 反向探针：把 dtmBranch 里的 s.invalidate(...) 删掉，本用例必红（deleted 为空）。
func TestBranchInvalidatesOrderCache(t *testing.T) {
	const gid = "gid-cache"
	cmd := dtmCmd()
	db := dbmock.New(t)
	cache := &fakeCache{}
	svc := newDtmSvcWithCache(db, cache)

	if err := svc.DtmCreateOrder(dtmCtx(gid, sagaBranchID, "action"), cmd); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if len(cache.deleted) != 1 {
		t.Fatalf("正向落单后必须失效一次详情缓存，实得 %v", cache.deleted)
	}
	if !strings.Contains(cache.deleted[0], "7") {
		t.Fatalf("失效的 key 必须含 user_id：%q", cache.deleted[0])
	}

	// 换一张单跑 TCC，确认后同样要失效。
	tccCmd := cmd
	tccCmd.OrderNo = "O-DTM-2"
	tccDB := dbmock.New(t)
	tccCache := &fakeCache{}
	tccSvc := newDtmSvcWithCache(tccDB, tccCache)
	if err := tccSvc.DtmTryPlace(dtmCtx(gid, tccBranchID, "try"), tccCmd); err != nil {
		t.Fatalf("TryPlace: %v", err)
	}
	if err := tccSvc.DtmConfirmFreeze(dtmCtx(gid, tccBranchID, "confirm"), tccCmd); err != nil {
		t.Fatalf("ConfirmFreeze: %v", err)
	}
	// 两次：TryPlace 建单后一次、ConfirmFreeze 改状态后一次。两个分支都动了
	// orders 行，而详情缓存的 TTL 是 5 分钟 —— 少任何一次失效，读侧都会在窗口内
	// 看到上一态（多半是那张 presale_locked 占位单）。
	if len(tccCache.deleted) != 2 {
		t.Fatalf("确认后必须失效详情缓存（否则读侧一直看到 presale_locked），实得 %v", tccCache.deleted)
	}
}
