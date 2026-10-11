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
//   - identity：**真接线 + 真闸门**（Task 7 起，2026-10-10 加固）。经
//     `WithIdentityResolver` 注入一个 Resolver，从请求头取凭据、反查主体，
//     结果既进 ctx（供本进程后续层用），也由 handler 写进出向 kitex metainfo
//     （供四域 srv 的限流与归属校验用）；再由 `WithRequiredIdentity` 决定
//     「解析不出主体时拒绝还是放行」。
//
// ⚠️ 2026-10-10 修正了一处**与实际不符的设计假设**：此前本包注释写「验签与票据
// 时效交网格的 RequestAuthentication + AuthorizationPolicy」，但 `examples/mesh`
// 里从来没有 RequestAuthentication 资源（Istio 需要真实 issuer + JWKS，本示例
// 没有身份提供方），于是「入口无鉴权、匿名可下单、可读任意订单」是**真实存在的
// 缺口**，而注释读起来像「已经下沉了」。现在改为：**入口鉴权由应用侧自己做**
// （静态令牌播种 + 强制主体），网格侧只保留 mTLS STRICT 与「四域只收 bff」的
// 传输层身份约束。口径与 examples/micro 的 `auth.require` 一致，不再有两套真相。
//
// 不注入 resolver 时退化为不干预请求 —— Task 1 的形态仍被 TestChainOrder 保护。
package routing

import (
	"context"
	"net/http"

	"github.com/cloudwego/hertz/pkg/app"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
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
//   - 解析不出来时 MUST 返回零值身份而不是错误 —— 「拒还是放」是 **identityLayer
//     的决定**（由 `WithRequiredIdentity` 配），不是解析器的。解析器只负责取证，
//     让「取证」与「裁决」各只有一处实现，改闸门时不必动凭据逻辑。
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
	// require 为 true 时，解析不出主体的请求被拒（401）。
	require bool
	// skip 判「该路径豁免强制鉴权」。仅在 require 为 true 时有意义。
	skip func(path string) bool
}

// WithIdentityResolver 给 identity 层装上真实的主体解析器。
func WithIdentityResolver(r Resolver) Option {
	return func(c *chainCfg) { c.resolver = r }
}

// WithRequiredIdentity 让 identity 层在解析不出主体时**拒绝请求**（401）。
//
// 这是 2026-10-10 企业级加固补的：此前 identity 层只做 best-effort 反查
// （fail-open），并假定「传输层鉴权由网格的 RequestAuthentication 承担」——
// 但 `examples/mesh` 里从来没有 RequestAuthentication 资源，于是**入口实际无鉴权**：
// 匿名请求可以下单、可以读任意订单。现在应用侧自己把这道闸关上。
//
// skip 命中的路径豁免（**健康探针必须豁免**：凭据存储抖动时若探针也 401，
// 编排系统会重启一批本来健康的实例，把局部故障放大成全局）。skip 为 nil
// 表示不豁免任何路径 —— 那是测试与「无探针场景」才该用的取值。
func WithRequiredIdentity(skip func(path string) bool) Option {
	return func(c *chainCfg) { c.require = true; c.skip = skip }
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
			mws = append(mws, identityLayer(cfg))
			continue
		}
		mws = append(mws, passThrough(name))
	}
	return mws
}

// identityLayer 开 identity span、解析主体，并在 require 时把匿名请求挡在门外。
//
// 身份只经 **ctx** 往下传（`c.Next(ctx2)` 会把 ctx 交给后续 handler 与末端：
// hertz 的 Next 实现是 `handlers[i](c, ctx)`），所以末端 handler 拿到的 ctx
// 里已经有身份，出向 RPC 也因此能把它写进 metainfo。
//
// 拒绝用 `AbortWithStatusJSON`（而不是只写响应就 return）：hertz 的 Next 是
// 「循环调用 handlers[i]」，光 return 挡不住后面的 handler，必须把 index 推到
// AbortIndex 才能让循环停下。这是「看起来拒了、实际业务照跑」的经典坑。
//
// 解析结果为零值时**不打 enduser.id 属性**：写 0 会让「匿名」在 Jaeger 里
// 看起来像一个 id 为 0 的用户，反而误导排查。被拒时另打 `enduser.rejected`，
// 让「401 是谁拒的」在链路上有唯一落点。
func identityLayer(cfg chainCfg) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		ctx, span := otel.Tracer("launchx.mesh.apps").Start(ctx, SpanIdentity)
		defer span.End()

		if cfg.resolver != nil {
			ctx = cfg.resolver(ctx, c)
		}
		if id := IdentityFrom(ctx); id.UserID != 0 {
			span.SetAttributes(attribute.Int64("enduser.id", int64(id.UserID)))
		} else {
			span.SetAttributes(attribute.Bool("enduser.anonymous", true))
			if cfg.require && !skipPath(cfg.skip, c) {
				span.SetAttributes(attribute.Bool("enduser.rejected", true))
				c.AbortWithStatusJSON(http.StatusUnauthorized, map[string]any{
					"code":  int32(codes.Unauthorized),
					"error": "missing or invalid credentials (send `Authorization: Bearer <token>`)",
				})
				return
			}
		}
		c.Next(ctx)
	}
}

// skipPath 判路径是否豁免强制鉴权。skip 为 nil 一律不豁免。
func skipPath(skip func(string) bool, c *app.RequestContext) bool {
	if skip == nil {
		return false
	}
	return skip(string(c.Path()))
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
