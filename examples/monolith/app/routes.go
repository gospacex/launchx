package app

import (
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/gospacex/launchx/examples/monolith/internal/handler"
)

// RegisterRoutes 注册订单路由（唯一注册源）。
//
// 根 span 由 webx 默认链的 otel 中间件开（`<METHOD> <path>`），
// 业务中间件（鉴权 / 限流）在其下挂 `middleware.*` span，
// handler / service / 各组件适配层再往下挂各自的子 span。
//
// 鉴权只挂在 /secure 分组上：其余路由是 E1 / T13 的验收入口，
// 加令牌会让既有脚本全变 401 —— 那不是这些脚本要验证的东西。
// auth 为 nil（组件未装配）时不注册该分组，而不是注册一个恒 401 的空壳。
func RegisterRoutes(e *route.Engine, h *handler.OrderHandler, auth app.HandlerFunc) {
	e.GET("/orders", h.List)
	e.POST("/orders", h.Create)
	e.GET("/orders/search", h.Search)
	e.GET("/orders/:order_no", h.Get)
	e.GET("/orders/:order_no/join", h.GetWithUser)
	e.GET("/orders/:order_no/snapshot", h.Snapshot)
	e.PUT("/orders/:order_no", h.Update)
	e.DELETE("/orders/:order_no", h.Delete)

	if auth != nil {
		secure := e.Group("/secure", auth)
		secure.GET("/orders", h.List)
	}
}
