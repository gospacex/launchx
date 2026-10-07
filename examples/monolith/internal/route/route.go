// Package route 路由层：分层链的第一节，位于根 span 与业务中间件之间。
//
// 链路位次（与 examples/mall 四个微服同构）：
//
//	根 SERVER span（webx otel 中间件）
//	  └── route.order.<Op>          ← 本层
//	        └── middleware.RateLimit
//	              └── middleware.AuthToken（仅 /secure）
//	                    └── handler.<Op> → service.<Op> → repository.<Op>
//
// 为什么 HTTP 形态也要单独有这一层 —— 早先本示例是「webx 根 span 直接挂
// 中间件」，缺的一环在这里很具体：webx 用**实际路径**给根 span 命名
// （`GET /orders/NO1791.../join`），Jaeger 上一笔订单一个 operation 名，
// 既看不出它属于哪条路由，也没法按路由聚合耗时/错误率。本层补一节 route span，
// 并把 `http.route`（路由模板）与读写语义打进属性，聚合口径由此唯一。
//
// 另一件事：路由表是**唯一注册源**。以前 apps/routes.go 写一遍 hertz 注册、
// 中间件/属性各写一遍，新增接口只改一处会漏 —— 漏登记在这里会在首个工作日
// 被打出来（Warn + span 属性 route.registered=false），而不是悄悄裸奔。
package route

import (
	"context"
	"net/http"
	"sync"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel/attribute"

	"github.com/gospacex/otelx/layer"
)

// Kind 路由的读写语义。
//
// 分类不是为了好看：读可以放宽限流、可以走缓存；写必须串行、必须算配额。
// 混为一谈时最常出的后果是"写接口被读接口的宽松配额放行"。
type Kind uint8

const (
	// KindRead 只读路径。
	KindRead Kind = iota
	// KindWrite 写路径。
	KindWrite
)

// String 返回读写语义的可读名（进 span 属性）。
func (k Kind) String() string {
	if k == KindWrite {
		return "write"
	}
	return "read"
}

// Entry 一条路由的登记项。
type Entry struct {
	Method  string // HTTP 方法（GET / POST / PUT / DELETE）
	Pattern string // hertz 路由模板，如 /orders/:order_no
	Op      string // 业务动作名（与 handler / service 的 op 同名，便于跨层对照）
	Kind    Kind
	Secure  bool // true = 挂在 /secure 分组上，走鉴权中间件
}

// Table 订单域路由表（唯一注册源）。
//
// Op 刻意与 handler / service 的方法名一致：跨层对照时同一个动作
// 在三个层次上是同一个名字（`route.order.CreateOrder` /
// `handler.CreateOrder` / `service.CreateOrder`），不用脑内翻译。
func Table() []Entry {
	return []Entry{
		{Method: http.MethodGet, Pattern: "/orders", Op: "ListOrders", Kind: KindRead},
		{Method: http.MethodPost, Pattern: "/orders", Op: "CreateOrder", Kind: KindWrite},
		{Method: http.MethodGet, Pattern: "/orders/search", Op: "SearchOrders", Kind: KindRead},
		{Method: http.MethodGet, Pattern: "/orders/:order_no", Op: "GetOrder", Kind: KindRead},
		{Method: http.MethodGet, Pattern: "/orders/:order_no/join", Op: "GetOrderWithUser", Kind: KindRead},
		{Method: http.MethodGet, Pattern: "/orders/:order_no/snapshot", Op: "GetOrderSnapshot", Kind: KindRead},
		{Method: http.MethodPut, Pattern: "/orders/:order_no", Op: "UpdateOrder", Kind: KindWrite},
		{Method: http.MethodDelete, Pattern: "/orders/:order_no", Op: "DeleteOrder", Kind: KindWrite},
		{Method: http.MethodGet, Pattern: "/secure/orders", Op: "ListOrders", Kind: KindRead, Secure: true},
	}
}

// Router 订单域路由层。
type Router struct {
	entity  string
	entries map[string]Entry // key = "<METHOD> <pattern>"
	traceOn bool
	logger  logx.Logger
	warned  sync.Map
}

// New 构造路由层。entries 为空时未登记路由会走「有请求无登记」的告警路径。
func New(entity string, logger logx.Logger, traceOn bool) *Router {
	r := &Router{entity: entity, entries: make(map[string]Entry), traceOn: traceOn, logger: logger}
	r.Register(Table())
	return r
}

// Register 登记路由项（同名覆盖）。
func (r *Router) Register(entries []Entry) {
	for _, e := range entries {
		r.entries[e.Method+" "+e.Pattern] = e
	}
}

// key 组装登记表的键。
func key(method, pattern string) string { return method + " " + pattern }

// Handle 是挂到 webx 引擎上的 hertz 中间件。
//
// 必须挂在业务中间件**之前**（见 setup.go 的装配顺序）：只有先入后出才能把
// 限流 / 鉴权的 span 收在本节之内；挂反了 route 会变成 handler 的兄弟节点，
// 链路读起来像"这次请求没走路由"。
func (r *Router) Handle(ctx context.Context, c *app.RequestContext) {
	method := string(c.Request.Method())
	pattern := c.FullPath() // 路由匹配成功后才是模板串；未匹配（404）为空
	e, ok := r.entries[key(method, pattern)]

	op := e.Op
	if !ok {
		// 未登记的请求（多为未匹配的 404 / 忘了登记的新接口）照样记一节，
		// 但要能一眼认出来：留 OP 名 + 属性，同时只告警一次，避免刷屏。
		op = "Unregistered"
		if pattern != "" {
			if _, dup := r.warned.LoadOrStore(pattern+method, struct{}{}); !dup && r.logger != nil {
				r.logger.Warn("monolith: request hit an unregistered route",
					logx.String("method", method),
					logx.String("route", pattern),
					logx.String("hint", "add it to route.Table()"))
			}
		}
	}

	// 本层不产生 error（失败标在下游 handler / service 上），故收尾传 nil：
	// 传 nil 不等于不调 —— 不调 end 这个 span 就永不 End。
	ctx, end := layer.Begin(ctx, "route", r.entity, op, r.traceOn, nil)
	defer end(nil)
	layer.Attrs(ctx,
		attribute.String("http.method", method),
		attribute.String("http.route", pattern),
		attribute.String("http.target", string(c.Request.Path())),
		attribute.String("route.kind", e.Kind.String()),
		attribute.Bool("route.registered", ok),
	)
	c.Next(ctx)
}
