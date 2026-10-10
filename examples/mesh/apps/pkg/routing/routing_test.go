package routing

import (
	"context"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
)

// TestChainOrder 保护本包唯一的核心不变量：链顺序 audit → identity → rate_limit。
// 顺序一旦被改，本测试即失败。
func TestChainOrder(t *testing.T) {
	// 裸字面量：把顺序与字面值一起锁住。若复用 SpanAudit/... 常量，
	// 改掉常量字面值（如 routing.audit → routing.auditX）本测试仍会 PASS。
	want := []string{"routing.audit", "routing.identity", "routing.rate_limit"}

	// 契约来源本身：chainSpanNames 的顺序必须与期望一致。
	if len(chainSpanNames) != len(want) {
		t.Fatalf("chainSpanNames len = %d, want %d", len(chainSpanNames), len(want))
	}
	for i := range want {
		if chainSpanNames[i] != want[i] {
			t.Fatalf("chainSpanNames[%d] = %q, want %q（顺序不可调换）", i, chainSpanNames[i], want[i])
		}
	}

	// Chain() 恰好三层。
	chain := Chain()
	if len(chain) != len(want) {
		t.Fatalf("Chain() len = %d, want %d", len(chain), len(want))
	}

	// 组合跑一轮：三层 passThrough 只开 span 不干预请求，末端 handler 应收到透传。
	c := app.NewContext(0)
	c.Request.SetMethod("GET")
	c.Request.SetRequestURI("/ordered")
	reached := false
	c.SetHandlers(append(chain, func(ctx context.Context, c *app.RequestContext) {
		reached = true
		c.JSON(200, "ok")
	}))
	c.Next(context.Background()) // 不应 panic

	if !reached {
		t.Fatal("链未把请求透传到末端 handler")
	}
	if got := c.Response.StatusCode(); got != 200 {
		t.Fatalf("composed chain status = %d, want 200", got)
	}
}

// TestIdentityCarrierRoundTrip 覆盖 WithIdentity / IdentityFrom 这对载体的存取行为
// （空 ctx 取零值、写入后可读回）。载体本身是 Task 1 已就位的部分，接线在 Task 7+。
func TestIdentityCarrierRoundTrip(t *testing.T) {
	if got := IdentityFrom(context.Background()); got != (Identity{}) {
		t.Fatalf("空 ctx 应取到零值 Identity，得到 %+v", got)
	}

	want := Identity{UserID: 7, OperatorID: 9}
	ctx := WithIdentity(context.Background(), want)
	if got := IdentityFrom(ctx); got != want {
		t.Fatalf("IdentityFrom = %+v, want %+v", got, want)
	}
}
