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

// runChainStatus 跑一次请求，返回响应码与「末端 handler 是否被触达」。
// 与 identity_test.go 的 runThroughChain 的区别：本函数**允许**链在中途被拒
// （末端不可达），用于断言闸门的拒绝行为。
func runChainStatus(t *testing.T, mws []app.HandlerFunc, uri string, hdr map[string]string) (int, bool) {
	t.Helper()
	c := app.NewContext(0)
	c.Request.SetMethod("GET")
	c.Request.SetRequestURI(uri)
	for k, v := range hdr {
		c.Request.Header.Set(k, v)
	}
	reached := false
	c.SetHandlers(append(mws, func(ctx context.Context, c *app.RequestContext) {
		reached = true
		c.JSON(200, "ok")
	}))
	c.Next(context.Background())
	return c.Response.StatusCode(), reached
}

// TestRequiredIdentityGate 强制闸门的四个方向：匿名被拒、具名放行、豁免放行、
// 未开闸门时匿名放行。
//
// 第一条与第四条是 P0-2 入口鉴权的可执行判据，也钉住「auth.require=false 不装闸门」：
// 若把 bff 装成「无论开关都装闸门再放行」，第四条会立刻 FAIL —— 这正是 2026-10-10
// 自查时发现的实现缺口（配置写着关、行为却是每个无凭据请求 401）。
func TestRequiredIdentityGate(t *testing.T) {
	authed := map[string]string{AuthorizationHeader: "Bearer abc"}
	newResolver := func() Option { return WithIdentityResolver(NewTokenResolver(&fakeCache{val: "7"}, nil)) }
	cases := []struct {
		name       string
		opts       []Option
		uri        string
		hdr        map[string]string
		wantStatus int
		wantEnd    bool
	}{
		{
			name:       "开闸门 + 匿名 → 401 且挡在门外",
			opts:       []Option{newResolver(), WithRequiredIdentity(nil)},
			uri:        "/api/order/2",
			wantStatus: 401, wantEnd: false,
		},
		{
			name:       "开闸门 + 具名 → 放行",
			opts:       []Option{newResolver(), WithRequiredIdentity(nil)},
			uri:        "/api/order/2",
			hdr:        authed,
			wantStatus: 200, wantEnd: true,
		},
		{
			name:       "开闸门 + 豁免路径匿名 → 放行（探针不可被鉴权拖垮）",
			opts:       []Option{newResolver(), WithRequiredIdentity(func(p string) bool { return p == "/healthz/live" })},
			uri:        "/healthz/live",
			wantStatus: 200, wantEnd: true,
		},
		{
			name:       "未开闸门 + 匿名 → 放行（auth.require=false 的语义）",
			opts:       []Option{newResolver()},
			uri:        "/api/order/2",
			wantStatus: 200, wantEnd: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, reached := runChainStatus(t, Chain(tc.opts...), tc.uri, tc.hdr)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
			if reached != tc.wantEnd {
				t.Fatalf("末端 handler 触达 = %v, want %v", reached, tc.wantEnd)
			}
		})
	}
}
