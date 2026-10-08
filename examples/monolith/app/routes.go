package app

import (
	"context"
	"net/http"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"

	srroute "github.com/gospacex/launchx/examples/monolith/internal/route"

	"github.com/gospacex/launchx/examples/monolith/internal/handler"
)

// RegisterRoutes 注册订单路由。
//
// 注册动作改由 `internal/route.Table()` 驱动 —— 表里同时给出路由模板、业务
// 动作名与读写语义，hertz 注册与链路属性读的是同一份数据，不会再出现
// "加了接口但路由层没登记"的半边状态（未登记会在首次请求时告警）。
//
// 根 span 由 webx 默认链的 otel 中间件开，业务中间件（鉴权 / 限流）在
// route span 之下各挂一节，handler / service / repository 再往下延伸。
//
// 鉴权只挂在 /secure 分组上：其余路由是 E1 / T13 的验收入口，加令牌会让既有
// 脚本全变 401 —— 那不是这些脚本要验证的东西。auth 为 nil（组件未装配）时不
// 注册该分组，而不是注册一个恒 401 的空壳。
func RegisterRoutes(e *route.Engine, h *handler.OrderHandler, auth app.HandlerFunc) {
	var secure *route.RouterGroup
	if auth != nil {
		secure = e.Group("/secure", auth)
	}

	for _, en := range srroute.Table() {
		fn := handlerOf(h, en.Op)
		if en.Secure {
			// secure 为 nil 说明鉴权中间件没装配（缓存降级 / 令牌为空），
			// /secure 分组根本不存在。此处必须显式跳过：往 nil 分组上注册会让
			// hertz 在 nil 接收者里取字段而 panic —— 实测路径是 redis 不可达 →
			// cache 降级 → auth 为 nil → 进程在启动装配期直接崩溃（退出码 2）。
			// 降级场景下崩溃是最坏的一种姿态：旁路边车挂了把主流程一起带走。
			if secure == nil {
				continue
			}
			// 分组路由只支持单个方法注册 API，按表里的 GET/POST 分派。
			if en.Method == http.MethodGet {
				secure.GET(en.Pattern, fn)
				continue
			}
			secure.Handle(en.Method, en.Pattern, fn)
			continue
		}
		e.Handle(en.Method, en.Pattern, fn)
	}
}

// handlerOf 把路由表中的业务动作名映射到 handler 方法。
//
// 显式 switch 而不是反射：接口面只有九个动作，编译期就能发现"表里写了
// handler 没有的 Op"（default 分支直接给 —— 比反射按名字找不到再 panic 清楚）。
func handlerOf(h *handler.OrderHandler, op string) app.HandlerFunc {
	switch op {
	case "ListOrders":
		return h.List
	case "CreateOrder":
		return h.Create
	case "SearchOrders":
		return h.Search
	case "GetOrder":
		return h.Get
	case "GetOrderWithUser":
		return h.GetWithUser
	case "GetOrderSnapshot":
		return h.Snapshot
	case "UpdateOrder":
		return h.Update
	case "DeleteOrder":
		return h.Delete
	default:
		return func(_ context.Context, c *app.RequestContext) {
			c.String(http.StatusNotImplemented, "route: unbound op "+op)
		}
	}
}
