// Package routing HTTP 边界中间件链骨架：audit → identity → rate_limit 三层，
// 顺序固定且每层自带 span，便于在 Jaeger 里一眼看出请求停在哪一层。
//
// 三层的职责**按证据分级**，别把「有这一层」读成「这一层在干活」：
//
//   - audit / rate_limit：**只开 span 的骨架**。边界审计由 webx 的访问日志承担；
//     入口限流由 webx 的 `rate_limit_rps/burst`（按 client IP）承担 —— 且
//     `rate_limit` 这里**不可能**做成 KV 计数限流：句柄矩阵不给 bff `KV()`
//     （计数器的存储入口），写成自建 store 就破了「一律经 App 取句柄」。
//     保留这两层的价值是把边界链的**形状与 span** 固定下来。
//   - identity：**真接线**（Task 7 起）。经 `WithIdentityResolver` 注入一个
//     Resolver，从请求头取凭据、反查主体，结果既进 ctx（供本进程后续层用），
//     也由 handler 写进出向 kitex metainfo（供四域 srv 的限流用）。
//     不注入 resolver 时退化为不干预请求 —— Task 1 的形态仍被
//     TestChainOrder 保护。
//
// 与 examples/micro/pkg/routing 的差别（有意为之的减法）：
//   - 不承载 token 签发 / 缓存反验（那是 micro 的 pkg/auth 195 行）；
//   - 本包**不拒绝请求**：验签与票据时效交网格的 RequestAuthentication +
//     AuthorizationPolicy（mTLS 身份）。反查不到主体就是匿名，fail-open。
package routing

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// 三层各自的 span 名。抽成常量以便 Chain 与测试共用同一份「顺序契约」。
const (
	SpanAudit     = "routing.audit"
	SpanIdentity  = "routing.identity"
	SpanRateLimit = "routing.rate_limit"
)

// chainSpanNames 是 Chain() 的规范顺序（顺序契约的唯一来源）。
// 顺序不可调换，理由见 Chain 注释；由 TestChainOrder 保护。
var chainSpanNames = []string{SpanAudit, SpanIdentity, SpanRateLimit}

// Identity 请求主体。取不到时为零值，由 handler 决定放行还是拒绝。
type Identity struct {
	UserID     uint64
	OperatorID uint64
}

// Resolver 从一次请求解析主体身份。
//
// 契约（写窄，免得实现者自由发挥）：
//   - 返回的 ctx MUST 带上解析结果（`WithIdentity`）与任何要传给下游的东西；
//   - 解析不出来时 MUST 返回零值身份而不是错误 —— 本层不拒绝请求（见包注释），
//     「取证失败」与「没带凭据」在这里是同一件事。
type Resolver func(ctx context.Context, c *app.RequestContext) context.Context

type ctxKey struct{}

// WithIdentity 把身份放进 ctx。
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// IdentityFrom 从 ctx 取身份（取不到返回零值，不 panic）。
func IdentityFrom(ctx context.Context) Identity {
	v, _ := ctx.Value(ctxKey{}).(Identity)
	return v
}

// Option 定制链的某一层。不传选项时三层都是不干预请求的骨架
// （Task 1 的形态，仍由 TestChainOrder 保护）。
type Option func(*chainCfg)

type chainCfg struct {
	resolver Resolver
}

// WithIdentityResolver 给 identity 层装上真实的主体解析器。
func WithIdentityResolver(r Resolver) Option {
	return func(c *chainCfg) { c.resolver = r }
}

// Chain 组装边界中间件链：audit → identity → rate_limit。
//
// 顺序是**契约**，不可调换（由 TestChainOrder 保护）：
// audit 最先（它要记录原始请求，晚于任何一层都会丢字段），
// identity 居中（rate_limit 需要按主体计数），
// rate_limit 最后（它是最便宜的一道拒绝，放在身份之后才能按人限流）。
//
// 返回 hertz 的 app.HandlerFunc 切片，可直接喂给 `engine.Use(...)`
// （webx.App 内嵌 *server.Hertz → *route.Engine，Use 汇入同一份 handlers）。
func Chain(opts ...Option) []app.HandlerFunc {
	var cfg chainCfg
	for _, o := range opts {
		o(&cfg)
	}

	mws := make([]app.HandlerFunc, 0, len(chainSpanNames))
	for _, name := range chainSpanNames {
		if name == SpanIdentity {
			mws = append(mws, identityLayer(cfg.resolver))
			continue
		}
		mws = append(mws, passThrough(name))
	}
	return mws
}

// identityLayer 开 identity span 并在有 resolver 时真的解析主体。
//
// 身份只经 **ctx** 往下传（`c.Next(ctx2)` 会把 ctx 交给后续 handler 与末端：
// hertz 的 Next 实现是 `handlers[i](c, ctx)`），所以末端 handler 拿到的 ctx
// 里已经有身份，出向 RPC 也因此能把它写进 metainfo。
//
// 解析结果为零值时**不打 enduser.id 属性**：写 0 会让「匿名」在 Jaeger 里
// 看起来像一个 id 为 0 的用户，反而误导排查。
func identityLayer(r Resolver) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		ctx, span := otel.Tracer("launchx.mesh.apps").Start(ctx, SpanIdentity)
		defer span.End()

		if r != nil {
			ctx = r(ctx, c)
			if id := IdentityFrom(ctx); id.UserID != 0 {
				span.SetAttributes(attribute.Int64("enduser.id", int64(id.UserID)))
			} else {
				span.SetAttributes(attribute.Bool("enduser.anonymous", true))
			}
		}
		c.Next(ctx)
	}
}

// passThrough 产出一个只记 span 不干预请求的中间件。
//
// 签名以 hubx/webx 的中间件类型为准：webx.Config.Middlewares 即 []app.HandlerFunc，
// 推进用 RequestContext.Next(ctx)（hertz 约定的链式调用，见 webx/middleware/otel.go）。
func passThrough(name string) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		ctx2, span := otel.Tracer("launchx.mesh.apps").Start(ctx, name)
		defer span.End()
		c.Next(ctx2)
	}
}
