package rpc

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/gospacex/launchx/examples/micro/pkg/auth"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	pointspb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/points"
	"github.com/gospacex/launchx/examples/micro/pkg/routing"
)

// 本文件锁两条互不相干的纪律，都用"被鉴权拒"这条路径驱动
// —— 它在触达 handler 之前就返回了，因此不需要真的 service / repository：
//
//  1. trace.enabled=false 时本层**不许**开 span（否则它是无父根 span，
//     会导出成一条只有一节的孤儿 trace）；
//  2. 被鉴权拒必须回 1004 而不是 1005（限流才该回 1005）。
//
// 判据与 srv-order 的同名用例逐字同构：四个域服的 rpc 层形态一致，
// 谁的用例少一条，那条纪律在谁身上就是没被锁住的。

func captureSpans(t *testing.T) func() tracetest.SpanStubs {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return exp.GetSpans
}

// rejectingRouter 装配一张只带鉴权闸门的路由表：ctx 里没有身份 → 一律拒。
func rejectingRouter(traceOn bool) *routing.Router {
	return routing.NewRouter("points", traceOn, nil, routing.Route{
		Method:      "Earn",
		Kind:        routing.KindWrite,
		Middlewares: []routing.Middleware{routing.Authenticate(auth.Gate{}, nil)},
	})
}

func TestRejectedCallProducesNoSpanWhenTraceDisabled(t *testing.T) {
	spans := captureSpans(t)
	s := &Server{Router: rejectingRouter(false), TraceOn: false}

	resp, err := s.Earn(context.Background(), &pointspb.EarnReq{UserId: 1001, OrderNo: "o1", Amount: 1})
	if err != nil {
		t.Fatalf("被拒不该回传输层错误：%v", err)
	}
	if got := resp.GetCode(); got != codes.CodeUnauthorized {
		t.Fatalf("code = %d, want %d（未授权，不是限流）", got, codes.CodeUnauthorized)
	}
	// 关键断言：本层没接 traceOn 门控时，这里会多出一条 rpc.points.Earn，
	// 而它没有父节点 —— 那就是一条孤儿 trace。
	if n := len(spans()); n != 0 {
		t.Fatalf("trace.enabled=false 却产了 %d 条 span（第一条：%s）", n, spans()[0].Name)
	}
}

func TestRejectedCallIsVisibleWhenTraceEnabled(t *testing.T) {
	spans := captureSpans(t)
	s := &Server{Router: rejectingRouter(true), TraceOn: true}

	resp, err := s.Earn(context.Background(), &pointspb.EarnReq{UserId: 1001, OrderNo: "o1", Amount: 1})
	if err != nil {
		t.Fatalf("被拒不该回传输层错误：%v", err)
	}
	if got := resp.GetCode(); got != codes.CodeUnauthorized {
		t.Fatalf("code = %d, want %d", got, codes.CodeUnauthorized)
	}

	var sawRPC bool
	for _, sp := range spans() {
		if sp.Name == "rpc.points.Earn" {
			sawRPC = true
		}
	}
	if !sawRPC {
		t.Fatal("trace.enabled=true 却没有 rpc.points.Earn span —— 本层埋点没接上")
	}
}

// TestRejectedIsNotRateLimited 鉴权拒与限流拒必须回不同的码。
//
// 客户端的重试策略完全依赖这个区分：限流值得退避重试，鉴权重试一万次也是
// 同一个结果，只会把无效流量放大一万倍。
func TestRejectedIsNotRateLimited(t *testing.T) {
	_ = captureSpans(t)
	s := &Server{Router: rejectingRouter(false), TraceOn: false}

	resp, _ := s.Earn(context.Background(), &pointspb.EarnReq{UserId: 1001, OrderNo: "o1", Amount: 1})
	if resp.GetCode() == codes.CodeRateLimited {
		t.Fatal("鉴权失败被回成了限流码：客户端会据此退避重试，把无效流量放大")
	}
}
