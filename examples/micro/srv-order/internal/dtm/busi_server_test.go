// 分支 handler 的单测。
//
// 本文件只测**协议层**该守的东西：全方法名与 dtm 回调 URL 是否一致、traceparent
// 是否续链、失败注入是否点火、以及口径①「失败必须走 gRPC status」这条分流 ——
// dtm server 只看 status（dtmsvr/trans_status.go 的 getGrpcResult 从不解析响应
// 字节），codes.Aborted 才是业务失败（不重试、转而补偿），其余 status 一律按
// 基础失败退避重试。弄反了就是一次「订单状态不对」把整条 Saga 判死。
package dtm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelprop "go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"gorm.io/gorm"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/dbmock"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/dtm/protos"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/model"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/repository"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/service"
)

var spans *tracetest.SpanRecorder

func TestMain(m *testing.M) {
	spans = tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	otel.SetTracerProvider(tp)
	// 分支面靠载荷里的 traceparent 续链，全局传播器必须先装好（W3C TraceContext）。
	// 生产由 launchx 的 telemetry 步骤装；单测里没人装，于是这里补上 ——
	// 缺了它 Extract 是 noop，「续链」这一条会假绿。
	otel.SetTextMapPropagator(otelprop.TraceContext{})
	// barrier 表名必须设成本部署用的 barrier（落在 order_db 内），而不是官方默认的
	// dtm_barrier.barrier —— 后者是独立库，与业务写不同库，CallWithDB 在业务连接
	// 的事务里插不进那张表。app/setup.go 的 dtm.Setup 做的是同一件事。
	//
	// 不设的后果很隐蔽：正向分支照样跑通（barrier 行插不进去也不影响它执行业务写），
	// 而补偿/回退分支会被官方语义判成「空补偿」直接返回 —— 于是正向的订单建了、
	// 补偿的作废没落，两边都报成功。
	dtmx.SetBarrierTableName("barrier")
	code := m.Run()
	_ = tp.Shutdown(context.Background())
	os.Exit(code)
}

const testTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func newTestServer(t *testing.T) (*BusiServer, *dbmock.DB) {
	t.Helper()
	db := dbmock.New(t)
	svc := newSvc(db)
	return NewBusiServer(svc, true, nil), db
}

func newSvc(db *dbmock.DB) *service.OrderService {
	gdb := &gorm.DB{Config: &gorm.Config{ConnPool: db.SQL()}}
	return service.NewOrderService(repository.NewOrderRepository(gdb, false), nil, nil, nil, false, nil)
}

func req(t *testing.T, branch, failInject string, cmd model.DtmOrderCmd) *protos.OrderBusiReq {
	t.Helper()
	b, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal biz: %v", err)
	}
	return &protos.OrderBusiReq{Biz: string(b), FailInject: failInject}
}

func dtmCtx(gid, branchID, op string) context.Context {
	md := metadata.New(map[string]string{
		"dtm-gid":        gid,
		"dtm-trans_type": "saga",
		"dtm-branch_id":  branchID,
		"dtm-op":         op,
	})
	return metadata.NewIncomingContext(context.Background(), md)
}

func bizCmd() model.DtmOrderCmd {
	return model.DtmOrderCmd{
		UserID:  7,
		OrderNo: "O-DTM-1",
		DtmGID:  "gid-ok",
		Items:   []model.OrderItemCmd{{ProductID: 11, Name: "sku-11", Quantity: 2, UnitPrice: 150}},
	}
}

// sagaBranchID / tccBranchID：一个 saga 步骤 / 一对 tcc 分支在 dtm server 里的
// branch_id。**正向与逆向共用同一个**（dtmsvr/trans_type_saga.go 的 GenBranches
// 给同一步的 action/compensate 同一个 "%02d"；TCC 见 dtmsvr/api.go 的
// svcRegisterBranch 复制同一个 branch）。barrier 判空补偿靠的正是这个共用 id，
// 逆序用例不先跑正向就只会测到空补偿。
const (
	sagaBranchID = "01"
	tccBranchID  = "01"
)

// branchCall 一个分支在 handler 上的调用形态。
type branchCall struct {
	method string
	branch string
	// branchID 该分支在 dtm server 里的 branch_id
	branchID string
	// op dtm 带的操作名
	op    string
	guard string
	call  func(ctx context.Context, r *protos.OrderBusiReq) (*protos.OrderBusiResp, error)
	// compensation 是否逆向分支（CancelOrder / CancelFreeze）
	compensation bool
	// prereq 前置正向分支；nil 表示自身即正向
	prereq *branchCall
}

// branchCalls 把五个方法收成「分支名 → 调用形态」，便于按同一组断言逐个跑。
func branchCalls(s *BusiServer) map[string]branchCall {
	createOrder := &branchCall{method: "CreateOrder", branch: model.DtmCreateOrderBranch,
		branchID: sagaBranchID, op: "action", guard: "insert ignore into orders", call: s.CreateOrder}
	tryPlace := &branchCall{method: "TryPlace", branch: model.DtmTryPlaceBranch,
		branchID: tccBranchID, op: "try", guard: "insert ignore into orders", call: s.TryPlace}
	m := map[string]*branchCall{
		model.DtmCreateOrderBranch: createOrder,
		model.DtmTryPlaceBranch:    tryPlace,
		// ConfirmFreeze 把**占位单原地转成正式单**，而占位单只可能由 TryPlace 建出来 ——
		// 与库存域的 Confirm 不同（那边 stocks 行早就在，Confirm 只改列），订单域
		// 这里少这一步就是「定位不到单」。故它的 prereq 是 TryPlace。
		model.DtmConfirmFreezeBranch: {method: "ConfirmFreeze", branch: model.DtmConfirmFreezeBranch,
			branchID: tccBranchID, op: "confirm", guard: "paid_amount = total_amount", call: s.ConfirmFreeze,
			prereq: tryPlace},
	}
	// 逆向分支的 prereq 必须与它们同 branchID：barrier 就是靠这个共用 id 去探
	// 「同一步的正向行在不在」，探不到就判空补偿、业务写根本不会被调用。
	m[model.DtmCancelOrderBranch] = &branchCall{method: "CancelOrder", branch: model.DtmCancelOrderBranch,
		branchID: sagaBranchID, op: "compensate", guard: "update orders set status = ?", call: s.CancelOrder,
		compensation: true, prereq: createOrder}
	m[model.DtmCancelFreezeBranch] = &branchCall{method: "CancelFreeze", branch: model.DtmCancelFreezeBranch,
		branchID: tccBranchID, op: "cancel", guard: "update orders set status = ?", call: s.CancelFreeze,
		compensation: true, prereq: tryPlace}
	out := map[string]branchCall{}
	for k, v := range m {
		out[k] = *v
	}
	return out
}

// branchNames 五个分支名（= busi.proto 里的方法名，编排侧按它点火）。
func branchNames() []string {
	return []string{
		model.DtmCreateOrderBranch,
		model.DtmCancelOrderBranch,
		model.DtmTryPlaceBranch,
		model.DtmConfirmFreezeBranch,
		model.DtmCancelFreezeBranch,
	}
}

// arrive 让 tc 到达（逆序分支先跑它的正向），并把执行记录归零，使断言只看见这一次。
func arrive(t *testing.T, db *dbmock.DB, tc *branchCall, gid string) {
	t.Helper()
	if tc.prereq == nil {
		return
	}
	pr := *tc.prereq
	cmd := bizCmd()
	cmd.DtmGID = gid
	if _, err := pr.call(dtmCtx(gid, pr.branchID, pr.op), req(t, pr.branch, "", cmd)); err != nil {
		t.Fatalf("前置正向分支 %s 应成功：%v", pr.branch, err)
	}
	db.ResetLog()
}

// TestFullMethodNamesMatchDtmCallbackURL 全方法名与编排侧拼的回调 URL 必须一致。
//
// dtm server 只按 URL 回调，一个字符对不上就是「分支 404，全局事务永远挂着」，
// 而本地没有任何迹象会指向 proto package 写错了。
//
// 反向探针：把 busi.proto 的 package 从 dtmbranch.order 改成别的并重新生成，
// 本用例必红。
func TestFullMethodNamesMatchDtmCallbackURL(t *testing.T) {
	want := map[string]string{
		"CreateOrder":   "/dtmbranch.order.OrderBusi/CreateOrder",
		"CancelOrder":   "/dtmbranch.order.OrderBusi/CancelOrder",
		"TryPlace":      "/dtmbranch.order.OrderBusi/TryPlace",
		"ConfirmFreeze": "/dtmbranch.order.OrderBusi/ConfirmFreeze",
		"CancelFreeze":  "/dtmbranch.order.OrderBusi/CancelFreeze",
	}
	got := map[string]string{
		"CreateOrder":   protos.OrderBusi_CreateOrder_FullMethodName,
		"CancelOrder":   protos.OrderBusi_CancelOrder_FullMethodName,
		"TryPlace":      protos.OrderBusi_TryPlace_FullMethodName,
		"ConfirmFreeze": protos.OrderBusi_ConfirmFreeze_FullMethodName,
		"CancelFreeze":  protos.OrderBusi_CancelFreeze_FullMethodName,
	}
	for m, w := range want {
		if got[m] != w {
			t.Errorf("方法 %s 的全名 = %q，编排侧按 %q 回调", m, got[m], w)
		}
	}
	if protos.OrderBusi_ServiceDesc.ServiceName != "dtmbranch.order.OrderBusi" {
		t.Errorf("服务名 = %q", protos.OrderBusi_ServiceDesc.ServiceName)
	}
	if n := len(protos.OrderBusi_ServiceDesc.Methods); n != len(want) {
		t.Errorf("注册方法数 = %d, want %d（少一个编排侧就会有一条分支 404）", n, len(want))
	}
	// 分支名（model 常量）必须与 proto 方法名逐字相等：编排侧的 fail_inject 按
	// 分支名点火、span 名按方法名开，两处拼错就是「点火失败、正向全绿」。
	branches := map[string]string{
		"CreateOrder":   model.DtmCreateOrderBranch,
		"CancelOrder":   model.DtmCancelOrderBranch,
		"TryPlace":      model.DtmTryPlaceBranch,
		"ConfirmFreeze": model.DtmConfirmFreezeBranch,
		"CancelFreeze":  model.DtmCancelFreezeBranch,
	}
	for method, branch := range branches {
		if method != branch {
			t.Errorf("方法名 %q 与分支名 %q 不一致（fail_inject 点不着它）", method, branch)
		}
	}
}

// TestBranchSucceeds 正常路径：resp.Code=0、grpc 无 error、账真的动了。
func TestBranchSucceeds(t *testing.T) {
	for _, name := range branchNames() {
		t.Run(name, func(t *testing.T) {
			spans.Reset()
			const gid = "gid-ok"
			s, db := newTestServer(t)
			tc := branchCalls(s)[name]
			arrive(t, db, &tc, gid)
			cmd := bizCmd()
			cmd.DtmGID = gid
			resp, err := tc.call(dtmCtx(gid, tc.branchID, tc.op), req(t, tc.branch, "", cmd))
			if err != nil {
				t.Fatalf("分支应成功：%v", err)
			}
			if resp.GetCode() != codes.CodeOK {
				t.Fatalf("code = %d, want 0（message=%q）", resp.GetCode(), resp.GetMessage())
			}
			if db.ExecCount(tc.guard) == 0 {
				t.Fatalf("订单未按分支语义变动，缺 %q：%v", tc.guard, db.Stmts())
			}
			assertSpan(t, "rpc.dtm.order."+tc.method, gid, tc.branchID, tc.compensation)
		})
	}
}

// TestFailInjectPoisonsOnlyItsOwnBranch fail_inject 命中本分支即返业务失败，
// 供编排侧点火 Saga 补偿 / TCC Cancel。
func TestFailInjectPoisonsOnlyItsOwnBranch(t *testing.T) {
	for _, name := range branchNames() {
		t.Run(name, func(t *testing.T) {
			const gid = "gid-inject"
			s, db := newTestServer(t)
			tc := branchCalls(s)[name]
			arrive(t, db, &tc, gid)
			cmd := bizCmd()
			cmd.DtmGID = gid
			// 逆序分支的前置正向已经在 arrive() 里落库，故这里比的是「本次调用
			// 有没有再动账」，不是「表里是不是空的」。
			beforeOrders := len(db.Orders())
			beforeOutbox := len(db.Outboxes())

			_, err := tc.call(dtmCtx(gid, tc.branchID, tc.op), req(t, tc.branch, tc.branch, cmd))
			if err == nil {
				t.Fatal("命中失败注入必须返错误，否则点火脚本跑完正向全绿、永远等不到补偿")
			}
			if got := status.Code(err); got != grpccodes.Aborted {
				t.Fatalf("grpc status = %s, want Aborted（dtm 据此判业务失败并立即转补偿）", got)
			}
			if len(db.Orders()) != beforeOrders || len(db.Outboxes()) != beforeOutbox {
				t.Fatalf("失败注入时不得写库：orders %d→%d、outbox %d→%d",
					beforeOrders, len(db.Orders()), beforeOutbox, len(db.Outboxes()))
			}

			// 注入的是别的分支名时本分支必须正常跑通 —— 否则一个错字会让整条链全挂。
			db.ResetLog()
			if _, err = tc.call(dtmCtx(gid, tc.branchID, tc.op),
				req(t, tc.branch, tc.branch+"-typo", cmd)); err != nil {
				t.Fatalf("注入别的分支不应影响本分支：%v", err)
			}
		})
	}
}

// TestBusinessFailureIsAbortedAndInfraIsInternal 口径①：失败必须走 status code。
//
// 业务失败 → codes.Aborted（dtm 不重试、转补偿）；基础设施故障 → codes.Internal
// （dtm 退避重试）。用 Internal 而不是 Unknown 是显式选择：Unknown 在
// GrpcError2DtmError 里与 Aborted 一样落到 default 分支，行为虽同，但日志与
// 抓包上「Internal」才说明得出是本域自己判的失败。
//
// 反向探针：把 busi_server.go 的 branchError 换成 `return err`（让 grpc-go 自己
// 编成 Unknown），本用例的 Internal 断言必红。
func TestBusinessFailureIsAbortedAndInfraIsInternal(t *testing.T) {
	s, db := newTestServer(t)
	// 业务失败：确认一个不存在的占位单。
	_, err := s.ConfirmFreeze(dtmCtx("gid-def", tccBranchID, "confirm"), req(t, model.DtmConfirmFreezeBranch, "", bizCmd()))
	if err == nil {
		t.Fatal("确认不到占位单必须报错")
	}
	if got := status.Code(err); got != grpccodes.Aborted {
		t.Fatalf("业务失败的 status = %s, want Aborted（dtm 据此不重试而转补偿）", got)
	}
	if n := len(db.Outboxes()); n != 0 {
		t.Fatalf("被拒的确认不得留下事件行，实际 %d 条", n)
	}

	// 基础设施故障：句柄不可用（没有 ConnPool 的裸 gorm 句柄）。
	brokenSvc := service.NewOrderService(repository.NewOrderRepository(&gorm.DB{Config: &gorm.Config{}}, false),
		nil, nil, nil, false, nil)
	bad := NewBusiServer(brokenSvc, true, nil)
	_, err = bad.CreateOrder(dtmCtx("gid-infra", sagaBranchID, "action"), req(t, model.DtmCreateOrderBranch, "", bizCmd()))
	if err == nil {
		t.Fatal("句柄不可用必须报错")
	}
	if got := status.Code(err); got != grpccodes.Internal {
		t.Fatalf("基础设施故障的 status = %s, want Internal", got)
	}
	if got := status.Code(err); got == grpccodes.Aborted {
		t.Fatalf("基础设施故障绝不能报 Aborted：dtm 会不重试而直接转补偿，一次 DB 抖动变成一次真实回滚：%v", err)
	}
}

// TestMalformedBizIsBusinessFailure 载荷里的 biz 不是合法 JSON → 业务失败。
func TestMalformedBizIsBusinessFailure(t *testing.T) {
	s, db := newTestServer(t)
	_, err := s.CreateOrder(dtmCtx("gid-bad", sagaBranchID, "action"),
		&protos.OrderBusiReq{Biz: "{not json"})
	if err == nil {
		t.Fatal("坏载荷必须报错 —— 带着空单号落库才是真事故")
	}
	if got := status.Code(err); got != grpccodes.Aborted {
		t.Fatalf("坏载荷是业务失败（编排侧 payload 生成有问题，重试无用），status = %s", got)
	}
	if n := len(db.Orders()); n != 0 {
		t.Fatalf("坏载荷不得写库，实际 %d 行", n)
	}
}

// TestMissingBarrierMetadataIsNotSwallowed 没有 dtm metadata 时必须报错，
// 且不得当成业务失败（否则一次装配错位会伪装成「订单状态不对」）。
func TestMissingBarrierMetadataIsNotSwallowed(t *testing.T) {
	s, db := newTestServer(t)
	_, err := s.CreateOrder(context.Background(), req(t, model.DtmCreateOrderBranch, "", bizCmd()))
	if err == nil {
		t.Fatal("缺 dtm metadata 必须报错，不能无屏障落库")
	}
	if !strings.Contains(err.Error(), "barrier") {
		t.Fatalf("错误文案须点明 barrier，实际：%v", err)
	}
	if status.Code(err) == grpccodes.Aborted {
		t.Fatal("缺 barrier 是装配错位（可重试），不得报成业务失败")
	}
	if n := len(db.Orders()); n != 0 {
		t.Fatalf("缺 barrier 时不得写库，实际 %d 行", n)
	}
}

// TestForwardBranchJoinsCallerTrace 正向分支必须挂回编排侧那条 trace。
func TestForwardBranchJoinsCallerTrace(t *testing.T) {
	s, _ := newTestServer(t)
	spans.Reset()

	r := req(t, model.DtmCreateOrderBranch, "", bizCmd())
	r.Traceparent = testTraceparent
	if _, err := s.CreateOrder(dtmCtx("gid-tr", sagaBranchID, "action"), r); err != nil {
		t.Fatalf("正向分支应成功：%v", err)
	}
	got := findSpan(t, "rpc.dtm.order.CreateOrder")
	if got.SpanContext().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("正向分支没挂回上游 trace，实际 trace_id=%s —— Jaeger 上会与 bff 的提交 span 分家",
			got.SpanContext().TraceID())
	}
	if !got.Parent().IsValid() {
		t.Fatal("正向分支 span 必须是子 span（父是载荷里的 span）")
	}
}

// TestCompensationBranchIsNewRootLinkedByGid 补偿分支没有 traceparent：
// 必须开新根 span，且凭 biz.dtm.gid 与正向链关联。
func TestCompensationBranchIsNewRootLinkedByGid(t *testing.T) {
	for _, name := range []string{model.DtmCancelOrderBranch, model.DtmCancelFreezeBranch} {
		t.Run(name, func(t *testing.T) {
			const gid = "gid-comp"
			spans.Reset()
			s, db := newTestServer(t)
			tc := branchCalls(s)[name]
			arrive(t, db, &tc, gid)
			// 注意载荷里**没有** traceparent —— 这正是 dtm 异步回调的真实形状。
			cmd := bizCmd()
			cmd.DtmGID = gid
			if _, err := tc.call(dtmCtx(gid, tc.branchID, tc.op), req(t, tc.branch, "", cmd)); err != nil {
				t.Fatalf("补偿分支应成功：%v", err)
			}
			got := findSpan(t, "rpc.dtm.order."+tc.method)
			if got.Parent().IsValid() {
				t.Fatal("补偿分支不应挂到任何父 span 上（载荷无 traceparent，凭空造同 ID 孤儿 span 是自欺）")
			}
			if got.SpanContext().TraceID().String() == "4bf92f3577b34da6a3ce929d0e0e4736" {
				t.Fatal("补偿分支复用了上游 trace id —— 那正是「伪造同 ID 孤儿 span」")
			}
			if !hasAttr(got.Attributes(), "biz.dtm.gid", gid) {
				t.Fatalf("补偿分支必须带 biz.dtm.gid 才能与正向链关联：%v", got.Attributes())
			}
			if !hasAttr(got.Attributes(), "biz.dtm.branch", tc.branchID) {
				t.Fatalf("biz.dtm.branch 应取 branch_id：%v", got.Attributes())
			}
			if !hasAttr(got.Attributes(), "biz.compensation", true) {
				t.Fatalf("逆向分支必须标 biz.compensation=true：%v", got.Attributes())
			}
		})
	}
}

// TestBranchSpanCarriesDtmAttrs 五个分支的 span 都带 gid / branch / compensation。
func TestBranchSpanCarriesDtmAttrs(t *testing.T) {
	for _, name := range branchNames() {
		t.Run(name, func(t *testing.T) {
			spans.Reset()
			const gid = "gid-attr"
			s, db := newTestServer(t)
			tc := branchCalls(s)[name]
			arrive(t, db, &tc, gid)
			cmd := bizCmd()
			cmd.DtmGID = gid
			if _, err := tc.call(dtmCtx(gid, tc.branchID, tc.op), req(t, tc.branch, "", cmd)); err != nil {
				t.Fatalf("分支应成功：%v", err)
			}
			assertSpan(t, "rpc.dtm.order."+tc.method, gid, tc.branchID, tc.compensation)
		})
	}
}

// TestBranchPbStaysOutOfKitexGen dtm 分支 pb 的生成产物只落在 internal/dtm/protos/，
// kitex_gen 保持原样 —— 这两面互不渗透（dtm-branch-facade spec 的硬要求）。
//
// 只靠「我把它生成在别处」是不够的：一次 `protoc --go_out=<kitex_gen>` 就能把
// grpc-go 生成码混进 kitex 目录，而那种混法的症状很怪 —— kitex 工具按 thriftgo
// 假设去处理该目录、生成物里出现 thriftgo 没预期的符号，报错位置离真正的成因很远。
// 故这里直接读盘断言：kitex_gen 里不许出现本域 dtm 分支包的引用。
func TestBranchPbStaysOutOfKitexGen(t *testing.T) {
	needle := "srv-order/internal/dtm"
	kitexGen := filepath.Join("..", "..", "..", "pkg", "kitex_gen")

	var hits []string
	err := filepath.WalkDir(kitexGen, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(b), needle) {
			rel, _ := filepath.Rel(kitexGen, path)
			hits = append(hits, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 kitex_gen 失败（守卫失效）：%v", err)
	}
	if len(hits) > 0 {
		t.Fatalf("kitex_gen 里出现了 dtm 分支包的引用 %v —— 两个生成面必须互不渗透", hits)
	}

	// 分支 pb 自己得是 grpc-go 生成码：BusiServer 内嵌 UnimplementedOrderBusiServer
	// 才能注册进 grpc（少这一行，protoc 新增方法时旧二进制会在运行期 panic 而不是编译期报错）。
	var _ interface {
		protos.OrderBusiServer
	} = (*BusiServer)(nil)
}

// ---- 断言辅助 ----

func assertSpan(t *testing.T, name, gid, branch string, compensation bool) {
	t.Helper()
	sp := findSpan(t, name)
	if !hasAttr(sp.Attributes(), "biz.dtm.gid", gid) {
		t.Errorf("%s 缺 biz.dtm.gid=%s：%v", name, gid, sp.Attributes())
	}
	if !hasAttr(sp.Attributes(), "biz.dtm.branch", branch) {
		t.Errorf("%s 缺 biz.dtm.branch=%s：%v", name, branch, sp.Attributes())
	}
	if !hasAttr(sp.Attributes(), "biz.compensation", compensation) {
		t.Errorf("%s 的 biz.compensation 应为 %v：%v", name, compensation, sp.Attributes())
	}
}

func findSpan(t *testing.T, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, sp := range spans.Ended() {
		if sp.Name() == name {
			return sp
		}
	}
	t.Fatalf("没找到 span %q，实际导出了：%v", name, spanNames())
	return nil
}

func spanNames() []string {
	var out []string
	for _, sp := range spans.Ended() {
		out = append(out, sp.Name())
	}
	return out
}

func hasAttr(attrs []attribute.KeyValue, key string, want any) bool {
	for _, a := range attrs {
		if string(a.Key) != key {
			continue
		}
		switch v := want.(type) {
		case string:
			if a.Value.AsString() == v {
				return true
			}
		case bool:
			if a.Value.AsBool() == v {
				return true
			}
		}
	}
	return false
}
