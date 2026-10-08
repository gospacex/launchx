package routing

import (
	"context"
	"testing"

	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// 本文件锁两件事：①鉴权闸门在链上的位置；②被拒时 AbortedBy 能说出是谁拒的。
//
// 前者错了，未授权流量会消耗限流配额；后者错了，rpc 层会把鉴权失败回成限流码，
// 客户端据此退避重试 —— 把一次鉴权故障放大成一万次无效重试。

// denyAll 恒拒的鉴权器（不需要真的票据，只要能表达"被鉴权拒"这一条路径）。
type denyAll struct{}

func (denyAll) Authenticate(context.Context) (string, bool) { return "", false }

// allowAll 恒放的鉴权器
type allowAll struct{}

func (allowAll) Authenticate(context.Context) (string, bool) { return "user=1", true }

func newTestRouter(gate Authenticator, traceOn bool) *Router {
	return NewRouter("order", traceOn, logx.GetGlobal(), Route{
		Method: "CreateOrder",
		Kind:   KindWrite,
		Middlewares: []Middleware{
			Audit(nil),
			Authenticate(gate, nil),
			RateLimit(nil),
		},
	})
}

func TestAuthenticateRejectsAndNamesItself(t *testing.T) {
	rt := newTestRouter(denyAll{}, false)
	v := rt.Enter(context.Background(), "CreateOrder", 1)
	defer v.End()

	if v.Allowed {
		t.Fatal("恒拒的鉴权器竟然放行了")
	}
	if v.AbortedBy != MiddlewareNameAuth {
		t.Errorf("AbortedBy = %q, want %q（rpc 层据此回 1004 而不是 1005）", v.AbortedBy, MiddlewareNameAuth)
	}
}

func TestAuthenticateNilGatePassesThrough(t *testing.T) {
	// gate 为 nil = 鉴权未启用（部署选择）。此时必须放行，且不得短路后续链路。
	rt := newTestRouter(nil, false)
	v := rt.Enter(context.Background(), "CreateOrder", 1)
	defer v.End()

	if !v.Allowed || v.AbortedBy != "" {
		t.Fatalf("鉴权未启用时不应短路：allowed=%v abortedBy=%q", v.Allowed, v.AbortedBy)
	}
}

func TestAuthenticateAcceptsValidIdentity(t *testing.T) {
	rt := newTestRouter(allowAll{}, false)
	v := rt.Enter(context.Background(), "CreateOrder", 1)
	defer v.End()

	if !v.Allowed {
		t.Fatalf("合法身份被拒了：abortedBy=%q", v.AbortedBy)
	}
}

// TestRateLimitSeesAuthenticatedSubjectOnly 鉴权在限流之前：被鉴权拒的请求
// 不该走到限流那一节（否则未授权流量会消耗配额）。
func TestAuthRunsBeforeRateLimit(t *testing.T) {
	counting := &countingLimiter{}
	rt := NewRouter("order", false, logx.GetGlobal(), Route{
		Method: "CreateOrder",
		Kind:   KindWrite,
		Middlewares: []Middleware{
			Authenticate(denyAll{}, nil),
			RateLimit(counting),
		},
	})
	v := rt.Enter(context.Background(), "CreateOrder", 1)
	defer v.End()

	if v.Allowed {
		t.Fatal("期望被拒")
	}
	if counting.calls != 0 {
		t.Errorf("限流器被调用了 %d 次：被鉴权拒的请求不应消耗配额", counting.calls)
	}
}

// TestAuditStillRecordsRejectedCall 审计在链首：被鉴权拒的请求也必须留痕。
//
// 丢了这条，"当时到底有没有进来过"就回答不了 —— 排障时只能靠猜。
func TestAuditStillRecordsRejectedCall(t *testing.T) {
	rt := NewRouter("order", true, logx.GetGlobal(), Route{
		Method: "CreateOrder",
		Kind:   KindWrite,
		Middlewares: []Middleware{
			Audit(nil),
			Authenticate(denyAll{}, nil),
		},
	})

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	v := rt.Enter(context.Background(), "CreateOrder", 1)
	if v.Allowed {
		t.Fatal("期望被拒")
	}
	v.End()

	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("被拒的请求一个 span 都没留下 —— 审计中间件没有在链首生效")
	}
	var sawRoute, sawAuth bool
	for _, s := range spans {
		switch s.Name {
		case "route.order.CreateOrder":
			sawRoute = true
		case "middleware.order.auth":
			sawAuth = true
		}
	}
	if !sawRoute || !sawAuth {
		t.Errorf("缺 span：route=%v auth=%v（实际 %d 条）", sawRoute, sawAuth, len(spans))
	}
}

// TestUnregisteredRouteWarnsAndPasses 漏登记的方法显式放行并告警，
// 而不是按"无中间件"静默处理 —— 静默的后果是那个方法上没有限流也没有鉴权。
func TestUnregisteredRouteWarnsAndPasses(t *testing.T) {
	rt := newTestRouter(denyAll{}, false)
	v := rt.Enter(context.Background(), "SomeNewMethod", 1)
	defer v.End()

	if !v.Allowed {
		t.Fatal("漏登记的方法被拒了：加接口不该因为漏登记就 500")
	}
}

type countingLimiter struct{ calls int }

func (c *countingLimiter) Allow(context.Context, int64) bool {
	c.calls++
	return true
}
