// 分支 handler 的单测。
//
// 本文件只测**协议层**该守的东西：全方法名与 dtm 回调 URL 是否一致、traceparent
// 是否续链、失败注入是否点火、以及「业务失败走 codes.Aborted、基础设施故障不
// 走」这条 dtm 侧的分流（dtmgrpc.DtmError2GrpcError：Aborted = 业务失败不重试，
// 其余 status = 基础失败按退避重试 —— 弄反了就是一次库存不足把整条 Saga 判死）。
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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"gorm.io/gorm"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/dbmock"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/dtm/protos"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/model"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/repository"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/service"
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
	// barrier 表名必须设成本部署用的 barrier（落在业务库内），而不是官方默认的
	// dtm_barrier.barrier —— 后者是独立库，与业务写不同库，CallWithDB 在业务连接
	// 的事务里插不进那张表。app/setup.go 的 dtmx.Setup 做的是同一件事。
	//
	// 不设的后果很隐蔽：正向分支照样跑通（barrier 行插不进去也不影响它执行业务写），
	// 而补偿/取消分支会被官方语义判成「空补偿」直接返回 —— 于是正向账扣了、
	// 补偿账没补，两边都报成功。
	dtmx.SetBarrierTableName("barrier")
	code := m.Run()
	_ = tp.Shutdown(context.Background())
	os.Exit(code)
}

const testTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func newTestServer(t *testing.T) (*BusiServer, *dbmock.DB) {
	t.Helper()
	db := dbmock.New(t)
	gdb := &gorm.DB{Config: &gorm.Config{ConnPool: db.SQL()}}
	svc := service.NewStockService(repository.NewStockRepository(gdb, true), nil, nil, nil, true, nil)
	return NewBusiServer(svc, true, nil), db
}

func req(t *testing.T, branch, failInject string, cmd model.DtmStockCmd) *protos.StockBusiReq {
	t.Helper()
	b, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal biz: %v", err)
	}
	return &protos.StockBusiReq{Biz: string(b), FailInject: failInject}
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

func bizCmd() model.DtmStockCmd {
	return model.DtmStockCmd{OrderNo: "SO-DTM-1", ProductID: 42, Quantity: 3, UserID: 9}
}

// sagaBranchID / tccBranchID：一个 saga 步骤 / 一对 tcc 分支在 dtm server 里的
// branch_id。**正向与逆向共用同一个**（dtmsvr/trans_type_saga.go 的 GenBranches
// 给同一步的 action/compensate 同一个 "%02d"；TCC 见 api.go 的 svcRegisterBranch
// 复制同一个 branch）。barrier 判空补偿靠的正是这个共用 id，逆序用例不先跑正向
// 就只会测到空补偿。
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
	call  func(ctx context.Context, r *protos.StockBusiReq) (*protos.StockBusiResp, error)
	// compensation 是否逆向分支（Release / CancelFreeze）
	compensation bool
	// prereq 前置正向分支；nil 表示自身即正向
	prereq *branchCall
}

// branchCalls 把五个方法收成「分支名 → 调用形态」，便于按同一组断言逐个跑。
func branchCalls(s *BusiServer) map[string]branchCall {
	m := map[string]*branchCall{
		model.DtmDeductBranch: {method: "Deduct", branch: model.DtmDeductBranch, branchID: sagaBranchID,
			op: "action", guard: "available = available - ?", call: s.Deduct},
		model.DtmTryFreezeBranch: {method: "TryFreeze", branch: model.DtmTryFreezeBranch, branchID: tccBranchID,
			op: "try", guard: "frozen = frozen + ?", call: s.TryFreeze},
		model.DtmConfirmBranch: {method: "ConfirmFreeze", branch: model.DtmConfirmBranch, branchID: tccBranchID,
			op: "confirm", guard: "frozen = frozen - ?", call: s.ConfirmFreeze},
	}
	// 逆向分支的 prereq 必须与它们同 branchID：barrier 就是靠这个共用 id 去探
	// 「同一步的正向行在不在」，探不到就判空补偿、业务写根本不会被调用。
	m[model.DtmReleaseBranch] = &branchCall{method: "Release", branch: model.DtmReleaseBranch, branchID: sagaBranchID,
		op: "compensate", guard: "available = available + ?", call: s.Release,
		compensation: true, prereq: m[model.DtmDeductBranch]}
	m[model.DtmCancelFreeze] = &branchCall{method: "CancelFreeze", branch: model.DtmCancelFreeze, branchID: tccBranchID,
		op: "cancel", guard: "available = available + ?", call: s.CancelFreeze,
		compensation: true, prereq: m[model.DtmTryFreezeBranch]}
	out := map[string]branchCall{}
	for k, v := range m {
		out[k] = *v
	}
	return out
}

// branchNames 五个分支名（= busi.proto 里的方法名，编排侧按它点火）。
func branchNames() []string {
	return []string{
		model.DtmDeductBranch,
		model.DtmReleaseBranch,
		model.DtmTryFreezeBranch,
		model.DtmConfirmBranch,
		model.DtmCancelFreeze,
	}
}

// arrive 让 tc 到达（逆序分支先跑它的正向），并把执行记录归零，使断言只看见这一次。
func arrive(t *testing.T, db *dbmock.DB, tc *branchCall, gid string) {
	t.Helper()
	if tc.prereq == nil {
		return
	}
	pr := *tc.prereq
	if _, err := pr.call(dtmCtx(gid, pr.branchID, pr.op), req(t, pr.branch, "", bizCmd())); err != nil {
		t.Fatalf("前置正向分支 %s 应成功：%v", pr.branch, err)
	}
	db.ResetLog()
}

// TestFullMethodNamesMatchDtmCallbackURL 全方法名与编排侧拼的回调 URL 必须一致。
//
// dtm server 只按 URL 回调，一个字符对不上就是「分支 404，全局事务永远挂着」，
// 而本地没有任何迹象会指向 proto package 写错了。
//
// 反向探针：把 busi.proto 的 package 从 dtmbranch.stock 改成别的，本用例必红。
func TestFullMethodNamesMatchDtmCallbackURL(t *testing.T) {
	want := map[string]string{
		"Deduct":        "/dtmbranch.stock.StockBusi/Deduct",
		"Release":       "/dtmbranch.stock.StockBusi/Release",
		"TryFreeze":     "/dtmbranch.stock.StockBusi/TryFreeze",
		"ConfirmFreeze": "/dtmbranch.stock.StockBusi/ConfirmFreeze",
		"CancelFreeze":  "/dtmbranch.stock.StockBusi/CancelFreeze",
	}
	got := map[string]string{
		"Deduct":        protos.StockBusi_Deduct_FullMethodName,
		"Release":       protos.StockBusi_Release_FullMethodName,
		"TryFreeze":     protos.StockBusi_TryFreeze_FullMethodName,
		"ConfirmFreeze": protos.StockBusi_ConfirmFreeze_FullMethodName,
		"CancelFreeze":  protos.StockBusi_CancelFreeze_FullMethodName,
	}
	for m, w := range want {
		if got[m] != w {
			t.Errorf("方法 %s 的全名 = %q，编排侧按 %q 回调", m, got[m], w)
		}
	}
	if protos.StockBusi_ServiceDesc.ServiceName != "dtmbranch.stock.StockBusi" {
		t.Errorf("服务名 = %q", protos.StockBusi_ServiceDesc.ServiceName)
	}
	if n := len(protos.StockBusi_ServiceDesc.Methods); n != len(want) {
		t.Errorf("注册方法数 = %d, want %d（少一个编排侧就会有一条分支 404）", n, len(want))
	}
	// 分支名（model 常量）必须与 proto 方法名逐字相等：编排侧的 fail_inject 按
	// 分支名点火、span 名按方法名开、两处拼错就是「点火失败、正向全绿」。
	branches := map[string]string{
		"Deduct":        model.DtmDeductBranch,
		"Release":       model.DtmReleaseBranch,
		"TryFreeze":     model.DtmTryFreezeBranch,
		"ConfirmFreeze": model.DtmConfirmBranch,
		"CancelFreeze":  model.DtmCancelFreeze,
	}
	for method, branch := range branches {
		if method != branch {
			t.Errorf("方法名 %q 与分支名 %q 不一致（fail_inject 点不着它）", method, branch)
		}
	}
}

// TestBranchSucceeds 正常路径：resp.Code=0、grpc 无 error、库存真的动了。
func TestBranchSucceeds(t *testing.T) {
	for _, name := range branchNames() {
		t.Run(name, func(t *testing.T) {
			spans.Reset()
			const gid = "gid-ok"
			s, db := newTestServer(t)
			tc := branchCalls(s)[name]
			arrive(t, db, &tc, gid)
			resp, err := tc.call(dtmCtx(gid, tc.branchID, tc.op), req(t, tc.branch, "", bizCmd()))
			if err != nil {
				t.Fatalf("分支应成功：%v", err)
			}
			if resp.GetCode() != 0 {
				t.Fatalf("code = %d, want 0（message=%q）", resp.GetCode(), resp.GetMessage())
			}
			if db.ExecCount(tc.guard) != 1 {
				t.Fatalf("库存未按分支语义变动，缺 %q：%v", tc.guard, db.Stmts())
			}
			assertSpan(t, "rpc.dtm.stock."+tc.method, gid, tc.branchID, tc.compensation)
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
			_, err := tc.call(dtmCtx(gid, tc.branchID, tc.op),
				req(t, tc.branch, tc.branch, bizCmd()))
			if err == nil {
				t.Fatal("命中失败注入必须返错误，否则点火脚本跑完正向全绿、永远等不到补偿")
			}
			if got := status.Code(err); got != codes.Aborted {
				t.Fatalf("grpc status = %s, want Aborted（dtm 据此判业务失败并立即转补偿）", got)
			}
			if db.ExecCount("update stocks") != 0 {
				t.Fatal("失败注入时不得写库存")
			}

			// 注入的是别的分支名时本分支必须正常跑通 —— 否则一个错字会让整条链全挂。
			db.ResetLog()
			_, err = tc.call(dtmCtx(gid, tc.branchID, tc.op),
				req(t, tc.branch, tc.branch+"-typo", bizCmd()))
			if err != nil {
				t.Fatalf("注入别的分支不应影响本分支：%v", err)
			}
		})
	}
}

// TestDeficitIsAbortedButInfraIsNot 分流：业务不足 → Aborted（不重试，转补偿）；
// 句柄故障 → 非 Aborted（dtm 退避重试）。
//
// 反向探针：把 busiServer.branch 里的 DtmError2GrpcError 换成直接 return err，
// 本用例必红（Aborted 变成 Unknown，dtm 会把「卖完了」当故障无限重试）。
func TestDeficitIsAbortedButInfraIsNot(t *testing.T) {
	s, db := newTestServer(t)
	db.SetAffected("update stocks", 0)

	_, err := s.Deduct(dtmCtx("gid-def", sagaBranchID, "action"),
		req(t, model.DtmDeductBranch, "", bizCmd()))
	if err == nil {
		t.Fatal("库存不足必须报错")
	}
	if got := status.Code(err); got != codes.Aborted {
		t.Fatalf("库存不足的 status = %s, want Aborted", got)
	}

	infra := service.NewStockService(repository.NewStockRepository(&gorm.DB{Config: &gorm.Config{}}, false),
		nil, nil, nil, true, nil)
	bad := NewBusiServer(infra, true, nil)
	_, err = bad.Deduct(dtmCtx("gid-infra", sagaBranchID, "action"),
		req(t, model.DtmDeductBranch, "", bizCmd()))
	if err == nil {
		t.Fatal("句柄不可用必须报错")
	}
	if got := status.Code(err); got == codes.Aborted {
		t.Fatalf("基础设施故障被报成业务失败（Aborted），dtm 将不重试而是转补偿：%v", err)
	}
}

// TestMalformedBizIsBusinessFailure 载荷里的 biz 不是合法 JSON → 业务失败。
func TestMalformedBizIsBusinessFailure(t *testing.T) {
	s, db := newTestServer(t)
	_, err := s.Deduct(dtmCtx("gid-bad", sagaBranchID, "action"),
		&protos.StockBusiReq{Biz: "{not json"})
	if err == nil {
		t.Fatal("坏载荷必须报错 —— 带着空 product_id 落库才是真事故")
	}
	if got := status.Code(err); got != codes.Aborted {
		t.Fatalf("坏载荷是业务失败（编排侧 payload 生成有问题，重试无用），status = %s", got)
	}
	if db.ExecCount("update stocks") != 0 {
		t.Fatal("坏载荷不得写库")
	}
}

// TestMissingBarrierMetadataIsNotSwallowed 没有 dtm metadata 时必须报错，
// 且不得当成业务失败（否则一次装配错位会伪装成「库存不足」）。
func TestMissingBarrierMetadataIsNotSwallowed(t *testing.T) {
	s, db := newTestServer(t)
	_, err := s.Deduct(context.Background(), req(t, model.DtmDeductBranch, "", bizCmd()))
	if err == nil {
		t.Fatal("缺 dtm metadata 必须报错，不能无屏障落库")
	}
	if !strings.Contains(err.Error(), "barrier") {
		t.Fatalf("错误文案须点明 barrier，实际：%v", err)
	}
	if status.Code(err) == codes.Aborted {
		t.Fatal("缺 barrier 是装配错位（可重试），不得报成业务失败")
	}
	if db.ExecCount("update stocks") != 0 {
		t.Fatal("缺 barrier 时不得写库")
	}
}

// TestForwardBranchJoinsCallerTrace 正向分支必须挂回编排侧那条 trace。
func TestForwardBranchJoinsCallerTrace(t *testing.T) {
	s, _ := newTestServer(t)
	spans.Reset()

	r := req(t, model.DtmDeductBranch, "", bizCmd())
	r.Traceparent = testTraceparent
	if _, err := s.Deduct(dtmCtx("gid-tr", sagaBranchID, "action"), r); err != nil {
		t.Fatalf("正向分支应成功：%v", err)
	}
	got := findSpan(t, "rpc.dtm.stock.Deduct")
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
	s, _ := newTestServer(t)

	calls := branchCalls(s)
	for _, name := range []string{model.DtmReleaseBranch, model.DtmCancelFreeze} {
		tc := calls[name]
		t.Run(tc.branch, func(t *testing.T) {
			const gid = "gid-comp"
			spans.Reset()
			s2, db2 := newTestServer(t)
			tc2 := branchCalls(s2)[name]
			arrive(t, db2, &tc2, gid)
			// 注意载荷里**没有** traceparent —— 这正是 dtm 异步回调的真实形状。
			if _, err := tc2.call(dtmCtx(gid, tc2.branchID, tc2.op),
				req(t, tc2.branch, "", bizCmd())); err != nil {
				t.Fatalf("补偿分支应成功：%v", err)
			}
			got := findSpan(t, "rpc.dtm.stock."+tc2.method)
			if got.Parent().IsValid() {
				t.Fatalf("补偿分支不应挂到任何父 span 上（载荷无 traceparent，凭空造同 ID 孤儿 span 是自欺）")
			}
			if got.SpanContext().TraceID().String() == "4bf92f3577b34da6a3ce929d0e0e4736" {
				t.Fatal("补偿分支复用了上游 trace id —— 那正是「伪造同 ID 孤儿 span」")
			}
			if !hasAttr(got.Attributes(), "biz.dtm.gid", gid) {
				t.Fatalf("补偿分支必须带 biz.dtm.gid 才能与正向链关联：%v", got.Attributes())
			}
			if !hasAttr(got.Attributes(), "biz.dtm.branch", tc2.branchID) {
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
			if _, err := tc.call(dtmCtx(gid, tc.branchID, tc.op), req(t, tc.branch, "", bizCmd())); err != nil {
				t.Fatalf("分支应成功：%v", err)
			}
			assertSpan(t, "rpc.dtm.stock."+tc.method, gid, tc.branchID, tc.compensation)
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
	needle := "srv-stock/internal/dtm"
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

	// 分支 pb 自己得是 grpc-go 生成码：BusiServer 内嵌 UnimplementedStockBusiServer
	// 才能注册进 grpc（少这一行，protoc 新增方法时旧二进制会在运行期 panic 而不是编译期报错）。
	var _ interface {
		protos.StockBusiServer
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
