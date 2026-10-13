// dtm 分支业务方法的单测（纯 Go 假驱动）。
//
// 每个分支方法都必须同时守住三类：
//  1. 正常写成功 —— 库存更新与流水插入都发生，且 barrier 行与业务写同事务提交；
//  2. 库存/冻结不足 → **业务失败** —— dtm 收到后立刻转补偿/回滚，不做退避重试；
//  3. barrier 幂等 —— dtm 重试或空补偿时，业务写一次都不能多。
//
// 第 3 条是本文件最要紧的。反向探针：把 dtmBranch 里的 barrier.Call 换成直接调
// write(ctx, tx, cmd)，本文件 TestBranchReplayIsIdempotent 必红 —— 没有 barrier
// 行，重放会真的再扣一次。
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
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"
	"gorm.io/gorm"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/dbmock"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/model"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/repository"
)

// TestMain 先把 dtm 的 barrier 表名设成本部署用的那个。
//
// 必须设：官方默认是跨库的 dtm_barrier.barrier，而本域的 barrier 表落在业务库内
// （examples/micro/scripts/schema-micro.sql）。app/setup.go 的 dtmx.Setup 做的是
// 同一件事 —— 测试不跟着设，测的就不是真机上的那条路径，而「barrier 行有没有插在
// 业务库」正是这里最要紧的断言。
//
// 不设的后果很隐蔽：正向分支照样跑通（barrier 行插不进去也不影响它执行业务写），
// 而补偿/取消分支会被官方语义判成「空补偿」直接返回 —— 正向账扣了、补偿账没补，
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
	// guard 正常路径里必须出现的库存更新片段
	guard string
	// prereq 前置正向分支（逆序分支才需要；nil 表示自身即正向）
	prereq *dtmMethod
	// call 调 service 的分支方法
	call func(s *StockService, ctx context.Context, cmd model.DtmStockCmd) error
}

// forwardMethods 正向分支（自带 barrier 语义，可单独到达）。
func forwardMethods() []*dtmMethod {
	return []*dtmMethod{
		{name: model.DtmDeductBranch, branchID: sagaBranchID, op: "action", guard: "available = available - ?",
			call: func(s *StockService, ctx context.Context, c model.DtmStockCmd) error { return s.DtmDeduct(ctx, c) }},
		{name: model.DtmTryFreezeBranch, branchID: tccBranchID, op: "try", guard: "frozen = frozen + ?",
			call: func(s *StockService, ctx context.Context, c model.DtmStockCmd) error { return s.DtmTryFreeze(ctx, c) }},
		{name: model.DtmConfirmBranch, branchID: tccBranchID, op: "confirm", guard: "frozen = frozen - ?",
			call: func(s *StockService, ctx context.Context, c model.DtmStockCmd) error {
				return s.DtmConfirmFreeze(ctx, c)
			}},
	}
}

// inverseMethods 逆序分支：Saga 补偿与 TCC 的一阶段回退。
//
// 它们由 dtm 异步发起，载荷里没有 traceparent，且**必须**有同 branch_id 的正向
// 行先落地，否则被 barrier 判成空补偿（那另有专门用例）。
func inverseMethods() []*dtmMethod {
	fwd := forwardMethods()
	return []*dtmMethod{
		{name: model.DtmReleaseBranch, branchID: sagaBranchID, op: "compensate",
			guard: "available = available + ?", prereq: fwd[0],
			call: func(s *StockService, ctx context.Context, c model.DtmStockCmd) error { return s.DtmRelease(ctx, c) }},
		{name: model.DtmCancelFreeze, branchID: tccBranchID, op: "cancel",
			guard: "available = available + ?", prereq: fwd[1],
			call: func(s *StockService, ctx context.Context, c model.DtmStockCmd) error {
				return s.DtmCancelFreeze(ctx, c)
			}},
	}
}

func allMethods() []*dtmMethod {
	return append(forwardMethods(), inverseMethods()...)
}

// newDtmSvc 造一条只挂 dbx 句柄的 service（cache/kv/lock 全 nil：分支面不需要，
// 预占那套互斥与熔断是 HTTP 面才走的路）。
func newDtmSvc(t *testing.T, db *dbmock.DB) *StockService {
	t.Helper()
	gdb := &gorm.DB{Config: &gorm.Config{ConnPool: db.SQL()}}
	return NewStockService(repository.NewStockRepository(gdb, false), nil, nil, nil, false, nil)
}

func dtmCmd() model.DtmStockCmd {
	return model.DtmStockCmd{OrderNo: "SO-DTM-1", ProductID: 42, Quantity: 3, UserID: 9}
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

// arrive 让一个分支到达（逆序分支先跑它的正向），返回假库句柄。
//
// 逆序分支跑完后把执行记录归零（barrier 表保留），于是断言只看见逆序那一次写。
func arrive(t *testing.T, db *dbmock.DB, m *dtmMethod, gid string) *StockService {
	t.Helper()
	svc := newDtmSvc(t, db)
	if m.prereq != nil {
		if err := m.prereq.call(svc, dtmCtx(gid, m.prereq.branchID, m.prereq.op), dtmCmd()); err != nil {
			t.Fatalf("前置正向分支 %s 应成功：%v", m.prereq.name, err)
		}
		db.ResetLog()
	}
	return svc
}

// TestBranchWritesInsideBarrier 正常路径：barrier 行与业务写在同一事务里提交。
func TestBranchWritesInsideBarrier(t *testing.T) {
	for _, m := range allMethods() {
		t.Run(m.name, func(t *testing.T) {
			const gid = "gid-1"
			db := dbmock.New(t)
			svc := arrive(t, db, m, gid)

			if err := m.call(svc, dtmCtx(gid, m.branchID, m.op), dtmCmd()); err != nil {
				t.Fatalf("分支写应成功：%v", err)
			}
			if db.BarrierCount() == 0 {
				t.Fatalf("barrier 行必须由 dtmx.Barrier.Call 插（与业务写同事务），实际序列：\n  %s",
					strings.Join(db.Stmts(), "\n  "))
			}
			if db.ExecCount(m.guard) == 0 {
				t.Fatalf("库存更新 %q 未执行，实际序列：\n  %s", m.guard, strings.Join(db.Stmts(), "\n  "))
			}
			if db.ExecCount("insert into stock_logs") != 1 {
				t.Fatalf("流水必须写一条，实际序列：\n  %s", strings.Join(db.Stmts(), "\n  "))
			}
			if db.Commits() != 1 || db.Rollbacks() != 0 {
				t.Fatalf("正常路径应恰好提交一次且不回滚，实际 commit=%d rollback=%d", db.Commits(), db.Rollbacks())
			}
		})
	}
}

// TestBranchDeficitIsBusinessFailure 库存/冻结不足 → 业务失败，且带 4xxx 业务码。
func TestBranchDeficitIsBusinessFailure(t *testing.T) {
	for _, m := range allMethods() {
		t.Run(m.name, func(t *testing.T) {
			const gid = "gid-2"
			db := dbmock.New(t)
			svc := arrive(t, db, m, gid)
			db.SetAffected("update stocks", 0)

			err := m.call(svc, dtmCtx(gid, m.branchID, m.op), dtmCmd())
			if err == nil {
				t.Fatal("条件更新未命中却返回成功 —— 账没扣却告诉 dtm 成功")
			}
			if !dtmx.IsBusinessFailure(err) {
				t.Fatalf("不足必须是业务失败（dtm 立刻转补偿/回滚），实际 %v", err)
			}
			be, ok := codes.As(err)
			if !ok {
				t.Fatalf("分支失败必须带 4xxx 业务码，实际 %v", err)
			}
			if be.Code != codes.CodeStockInsufficient && be.Code != codes.CodeStockNotFound {
				t.Fatalf("业务码 = %d，库存域不足类只应是 4001/4002", be.Code)
			}
			if db.Commits() != 0 || db.Rollbacks() != 1 {
				t.Fatalf("业务失败必须整笔回滚（barrier 行也随之消失，dtm 才会重试/转补偿），实际 commit=%d rollback=%d",
					db.Commits(), db.Rollbacks())
			}
			if db.ExecCount("insert into stock_logs") != 0 {
				t.Fatal("未扣成不得写流水")
			}
		})
	}
}

// TestBranchReplayIsIdempotent 幂等：dtm 重复回调同一分支时，第二次不得产生任何
// 业务写（barrier 的 insert ignore 影响 0 行 → 官方语义直接跳过业务回调）。
func TestBranchReplayIsIdempotent(t *testing.T) {
	for _, m := range allMethods() {
		t.Run(m.name, func(t *testing.T) {
			const gid = "gid-3"
			db := dbmock.New(t)
			svc := arrive(t, db, m, gid)

			if err := m.call(svc, dtmCtx(gid, m.branchID, m.op), dtmCmd()); err != nil {
				t.Fatalf("首次到达应成功：%v", err)
			}
			first := db.ExecCount("update stocks")
			if first != 1 {
				t.Fatalf("首次到达应写一次库存，实际 %d 次", first)
			}

			db.ResetLog()
			if err := m.call(svc, dtmCtx(gid, m.branchID, m.op), dtmCmd()); err != nil {
				t.Fatalf("barrier 判重必须回成功（返回 error 会让 dtm 无限重试）：%v", err)
			}
			if n := db.ExecCount("update stocks"); n != 0 {
				t.Fatalf("重放不得再动库存，实际又执行了 %d 次", n)
			}
			if n := db.ExecCount("insert into stock_logs"); n != 0 {
				t.Fatalf("重放不得再写流水，实际又写了 %d 条", n)
			}
			if db.BarrierCount() == 0 {
				t.Fatal("判重也要记 barrier（下次重试仍要能判出来）")
			}
		})
	}
}

// TestNullCompensationWritesNothing 空补偿（逆向分支先于正向到达）不得写库。
//
// 这条守卫是 barrier 存在的主要理由：少它，补偿会凭空把库存加回去，而正向账
// 从来没扣过 —— 于是 available 虚高，可以卖超。
func TestNullCompensationWritesNothing(t *testing.T) {
	for _, m := range inverseMethods() {
		t.Run(m.name, func(t *testing.T) {
			db := dbmock.New(t)
			svc := newDtmSvc(t, db)
			// 故意**不**跑前置正向：barrier 探不到同 branch_id 的 action/try 行，
			// 官方语义判为空补偿。
			if err := m.call(svc, dtmCtx("gid-4", m.branchID, m.op), dtmCmd()); err != nil {
				t.Fatalf("空补偿必须回成功：%v", err)
			}
			if n := db.ExecCount("update stocks"); n != 0 {
				t.Fatalf("空补偿不得写库存，实际 %d 次", n)
			}
			if n := db.ExecCount("insert into stock_logs"); n != 0 {
				t.Fatalf("空补偿不得写流水，实际 %d 条", n)
			}
			if db.BarrierCount() == 0 {
				t.Fatal("空补偿也要记 barrier（否则每次重试都重判一次空补偿）")
			}
		})
	}
}

// TestBranchWithoutBarrierMetaFails 分支必须带 dtm metadata：
// 取不到 barrier 还继续，等于在无屏障状态下落库，且当场没有任何迹象。
func TestBranchWithoutBarrierMetaFails(t *testing.T) {
	for _, m := range allMethods() {
		t.Run(m.name, func(t *testing.T) {
			db := dbmock.New(t)
			svc := newDtmSvc(t, db)

			err := m.call(svc, context.Background(), dtmCmd())
			if err == nil {
				t.Fatal("没有 dtm metadata 时必须报错，不能无屏障落库")
			}
			if !strings.Contains(err.Error(), "barrier") {
				t.Fatalf("错误文案须点明 barrier 便于 dtm 侧排障，实际：%v", err)
			}
			if db.ExecCount("update stocks") != 0 {
				t.Fatal("取不到 barrier 时一条业务写都不该发生")
			}
		})
	}
}

// TestBranchRejectsBadParams 参数非法同样是业务失败（重试一万次也不会变好）。
func TestBranchRejectsBadParams(t *testing.T) {
	cases := []struct {
		name string
		cmd  model.DtmStockCmd
	}{
		{"no product", model.DtmStockCmd{OrderNo: "SO-1", Quantity: 1}},
		{"zero qty", model.DtmStockCmd{OrderNo: "SO-1", ProductID: 1}},
		{"negative qty", model.DtmStockCmd{OrderNo: "SO-1", ProductID: 1, Quantity: -1}},
		{"no order_no", model.DtmStockCmd{ProductID: 1, Quantity: 1}},
	}
	for _, m := range allMethods() {
		for _, c := range cases {
			t.Run(m.name+"/"+c.name, func(t *testing.T) {
				db := dbmock.New(t)
				svc := newDtmSvc(t, db)

				err := m.call(svc, dtmCtx("gid-5", m.branchID, m.op), c.cmd)
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
				if db.BarrierCount() != 0 {
					t.Fatal("参数校验必须在开事务之前，否则每次重试都会留下一行 barrier")
				}
			})
		}
	}
}

// TestBranchInfraErrorIsNotBusinessFailure 基础设施故障不得被标成业务失败：
// 标了 dtm 就不重试，一次 DB 抖动会把整条 Saga 判死并触发回补。
//
// 这里用「句柄取不到 *sql.DB」模拟基础设施不可用 —— 它在真实部署里的对应物是
// dbx 句柄未装配 / 换驱动后 ConnPool 不是 *sql.DB。
func TestBranchInfraErrorIsNotBusinessFailure(t *testing.T) {
	svc := NewStockService(repository.NewStockRepository(&gorm.DB{Config: &gorm.Config{}}, false),
		nil, nil, nil, false, nil)

	err := svc.DtmDeduct(dtmCtx("gid-6", sagaBranchID, "action"), dtmCmd())
	if err == nil {
		t.Fatal("句柄不可用时必须报错")
	}
	if dtmx.IsBusinessFailure(err) {
		t.Fatalf("基础设施故障不得标成业务失败（否则 dtm 不重试、整条 Saga 直接判死）：%v", err)
	}
	be, ok := codes.As(err)
	if ok && (be.Code == codes.CodeStockInsufficient || be.Code == codes.CodeStockNotFound) {
		t.Fatalf("句柄故障不应被翻译成库存类业务码 %d", be.Code)
	}
}
