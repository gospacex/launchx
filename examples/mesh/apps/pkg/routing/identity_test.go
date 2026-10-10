package routing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/gospacex/cachex"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// fakeCache 只实现反查用得到的那个方法，其余按接口补零值。
//
// 不引真 cachex 驱动：本层要验的是「查不到 / 查到坏值 / 报错」三种姿态，
// 真驱动反而让这三种更难过驱动。
type fakeCache struct {
	val string
	err error
}

func (c *fakeCache) Get(context.Context, string) (string, error) { return c.val, c.err }
func (c *fakeCache) Set(context.Context, string, string, time.Duration) error {
	return nil
}
func (c *fakeCache) Delete(context.Context, string) error           { return nil }
func (c *fakeCache) Exists(context.Context, string) (bool, error)   { return false, nil }
func (c *fakeCache) Keys(context.Context, string) ([]string, error) { return nil, nil }
func (c *fakeCache) Close() error                                   { return nil }
func (c *fakeCache) HealthCheck(context.Context) error              { return nil }

var _ cachex.Cache = (*fakeCache)(nil)

// runThroughChain 把链挂进一个真实的 hertz RequestContext，跑一次请求，
// 返回末端 handler 拿到的 ctx（身份就藏在里面）。
func runThroughChain(t *testing.T, mws []app.HandlerFunc, hdr map[string]string) context.Context {
	t.Helper()

	c := app.NewContext(0)
	c.Request.SetMethod("GET")
	c.Request.SetRequestURI("/ping")
	for k, v := range hdr {
		c.Request.Header.Set(k, v)
	}

	var got context.Context
	c.SetHandlers(append(mws, func(ctx context.Context, c *app.RequestContext) {
		got = ctx
		c.JSON(200, "ok")
	}))
	c.Next(context.Background())

	if got == nil {
		t.Fatal("链没有把请求透传到末端 handler")
	}
	return got
}

// TestIdentityResolverReachesEndHandler 身份必须真的到得了末端 handler。
//
// 这条是「从请求头取身份」整条链的判据：中间件里解析成功但没传下去，
// 现象与「解析失败」一模一样（都是零值），只跑解析器单测看不出来。
// 末端 handler 的 ctx 正是出向 RPC 用的那个 ctx（metainfo 从它取）。
func TestIdentityResolverReachesEndHandler(t *testing.T) {
	cache := &fakeCache{val: "7"}
	chain := Chain(WithIdentityResolver(NewTokenResolver(cache, nil)))

	ctx := runThroughChain(t, chain, map[string]string{AuthorizationHeader: "Bearer abc"})

	if got := IdentityFrom(ctx); got.UserID != 7 {
		t.Fatalf("末端 handler 拿到的 UserID = %d，期望 7（身份没被传下去）", got.UserID)
	}
}

// TestIdentitySpanAttributes 匿名与具名要在 span 上分得开。
//
// 打 id=0 会让「匿名」看起来像一个 id 为 0 的用户；不打标记又会让
// 「限流为何落在 svc 桶」无从判断。
func TestIdentitySpanAttributes(t *testing.T) {
	cases := []struct {
		name     string
		cache    *fakeCache
		hdr      map[string]string
		wantAttr string
	}{
		{"具名", &fakeCache{val: "7"}, map[string]string{AuthorizationHeader: "Bearer abc"}, "enduser.id"},
		{"匿名", &fakeCache{val: "7"}, nil, "enduser.anonymous"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sr, restore := withRecorder(t)
			defer restore()

			runThroughChain(t, Chain(WithIdentityResolver(NewTokenResolver(tc.cache, nil))), tc.hdr)

			var found bool
			for _, sp := range sr.Ended() {
				if sp.Name() != SpanIdentity {
					continue
				}
				for _, kv := range sp.Attributes() {
					if string(kv.Key) == tc.wantAttr {
						found = true
					}
					if string(kv.Key) == "enduser.id" && tc.wantAttr == "enduser.anonymous" {
						t.Fatalf("匿名请求不该带 enduser.id（值为 %v）", kv.Value.AsInt64())
					}
				}
			}
			if !found {
				t.Fatalf("identity span 上没有 %s 属性（spans=%v）", tc.wantAttr, spanNames(sr))
			}
		})
	}
}

// TestNoResolverStaysAnonymous 不注入 resolver 时链退化为骨架，且不 panic。
// 这是 Task 1 形态的回归保护（Chain() 无参调用仍须可用）。
func TestNoResolverStaysAnonymous(t *testing.T) {
	ctx := runThroughChain(t, Chain(), nil)
	if got := IdentityFrom(ctx); got != (Identity{}) {
		t.Fatalf("无 resolver 时不该出现身份，得到 %+v", got)
	}
}

// TestTokenResolverShapes 反查的四种姿态。
//
// 「取不到」一律零值而不是错误：本层不拒绝请求（鉴权在网格侧），
// 把「token 不认识」变成 401 会让一次凭据未播种直接升级成业务中断。
func TestTokenResolverShapes(t *testing.T) {
	cases := []struct {
		name  string
		cache cachex.Cache
		hdr   map[string]string
		want  uint64
	}{
		{"没带凭据", &fakeCache{val: "7"}, nil, 0},
		{"方案不对", &fakeCache{val: "7"}, map[string]string{AuthorizationHeader: "abc"}, 0},
		{"凭据为空", &fakeCache{val: "7"}, map[string]string{AuthorizationHeader: "Bearer "}, 0},
		{"反查命中", &fakeCache{val: "42"}, map[string]string{AuthorizationHeader: "Bearer abc"}, 42},
		{"反查未命中", &fakeCache{}, map[string]string{AuthorizationHeader: "Bearer abc"}, 0},
		{"载荷非法", &fakeCache{val: "not-a-number"}, map[string]string{AuthorizationHeader: "Bearer abc"}, 0},
		{"载荷为零", &fakeCache{val: "0"}, map[string]string{AuthorizationHeader: "Bearer abc"}, 0},
		{"缓存报错", &fakeCache{err: errors.New("boom")}, map[string]string{AuthorizationHeader: "Bearer abc"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewTokenResolver(tc.cache, nil)
			if r == nil {
				t.Fatal("NewTokenResolver 不该返回 nil")
			}
			ctx := runThroughChain(t, Chain(WithIdentityResolver(r)), tc.hdr)
			if got := IdentityFrom(ctx).UserID; got != tc.want {
				t.Fatalf("UserID = %d，期望 %d", got, tc.want)
			}
		})
	}
}

// TestTokenResolverNilStore 缓存缺失时返回 nil resolver（identity 层退化）。
// 返回一个「永远匿名」的 resolver 会掩盖「cache 没装配」这件事。
func TestTokenResolverNilStore(t *testing.T) {
	if r := NewTokenResolver(nil, nil); r != nil {
		t.Fatal("store 为 nil 时应返回 nil resolver")
	}
}

// TestTokenKeyPrefix 键前缀是跨 Task 契约（Task 11 播种要用同一个）。
func TestTokenKeyPrefix(t *testing.T) {
	if !strings.HasPrefix(TokenKeyPrefix, "mesh:") || !strings.HasSuffix(TokenKeyPrefix, ":") {
		t.Fatalf("TokenKeyPrefix = %q，期望形如 mesh:<域>:<用途>:", TokenKeyPrefix)
	}
}

func spanNames(sr *tracetest.SpanRecorder) []string {
	names := make([]string, 0, len(sr.Ended()))
	for _, sp := range sr.Ended() {
		names = append(names, sp.Name())
	}
	return names
}

func withRecorder(t *testing.T) (*tracetest.SpanRecorder, func()) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	return sr, func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	}
}
