package dtm

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/dtm-labs/client/dtmcli"
	dtmimp "github.com/dtm-labs/client/dtmcli/dtmimp"
	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/gospacex/dtmx"
	bizcodes "github.com/gospacex/launchx/examples/micro/pkg/codes"
	pointsbusi "github.com/gospacex/launchx/examples/micro/srv-points/internal/dtm/protos"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/model"
)

// 本文件锁分支面（busi handler）的三条协议事实。它们都不是「风格偏好」，而是
// 错了就会让整笔全局事务静默挂死或重复记账：
//
//  1. **分支 URL ＝ 全方法名**：dtm 只拿 URL 去调，不做名字映射。方法名与
//     proto package 任一侧改动，dtm 侧的表现是「该分支从未被调用」，事务一直
//     重试到超时。
//  2. **失败靠 gRPC status，不靠响应体**：dtm 对分支调用用的是 raw codec，
//     响应字节被它整个丢进一个 `*[]byte` 之后**从不解析**；它只认
//     grpccodes.Aborted（业务失败 → Saga 立即逆序补偿 / TCC 不再等）与
//     grpccodes.Internal（基础失败 → 按退避重试）。把「FAILURE」只写进
//     resp.Message 是无效的。
//  3. **补偿段走新根 trace**：dtm 异步发起补偿时载荷里没有 traceparent，
//     此时若伪造一个同 trace ID 的 span，就是一条永远不存在的父链。

func TestMain(m *testing.M) {
	dtmx.SetBarrierTableName("barrier")
	os.Exit(m.Run())
}

// ---- 事实 1：全方法名必须与编排侧分支 URL 一致 ----

// TestBranchMethodsUseDtmFullMethodNames 钉住 proto package / 服务名 / 方法名。
//
// 反向探针：把 busi.proto 的 package 改成 dtmbranch.point，本用例必红。
func TestBranchMethodsUseDtmFullMethodNames(t *testing.T) {
	want := map[string]string{
		"Debit":         pointsbusi.PointsBusi_Debit_FullMethodName,
		"Refund":        pointsbusi.PointsBusi_Refund_FullMethodName,
		"TryFreeze":     pointsbusi.PointsBusi_TryFreeze_FullMethodName,
		"ConfirmFreeze": pointsbusi.PointsBusi_ConfirmFreeze_FullMethodName,
		"CancelFreeze":  pointsbusi.PointsBusi_CancelFreeze_FullMethodName,
	}
	for method, full := range want {
		if expected := "/dtmbranch.points.PointsBusi/" + method; full != expected {
			t.Errorf("%s 的全方法名 = %q，编排侧会用 %q 提交分支 URL —— 两者不一致时 dtm 永远调不到这个分支", method, full, expected)
		}
	}
	// 再从服务描述符取一遍：只比对生成常量的话，将来有人改了注册的方法集而
	// 忘了同步常量，这里看不出来。
	desc := pointsbusi.PointsBusi_ServiceDesc
	if desc.ServiceName != "dtmbranch.points.PointsBusi" {
		t.Fatalf("注册的服务名 = %q，want dtmbranch.points.PointsBusi", desc.ServiceName)
	}
	registered := map[string]bool{}
	for _, m := range desc.Methods {
		registered[m.MethodName] = true
	}
	for method := range want {
		if !registered[method] {
			t.Errorf("服务描述符里没有方法 %s —— dtm 按 URL 调会得到 Unimplemented", method)
		}
	}
}

// ---- 事实 2：失败语义走 gRPC status ----

// stubService 是 BranchService 的测试桩：让「协议翻译」这一层能被单独验。
type stubService struct {
	err  error
	seen []model.DtmBiz
}

func (s *stubService) record(b model.DtmBiz) error {
	s.seen = append(s.seen, b)
	return s.err
}
func (s *stubService) DtmDebit(_ context.Context, b model.DtmBiz) error         { return s.record(b) }
func (s *stubService) DtmRefund(_ context.Context, b model.DtmBiz) error        { return s.record(b) }
func (s *stubService) DtmTryFreeze(_ context.Context, b model.DtmBiz) error     { return s.record(b) }
func (s *stubService) DtmConfirmFreeze(_ context.Context, b model.DtmBiz) error { return s.record(b) }
func (s *stubService) DtmCancelFreeze(_ context.Context, b model.DtmBiz) error  { return s.record(b) }

func reqWithBiz(t *testing.T, bizJSON string) *pointsbusi.PointsBusiReq {
	t.Helper()
	if bizJSON == "" {
		return &pointsbusi.PointsBusiReq{Biz: `{"user_id":1001,"cost":30,"order_no":"SO-1"}`}
	}
	return &pointsbusi.PointsBusiReq{Biz: bizJSON}
}

// TestBusinessFailureMapsToAborted：带官方哨兵的业务失败必须回 Aborted。
//
// Aborted 对 dtm 的意义（client/dtmgrpc/type.go 的 GrpcError2DtmError）：
// 业务失败 → Saga 立即逆序补偿、TCC 停止等待；代码换成 Internal 就变成
// 「按退避重试」，积分不足会被无限重试。
func TestBusinessFailureMapsToAborted(t *testing.T) {
	svc := &stubService{err: dtmx.BusinessFailure(
		errors.Join(bizcodes.New(bizcodes.CodePointsInsufficient, "points insufficient"), errors.New("balance 10 < cost 30")))}
	srv := NewBusiServer(svc, false, logx.GetGlobal())

	_, err := srv.Debit(t.Context(), reqWithBiz(t, ""))
	st, ok := grpcstatus.FromError(err)
	if !ok {
		t.Fatalf("业务失败必须以 gRPC 错误上抛（dtm 只读 status），实际 err=%v", err)
	}
	if st.Code() != grpccodes.Aborted {
		t.Fatalf("业务失败 status code = %v，want Aborted（= 官方 FAILURE 的 grpc 形态）", st.Code())
	}
	if !strings.Contains(st.Message(), dtmcli.ErrFailure.Error()) {
		t.Fatalf("status message 应带上官方失败哨兵文案便于排障，实际 %q", st.Message())
	}
}

// TestBaseFailureMapsToInternal：数据库类失败必须回 Internal 让 dtm 重试。
//
// 反向探针：把它改成 Aborted，本用例必红 —— 一笔网络抖动造成的失败会被永久
// 判成「积分不够」，用户的积分再也回不来。
func TestBaseFailureMapsToInternal(t *testing.T) {
	svc := &stubService{err: errors.New("dial tcp 10.0.0.7:3306: i/o timeout")}
	srv := NewBusiServer(svc, false, logx.GetGlobal())

	_, err := srv.Debit(t.Context(), reqWithBiz(t, ""))
	st, _ := grpcstatus.FromError(err)
	if st.Code() != grpccodes.Internal {
		t.Fatalf("基础失败 status code = %v，want Internal（= 可重试）", st.Code())
	}
}

// TestFailInjectReturnsAborted：编排侧点火的失败注入必须走业务失败语义，
// 且只命中被点名的那一段。
//
// 它是演示「Saga 逆序补偿 / TCC 走 Cancel」的唯一开关：返回 Internal 的话
// dtm 会一直重试到超时，用户看不到补偿被触发。
func TestFailInjectReturnsAborted(t *testing.T) {
	svc := &stubService{}
	srv := NewBusiServer(svc, false, logx.GetGlobal())

	req := reqWithBiz(t, "")
	req.FailInject = "Refund"

	resp, err := srv.Debit(t.Context(), req)
	if err != nil {
		t.Fatalf("未点名的分支不该失败：%v", err)
	}
	if resp.GetCode() != bizcodes.CodeOK {
		t.Fatalf("未点名的分支 code = %d，want 0", resp.GetCode())
	}
	if len(svc.seen) != 1 {
		t.Fatalf("未点名的分支应正常调 service，实际调用 %d 次", len(svc.seen))
	}

	resp, err = srv.Refund(t.Context(), req)
	st, _ := grpcstatus.FromError(err)
	if st.Code() != grpccodes.Aborted {
		t.Fatalf("被点名的分支 status = %v，want Aborted", st.Code())
	}
	if resp.GetCode() == bizcodes.CodeOK {
		t.Fatal("被点名的分支响应码不该是 0")
	}
	if len(svc.seen) != 1 {
		t.Fatalf("被点名的分支不得落库（service 不该被调用），实际调用 %d 次", len(svc.seen))
	}
}

// TestMalformedBizIsBusinessFailure：载荷解析失败也按业务失败。
//
// 编排侧拼错 JSON 时重试一万次也不会变好；而若报成 Internal，dtm 会把这笔
// 全局事务重试到超时 —— 期间其它域的分支一起悬着。
func TestMalformedBizIsBusinessFailure(t *testing.T) {
	svc := &stubService{}
	srv := NewBusiServer(svc, false, logx.GetGlobal())

	for _, bad := range []string{"", "{not json", `{"user_id":"abc"}`} {
		_, err := srv.Debit(t.Context(), &pointsbusi.PointsBusiReq{Biz: bad})
		st, _ := grpcstatus.FromError(err)
		if st.Code() != grpccodes.Aborted {
			t.Fatalf("载荷 %q：status = %v，want Aborted（重试不会变好，须立刻失败）", bad, st.Code())
		}
	}
	if len(svc.seen) != 0 {
		t.Fatalf("载荷解析失败时不得调 service，实际调用 %d 次", len(svc.seen))
	}
}

// ---- 事实 3：span 口径 ----

// newTracer 装一个内存 exporter（span 断言必须落在真实导出结果上，
// 靠读代码推断「它应该会打这个 tag」不算数）。
func newTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(trace.NewNoopTracerProvider()) })
	return exp
}

// dtmMetaCtx 造一条带 dtm-* metadata 的上下文（真实回调一定有，见 e2e 用例）。
func dtmMetaCtx(t *testing.T, gid, branchID, op string) context.Context {
	t.Helper()
	return metadata.NewIncomingContext(t.Context(), metadata.Pairs(
		"dtm-gid", gid,
		"dtm-trans_type", "saga",
		"dtm-branch_id", branchID,
		"dtm-op", op,
	))
}

// TestCompensationStartsNewRootTrace：补偿段没有 traceparent，必须开新根 trace。
//
// 判据是「导出结果的 span 没有 parent」：伪造同 ID 孤儿的写法会让这里出现
// 一个不存在的父，Jaeger 上表现为一条断链的短 trace，比新根更难查。
func TestCompensationStartsNewRootTrace(t *testing.T) {
	exp := newTracer(t)
	srv := NewBusiServer(&stubService{}, true, logx.GetGlobal())

	// 补偿段：无 traceparent —— dtm 异步发起补偿时载荷里就是没有它。
	ctx := dtmMetaCtx(t, "gid-comp", "01", "compensate")
	if _, err := srv.Refund(ctx, reqWithBiz(t, "")); err != nil {
		t.Fatalf("Refund: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("应恰好导出 1 个 span（rpc.dtm.points.Refund），实际 %d", len(spans))
	}
	span := spans[0]
	if span.Name != "rpc.dtm.points.Refund" {
		t.Fatalf("span 名 = %q，want rpc.dtm.points.Refund", span.Name)
	}
	if span.Parent.HasSpanID() {
		t.Fatalf("补偿段不该有父 span（新根 trace），实际 parent=%v —— 伪造同 ID 孤儿的写法会栽在这里", span.Parent)
	}
	wantTags := map[string]string{
		"biz.dtm.gid":      "gid-comp",
		"biz.dtm.branch":   "01",
		"biz.compensation": "true",
		"biz.dtm.trace":    "new_root",
	}
	got := map[string]string{}
	for _, kv := range span.Attributes {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	for k, v := range wantTags {
		if got[k] != v {
			t.Errorf("span 缺标签 %s=%s（实际 %q）", k, v, got[k])
		}
	}
}

// TestForwardBranchContinuesCallerTrace：正向段带 traceparent 时必须续链。
//
// dtm server 只按 URL 回调、不透传自定义 metadata，正向链能挂回同一条 trace
// 全靠编排侧把 traceparent 编进载荷（见 dtmx.InjectKey）。
func TestForwardBranchContinuesCallerTrace(t *testing.T) {
	exp := newTracer(t)
	srv := NewBusiServer(&stubService{}, true, logx.GetGlobal())

	// 先在一条本地 trace 里造出父 span，取它的 trace id 拼成 traceparent。
	parentCtx, parent := otel.Tracer("bff").Start(t.Context(), "bff.submit")
	parentTraceID := parent.SpanContext().TraceID().String()
	tp := "00-" + parentTraceID + "-" + parent.SpanContext().SpanID().String() + "-01"
	parent.End()

	req := reqWithBiz(t, "")
	req.Traceparent = tp
	if _, err := srv.Debit(parentCtx, req); err != nil {
		t.Fatalf("Debit: %v", err)
	}
	// 导出结果里还有那个父 span（同一个 exporter），按名字挑出分支 span。
	var branchSpan tracetest.SpanStub
	var found bool
	for _, s := range exp.GetSpans() {
		if s.Name == "rpc.dtm.points.Debit" {
			branchSpan, found = s, true
		}
	}
	if !found {
		t.Fatalf("没导出 rpc.dtm.points.Debit span，实际导出 %d 个 span", len(exp.GetSpans()))
	}
	if got := branchSpan.SpanContext.TraceID().String(); got != parentTraceID {
		t.Fatalf("分支 span 的 trace id = %s，应与编排侧一致（%s）—— 续链没生效，这条分支在 Jaeger 上是另一条链", got, parentTraceID)
	}
	// 正向段不算补偿
	for _, kv := range branchSpan.Attributes {
		if string(kv.Key) == "biz.compensation" && kv.Value.Emit() != "false" {
			t.Fatalf("Debit 是正向段，biz.compensation 应为 false，实际 %s", kv.Value.Emit())
		}
	}
}

// ---- 装配口径：地址来自 config、barrier 表名、不起 consul ----

// TestConfigDeclaresBranchListenAddr：地址一律进 config，Go 代码零硬编码。
func TestConfigDeclaresBranchListenAddr(t *testing.T) {
	cfg, err := loadRepoConfig(t)
	if err != nil {
		t.Fatalf("读 config.yaml 失败：%v", err)
	}
	addr := cfg.GetString(ListenAddrKey)
	if addr == "" {
		t.Fatalf("config.yaml 缺 %s —— 分支面地址不许回落到代码里的默认值（换部署形态就得改代码）", ListenAddrKey)
	}
	if addr != "127.0.0.1:9044" {
		t.Fatalf("%s = %q，本机默认应为 127.0.0.1:9044（与其它域的 904x 段连号）", ListenAddrKey, addr)
	}
}

// TestSetupSourceHasNoHardcodedAddressAndNoConsul：源码级守卫。
//
// 两条一起看：地址字面量一旦出现在 Go 源码里，换部署形态就得改代码；而分支面
// 调不通时若还注册进 consul，别的服务会**调用**这个端口并拿到一堆
// Unimplemented —— 把「分支面没起来」变成「一堆下游报 500」。
func TestSetupSourceHasNoHardcodedAddressAndNoConsul(t *testing.T) {
	lits := stringLiterals(t, "setup.go")
	for _, l := range lits {
		if strings.Contains(l, "127.0.0.1") || strings.Contains(l, "localhost") || strings.Contains(l, ":9044") {
			t.Errorf("setup.go 出现地址字面量 %q —— 地址必须来自 config 的 %s", l, ListenAddrKey)
		}
		if strings.Contains(l, "Register") || strings.Contains(l, "Deregister") {
			t.Errorf("setup.go 出现注册中心调用 %q —— 分支面只被 dtm 回调，不注册进 consul", l)
		}
	}
}

// TestSetupSetsBarrierTableName：进程级全局量必须在装配期设成本域表名。
//
// 反向探针：把 applyProcessGlobals 里的 SetBarrierTableName 去掉，本用例必红 ——
// 而真机上的表现是 barrier 行插进不存在的 dtm_barrier 库、每个分支都失败。
func TestSetupSetsBarrierTableName(t *testing.T) {
	prev := dtmimp.BarrierTableName
	t.Cleanup(func() { dtmx.SetBarrierTableName(prev) })
	dtmimp.BarrierTableName = "dtm_barrier.barrier" // 官方默认，模拟「忘了设」

	applyProcessGlobals()

	if dtmimp.BarrierTableName != "barrier" {
		t.Fatalf("装配后 dtm 官方 barrier 表名 = %q，want barrier（本域业务库内，见 schema-micro.sql）", dtmimp.BarrierTableName)
	}
}

// ---- 源码静态守卫工具 ----

// stringLiterals 取本包某个文件里的全部字符串字面量。
func stringLiterals(t *testing.T, file string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s 失败：%v（守卫失效）", file, err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if ok && lit.Kind == token.STRING {
			out = append(out, lit.Value)
		}
		return true
	})
	return out
}
