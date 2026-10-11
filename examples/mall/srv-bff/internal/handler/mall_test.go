package handler

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	otelcodes "go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/mall/pkg/auth"
	"github.com/gospacex/launchx/examples/mall/pkg/codes"
	stockpb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/stock"
)

// 本文件覆盖 bff 三条**决定账实是否相符**的分支：
//
//   - statusOf：领域码 → HTTP 状态码的映射（猜错状态码比 502 更难排查）
//   - settle：传输失败与业务拒绝的分流（分错就会把已建成的订单的库存放掉）
//   - compensateWith：补偿结果怎么落到 span（4005 记成 Error 会让
//     「补偿成功」和「补偿失败」在 Jaeger 上同色，按 Error 扫出来全是噪声）
//
// 断言一律看**录到的 span**，不看日志文本：日志文案改一次判据就失效，
// span 状态与属性是本仓的判据口径。

// TestCreateOrderRequestCarriesIdempotencyKey 钉住「bff 把幂等键传给了订单域」。
//
// 为什么用源码判据：bff 是**唯一**知道这个号的地方，它传没传，在运行期没有任何
// 可观测的副作用 —— 不传也不会报错，订单域只是自己另起一个号。这正是当前最危险
// 的失效形态：两端代码看着都对，重试一次就是一张新单。
//
// 反向探针：把 mall.go 里 CreateOrderReq 的 `OrderNo: orderNo` 一行注释掉，
// 本用例立刻红（实测过）。
func TestCreateOrderRequestCarriesIdempotencyKey(t *testing.T) {
	src, err := os.ReadFile("mall.go")
	if err != nil {
		t.Fatalf("读 mall.go: %v", err)
	}
	text := string(src)
	idx := strings.Index(text, "orderpb.CreateOrderReq{")
	if idx < 0 {
		t.Fatal("mall.go 里找不到 CreateOrderReq 的构造点（下单调用改结构了吗）")
	}
	end := idx + 1200
	if end > len(text) {
		end = len(text)
	}
	// 逐行比对而不是 `strings.Contains(片段, "OrderNo:")` —— 后者会命中**注释里**
	// 的同名串：实测把字段整行注释掉（`// OrderNo: orderNo,`）后判据照样绿，
	// 等于空壳。字段名只有在**行首**（去掉缩进后）才是真的字段。
	found := false
	for _, line := range strings.Split(text[idx:end], "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "OrderNo:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("CreateOrderReq 没带 OrderNo —— 幂等键没传给订单域，" +
			"重试（rpcx 的失败重试 / 上游重发）会重复下单")
	}
}

// TestStatusOfMapsDomainCodes 逐码核对 statusOf 的映射。
//
// 这张表是"漏映射 → 落到 default 502"这类缺陷的唯一防线：新增一个领域码
// 而忘了补映射，调用方会看到一个语义完全不对的 502。
func TestStatusOfMapsDomainCodes(t *testing.T) {
	cases := []struct {
		name string
		code int64
		want int
	}{
		{"参数非法 → 400", codes.CodeInvalidParam, http.StatusBadRequest},
		{"身份缺失 → 401", codes.CodeUnauthorized, http.StatusUnauthorized},
		{"通用不存在 → 404", codes.CodeNotFound, http.StatusNotFound},
		{"商品不存在 → 404", codes.CodeProductNotFound, http.StatusNotFound},
		{"商品已下架 → 404", codes.CodeProductOffShelf, http.StatusNotFound},
		{"订单不存在 → 404", codes.CodeOrderNotFound, http.StatusNotFound},
		{"库存记录不存在 → 404", codes.CodeStockNotFound, http.StatusNotFound},
		{"积分账户不存在 → 404", codes.CodePointsAccountNF, http.StatusNotFound},
		{"被限流 → 429", codes.CodeRateLimited, http.StatusTooManyRequests},
		{"并发下单 → 429", codes.CodeOrderConcurrent, http.StatusTooManyRequests},
		{"库存不足 → 409", codes.CodeStockInsufficient, http.StatusConflict},
		{"积分不足 → 409", codes.CodePointsInsufficient, http.StatusConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := statusOf(c.code); got != c.want {
				t.Fatalf("statusOf(%d) = %d, want %d", c.code, got, c.want)
			}
		})
	}
}

// TestStatusOfUnknownCodeIsBadGateway 未知码一律按下游故障透传 502。
func TestStatusOfUnknownCodeIsBadGateway(t *testing.T) {
	if got := statusOf(9999); got != http.StatusBadGateway {
		t.Fatalf("statusOf(9999) = %d, want %d", got, http.StatusBadGateway)
	}
}

// TestSettleDistinguishesUncertainFromRejected settle 的分流必须与补偿决策一致。
//
// 最关键的是最后一例：err != nil 时无论业务码是多少都必须判成
// outcomeUncertain —— 超时响应里的 Code 字段是零值，不是"下游接受了"。
func TestSettleDistinguishesUncertainFromRejected(t *testing.T) {
	boom := errors.New("dial tcp 10.0.0.1:9103: i/o timeout")
	cases := []struct {
		name string
		err  error
		code int64
		want outcome
	}{
		{"成功", nil, codes.CodeOK, outcomeOK},
		{"业务拒绝", nil, codes.CodeStockInsufficient, outcomeRejected},
		{"传输失败", boom, codes.CodeOK, outcomeUncertain},
		{"传输失败时业务码是零值，仍判未知", boom, 0, outcomeUncertain},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := settle(c.err, c.code); got != c.want {
				t.Fatalf("settle(%v, %d) = %s, want %s", c.err, c.code, got, c.want)
			}
		})
	}
}

// fakeReleaser 补偿路径的窄接口替身（只需"释放预占"这一个能力）。
type fakeReleaser struct {
	resp  *stockpb.ReleaseStockResp
	err   error
	calls int
}

func (f *fakeReleaser) ReleaseStock(ctx context.Context, req *stockpb.ReleaseStockReq) (*stockpb.ReleaseStockResp, error) {
	f.calls++
	return f.resp, f.err
}

// newTestHandler 构造一个 tracer 挂内存 recorder 的 handler，并取出 recorder
// 供断言（补偿的成败只体现在 span 上，没有别的可观测出口）。
func newTestHandler(t *testing.T) (*MallHandler, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return New(nil, tp.Tracer("bff-test"), nil, nil), sr
}

// endSpanOf 取出指定名字的已结束 span。
func endSpanOf(t *testing.T, sr *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range sr.Ended() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("没有名字为 %q 的已结束 span（共录到 %d 条）", name, len(sr.Ended()))
	return nil
}

// TestCompensateWithTreatsReservationNotFoundAsSuccess 4005 是幂等命中，按成功记。
//
// 4005 意味着这一单的预占已经不在了（已被释放/确认过），正是补偿想达到的
// 状态。把它记成 Error，会让"补偿成功"在 Jaeger 上和"补偿失败"同色。
func TestCompensateWithTreatsReservationNotFoundAsSuccess(t *testing.T) {
	h, sr := newTestHandler(t)
	d := &fakeReleaser{resp: &stockpb.ReleaseStockResp{
		Code:    codes.CodeStockReservationNF,
		Message: "reservation not found",
	}}

	h.compensateWith(context.Background(), spanOf(t, h), d, 7, 2, "NO-1")

	if d.calls != 1 {
		t.Fatalf("ReleaseStock 调用 %d 次，want 1", d.calls)
	}
	sp := endSpanOf(t, sr, "handler.bff.ReleaseStock.compensate")
	if sp.Status().Code == otelcodes.Error {
		t.Fatalf("4005 被记成了 Error（status=%v）—— 补偿成功会与失败同色", sp.Status())
	}
	if got := attrInt64(t, sp, "biz.compensation_code"); got != codes.CodeStockReservationNF {
		t.Fatalf("biz.compensation_code = %d, want %d", got, codes.CodeStockReservationNF)
	}
}

// TestCompensateWithMarksErrorOnTransportFailure 补偿请求本身失败必须留 Error。
//
// 补偿失败意味着"这单的库存可能还冻着"，善后属于对账域，但对账要能检索到。
func TestCompensateWithMarksErrorOnTransportFailure(t *testing.T) {
	h, sr := newTestHandler(t)
	d := &fakeReleaser{err: errors.New("connection refused")}

	h.compensateWith(context.Background(), spanOf(t, h), d, 7, 2, "NO-2")

	sp := endSpanOf(t, sr, "handler.bff.ReleaseStock.compensate")
	if sp.Status().Code != otelcodes.Error {
		t.Fatalf("传输失败未标记 Error（status=%v）—— 对账检索不到补偿失败", sp.Status())
	}
}

// TestCompensateWithMarksErrorOnDomainFailure 领域码非 0 也是补偿失败。
func TestCompensateWithMarksErrorOnDomainFailure(t *testing.T) {
	h, sr := newTestHandler(t)
	d := &fakeReleaser{resp: &stockpb.ReleaseStockResp{
		Code:    codes.CodeStockReserveFailed,
		Message: "release failed",
	}}

	h.compensateWith(context.Background(), spanOf(t, h), d, 7, 2, "NO-3")

	sp := endSpanOf(t, sr, "handler.bff.ReleaseStock.compensate")
	if sp.Status().Code != otelcodes.Error {
		t.Fatalf("领域码 %d 未标记 Error（status=%v）", codes.CodeStockReserveFailed, sp.Status())
	}
}

// TestIdentityOrPrefersIdentityOverFallback 有身份时一律以身份为准。
//
// 反过来（信请求参数）就等于把"我是谁"交给调用方声明，鉴权白做。
// 第三例是关键边界：UserID=0 不是"0 号用户"，而是"没有身份"。
func TestIdentityOrPrefersIdentityOverFallback(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		want int64
	}{
		{"无身份 → 回落", context.Background(), 0},
		{"有身份 → 身份优先", auth.WithIdentity(context.Background(), auth.Identity{UserID: 42}), 42},
		{"UserID=0 视为无身份", auth.WithIdentity(context.Background(), auth.Identity{UserID: 0}), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := identityOr(c.ctx, 0); got != c.want {
				t.Fatalf("identityOr() = %d, want %d", got, c.want)
			}
		})
	}
}

// spanOf 取一个 recording span 传给 compensateWith（它只用作父 span）。
func spanOf(t *testing.T, h *MallHandler) trace.Span {
	t.Helper()
	_, sp := h.tracer.Start(context.Background(), "test-parent")
	return sp
}

// attrInt64 从 span 上取整型属性。
func attrInt64(t *testing.T, sp sdktrace.ReadOnlySpan, key string) int64 {
	t.Helper()
	for _, a := range sp.Attributes() {
		if string(a.Key) == key {
			return a.Value.AsInt64()
		}
	}
	t.Fatalf("span %q 上没有属性 %q", sp.Name(), key)
	return 0
}
