package app

import (
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/gospacex/launchx/examples/monolith/internal/handler"
)

// RegisterRoutes 注册订单路由（唯一注册源）。
//
// 根 span 由 webx 默认链的 otel 中间件开（`<METHOD> <path>`），
// handler / service / repository 三层的子 span 在各自方法内自开。
func RegisterRoutes(e *route.Engine, h *handler.OrderHandler) {
	e.GET("/orders", h.List)
	e.POST("/orders", h.Create)
	e.GET("/orders/:order_no", h.Get)
	e.GET("/orders/:order_no/join", h.GetWithUser)
	e.PUT("/orders/:order_no", h.Update)
	e.DELETE("/orders/:order_no", h.Delete)
}
