// Package routing 四服共用的入口路由与中间件链。
//
// 它把「路由 → 中间件 → 业务」这一段的公共骨架抽出来，让四个微服各自只写
// 域内差异（路由表怎么配、限流器怎么建），不再各抄一份 Enter/放行判定。
//
// 分层链上的位置（与 handler / service / repository 的父子关系）：
//
//	route.<entity>.<Method>
//	  └── middleware.<entity>.audit
//	        └── middleware.<entity>.rate_limit
//	              └── handler.<entity>.<Method>
//	                    └── service.<entity>.<op>
//	                          └── repository.<entity>.<op>
//
// 中间件 span 刻意保持**嵌套**而非平铺：平铺时所有中间件的耗时都叠在 route
// 的自身耗时里，看不出是限流慢还是审计慢；嵌套后每段各自计时，慢的那一节
// 在火焰图上一眼可见。
//
// 本包只认识 layer 与 logx，不认识 kitex —— 路由表对传输协议无感知，
// 换 HTTP（bff / gateway）或换 RPC 都不用改这里。
package routing

import (
	"context"
	"fmt"
	"sort"

	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/otelx/layer"
)

// Kind 路由语义：读还是写。
//
// 中间件据此差异化（写操作按 operator 限流更严、审计字段更全），而不是让
// 每个方法各自传一堆 bool。
type Kind int

// 路由语义取值
const (
	KindRead Kind = iota // 读（列表 / 详情 / 报表）
	KindWrite
)

// String 稳定字符串（进 span 属性，别用 %d —— 属性是给人看的）
func (k Kind) String() string {
	if k == KindWrite {
		return "write"
	}
	return "read"
}

// Call 一次入口调用的描述（中间件的唯一入参）
//
// 只放中间件真正需要的字段：方法名、读写语义、限流主体、链路 ID。
// 不放 req / resp —— 一旦放进去，中间件就会开始读业务字段，链子就散了。
type Call struct {
	Method  string
	Kind    Kind
	Subject int64  // 限流主体：写操作取 operator_id，读操作取 user_id
	TraceID string // 供中间件日志对齐链路（无活动 span 时为空）
}

// Middleware 入口中间件。返回 false 表示拒绝本次请求并短路后续链路。
//
// Name 不是装饰：被拒时要能回答"是谁拒的"，否则排障只能猜。
type Middleware interface {
	Name() string
	Handle(ctx context.Context, c *Call) bool
}

// Limiter 计数型限流器接口（由各服 internal/middleware 提供实现）。
//
// 放接口而不是直接依赖 kvx：routing 是公共包，不该因为"限流用 kvx"就把
// kvx 绑死进所有使用方；各服想换本地令牌桶也能直接替换。
type Limiter interface {
	Allow(ctx context.Context, subject int64) bool
}

// rateLimitMW 把 Limiter 适配成 Middleware
type rateLimitMW struct {
	limiter Limiter
}

// Name 中间件名（进 span 名与日志）
func (m rateLimitMW) Name() string { return "rate_limit" }

// Handle 放行判定。limiter 为 nil（组件未装配）时恒放行 —— 见包注释的 fail-open 立场
func (m rateLimitMW) Handle(ctx context.Context, c *Call) bool {
	if m.limiter == nil {
		return true
	}
	return m.limiter.Allow(ctx, c.Subject)
}

// RateLimit 构造限流中间件。l 为 nil 时该中间件恒放行。
func RateLimit(l Limiter) Middleware { return rateLimitMW{limiter: l} }

// auditMW 审计中间件：把路由语义写进 span 属性，并落一条入口访问日志。
//
// 恒放行（它是观察型中间件，不是闸门）。放在链首：即使被后面的限流拒了，
// 这一次调用也已经在链路上留痕，能回答"当时到底有没有进来过"。
type auditMW struct {
	logger logx.Logger
}

// Name 中间件名
func (m auditMW) Name() string { return "audit" }

// Handle 写 span 属性 + 入口日志
func (m auditMW) Handle(ctx context.Context, c *Call) bool {
	span := trace.SpanFromContext(ctx)
	if span != nil && span.IsRecording() {
		span.SetAttributes(
			attribute.String("routing.method", c.Method),
			attribute.String("routing.kind", c.Kind.String()),
			attribute.Int64("routing.subject", c.Subject),
		)
	}
	if m.logger != nil {
		m.logger.DebugContext(ctx, "enter route",
			logx.String("method", c.Method),
			logx.String("kind", c.Kind.String()),
			logx.Int("subject", int(c.Subject)),
			logx.String("trace_id", c.TraceID),
		)
	}
	return true
}

// Audit 构造审计中间件。logger 为 nil 时只写 span 属性，不落日志。
func Audit(logger logx.Logger) Middleware { return auditMW{logger: logger} }

// Route 一条路由：方法名 + 读写语义 + 中间件链
type Route struct {
	Method      string
	Kind        Kind
	Middlewares []Middleware
}

// Router 路由表（按方法名索引）
type Router struct {
	entity  string
	traceOn bool
	logger  logx.Logger
	routes  map[string]Route
}

// NewRouter 构造路由表并登记初始路由。
//
// entity 进 span 名的第二段（route.product.CreateProduct），与 layer 的
// "<layer>.<entity>.<op>" 约定一致，四服在 Jaeger 上可按 entity 筛。
func NewRouter(entity string, traceOn bool, logger logx.Logger, routes ...Route) *Router {
	r := &Router{entity: entity, traceOn: traceOn, logger: logger, routes: make(map[string]Route, len(routes))}
	r.Register(routes...)
	return r
}

// Register 登记/覆盖路由。重复登记同名方法以最后一次为准（装配期就近可改）。
func (r *Router) Register(routes ...Route) {
	for _, rt := range routes {
		if rt.Method == "" {
			continue
		}
		r.routes[rt.Method] = rt
	}
}

// Has 路由是否已登记
func (r *Router) Has(method string) bool {
	_, ok := r.routes[method]
	return ok
}

// Methods 已登记的方法名（排序后输出，便于装配期自检日志稳定）
func (r *Router) Methods() []string {
	out := make([]string, 0, len(r.routes))
	for m := range r.routes {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// Visit 一次路由访问的句柄。
//
// 调用方**必须**调 End：route span 与各中间件 span 都挂在这里，漏掉会让
// 这一次请求在 Jaeger 上只剩 rpc / handler 两节，且 parent 悬空。
// 被拒（Allowed=false）时同样要 End —— 被限流的请求才更需要留痕。
type Visit struct {
	Ctx       context.Context // 经中间件链后的 ctx（handler 应以此为父）
	Allowed   bool
	AbortedBy string // 短路的中间件名（Allowed=true 时为空）
	ends      []func()
}

// End 逆序收尾（先开的最后关），保证 span 嵌套关系与耗时归属正确
func (v *Visit) End() {
	if v == nil {
		return
	}
	for i := len(v.ends) - 1; i >= 0; i-- {
		if v.ends[i] != nil {
			v.ends[i]()
		}
	}
	v.ends = nil
}

// Enter 走一次路由：开 route span → 顺序执行中间件链 → 返回访问句柄。
//
// 未登记的方法**显式放行并告警**而不是按"无中间件"静默处理：kitex 只会调
// 生成接口里的方法，走到这里说明有人加了接口忘了登记路由，静默放行的后果是
// 新方法上没有限流、审计也没有，等出事才发现。放行则不阻断业务（加接口不该
// 因为漏登记就 500）。
func (r *Router) Enter(ctx context.Context, method string, subject int64) *Visit {
	rt, ok := r.routes[method]
	if !ok {
		if r.logger != nil {
			r.logger.WarnContext(ctx, "route not registered, serving without middleware",
				logx.String("entity", r.entity),
				logx.String("method", method),
				logx.String("consequence", "no rate limit and no audit on this method"),
				logx.String("hint", "register it in internal/route"))
		}
		return &Visit{Ctx: ctx, Allowed: true}
	}

	v := &Visit{Ctx: ctx, Allowed: true}
	if r.traceOn {
		spanCtx, span := layer.Start(ctx, "route", r.entity, method)
		span.SetAttributes(
			attribute.String("routing.method", method),
			attribute.String("routing.kind", rt.Kind.String()),
		)
		v.Ctx = spanCtx
		v.ends = append(v.ends, func() { span.End() })
	}

	c := &Call{Method: method, Kind: rt.Kind, Subject: subject, TraceID: traceIDOf(v.Ctx)}
	cur := v.Ctx
	for _, mw := range rt.Middlewares {
		if mw == nil {
			continue
		}
		if r.traceOn {
			mwCtx, mwSpan := layer.Start(cur, "middleware", r.entity, mw.Name())
			cur = mwCtx
			span := mwSpan // 闭包捕获局部变量：直接捕获循环变量会关错 span
			v.ends = append(v.ends, func() { span.End() })
		}
		if !mw.Handle(cur, c) {
			v.Allowed = false
			v.AbortedBy = mw.Name()
			if span := trace.SpanFromContext(v.Ctx); span != nil && span.IsRecording() {
				span.SetAttributes(
					attribute.Bool("routing.aborted", true),
					attribute.String("routing.aborted_by", mw.Name()),
				)
			}
			return v
		}
	}
	// 放行：把最后一节中间件 ctx 交出去，handler 因此挂在链尾（而非挂在 route 上）
	v.Ctx = cur
	return v
}

// traceIDOf 取当前链路 ID（无活动 span 时为空串，中间件日志里就不带这一项）
func traceIDOf(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}

// String 便于装配期自检打印
func (r *Router) String() string {
	return fmt.Sprintf("router[%s]: %v", r.entity, r.Methods())
}
