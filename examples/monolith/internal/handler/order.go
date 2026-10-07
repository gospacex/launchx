// Package handler HTTP 层：只做参数绑定、状态码映射与响应封装，
// 业务一律下沉到 service；每个 handler 自开 `handler.*` span。
//
// 本包是 webx/hertz 唯一出现的地方（函数签名是 hertz 的 app.HandlerFunc），
// 往下（service / repository）不出现任何 HTTP 概念。
package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/monolith/internal/model"
	"github.com/gospacex/launchx/examples/monolith/internal/service"
	"github.com/gospacex/launchx/examples/monolith/internal/tracex"
)

// OrderHandler 订单 HTTP 入口。
type OrderHandler struct {
	svc    *service.Service
	tracer trace.Tracer
}

// New 构造订单 handler。
func New(svc *service.Service, tracer trace.Tracer) *OrderHandler {
	return &OrderHandler{svc: svc, tracer: tracer}
}

// begin 开 handler 层 span，返回「收 error 再收尾」的闭包。
//
// span 名由另一处改为三段式 handler.order.<Op>（与 service / repository 对齐），
// 本次只统一调用形态，不动命名。
//
// handler 是 hertz 回调（无返回值），故每个方法里显式 `var err error` 并在失败
// 分支赋值 —— 少这一步，失败路径在 Jaeger 上就是"成功"的绿色。
func (h *OrderHandler) begin(ctx context.Context, op string) (context.Context, tracex.End) {
	return tracex.Begin(h.tracer, ctx, "handler", "order", op, nil)
}

// List GET /orders —— 单表查询入口（E1 的 JSON 数组断言落在这里）。
func (h *OrderHandler) List(ctx context.Context, c *app.RequestContext) {
	ctx, end := h.begin(ctx, "ListOrders")
	var err error
	defer func() { end(err) }()

	limit, _ := strconv.Atoi(c.Query("limit"))
	orders, err := h.svc.List(ctx, limit)
	if err != nil {
		fail(c, http.StatusInternalServerError, err)
		return
	}
	if orders == nil {
		orders = []model.Order{} // 空结果渲染成 [] 而不是 null
	}
	c.JSON(http.StatusOK, orders)
}

// Create POST /orders —— 创建订单（事务写 orders + order_items）。
func (h *OrderHandler) Create(ctx context.Context, c *app.RequestContext) {
	ctx, end := h.begin(ctx, "CreateOrder")
	var err error
	defer func() { end(err) }()

	var req createRequest
	if err = c.Bind(&req); err != nil {
		fail(c, http.StatusBadRequest, service.ErrInvalidInput)
		return
	}
	in := service.CreateInput{
		UserID:    req.UserID,
		Item:      req.Item,
		SKU:       req.SKU,
		Qty:       req.Qty,
		UnitPrice: req.UnitPrice,
	}
	order, err := h.svc.Create(ctx, in)
	if err != nil {
		fail(c, statusOf(err), err)
		return
	}
	c.JSON(http.StatusCreated, order)
}

// Get GET /orders/:order_no —— 单表单条查询。
func (h *OrderHandler) Get(ctx context.Context, c *app.RequestContext) {
	ctx, end := h.begin(ctx, "GetOrder")
	var err error
	defer func() { end(err) }()

	order, err := h.svc.Get(ctx, c.Param("order_no"))
	if err != nil {
		fail(c, statusOf(err), err)
		return
	}
	c.JSON(http.StatusOK, order)
}

// GetWithUser GET /orders/:order_no/join —— 连表查询（orders JOIN users）。
func (h *OrderHandler) GetWithUser(ctx context.Context, c *app.RequestContext) {
	ctx, end := h.begin(ctx, "GetOrderWithUser")
	var err error
	defer func() { end(err) }()

	order, err := h.svc.GetWithUser(ctx, c.Param("order_no"))
	if err != nil {
		fail(c, statusOf(err), err)
		return
	}
	c.JSON(http.StatusOK, order)
}

// Search GET /orders/search?q=xxx —— 经 ES 检索订单。
//
// 路径刻意是独立的 /orders/search 而不是 /orders?q=：路由上 /orders/:order_no
// 会先命中 "search"，把检索请求当订单号去查库。
func (h *OrderHandler) Search(ctx context.Context, c *app.RequestContext) {
	ctx, end := h.begin(ctx, "SearchOrders")
	var err error
	defer func() { end(err) }()

	limit, _ := strconv.Atoi(c.Query("limit"))
	out, err := h.svc.Search(ctx, c.Query("q"), limit)
	if err != nil {
		fail(c, statusOf(err), err)
		return
	}
	if out == nil {
		out = []map[string]any{} // 空结果渲染成 [] 而不是 null，与 List 保持一致
	}
	c.JSON(http.StatusOK, out)
}

// Snapshot GET /orders/:order_no/snapshot —— 读该单最近一份 Mongo 快照。
func (h *OrderHandler) Snapshot(ctx context.Context, c *app.RequestContext) {
	ctx, end := h.begin(ctx, "GetOrderSnapshot")
	var err error
	defer func() { end(err) }()

	doc, err := h.svc.Snapshot(ctx, c.Param("order_no"))
	if err != nil {
		fail(c, statusOf(err), err)
		return
	}
	c.JSON(http.StatusOK, doc)
}

// Update PUT /orders/:order_no —— 更新订单状态。
func (h *OrderHandler) Update(ctx context.Context, c *app.RequestContext) {
	ctx, end := h.begin(ctx, "UpdateOrder")
	var err error
	defer func() { end(err) }()

	var req struct {
		Status string `json:"status"`
	}
	_ = c.Bind(&req)
	order, err := h.svc.UpdateStatus(ctx, c.Param("order_no"), req.Status)
	if err != nil {
		fail(c, statusOf(err), err)
		return
	}
	c.JSON(http.StatusOK, order)
}

// Delete DELETE /orders/:order_no —— 删除订单与明细。
func (h *OrderHandler) Delete(ctx context.Context, c *app.RequestContext) {
	ctx, end := h.begin(ctx, "DeleteOrder")
	var err error
	defer func() { end(err) }()

	if err := h.svc.Delete(ctx, c.Param("order_no")); err != nil {
		fail(c, statusOf(err), err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"order_no": c.Param("order_no"), "deleted": true})
}

// createRequest 创建订单请求体。缺 user_id 时默认为 0，便于 E1 的最小
// body {"item":"e2e","qty":1} 直接用（连表接口的示例 body 才需要 user_id）。
type createRequest struct {
	UserID    int64   `json:"user_id"`
	Item      string  `json:"item"`
	SKU       string  `json:"sku"`
	Qty       int     `json:"qty"`
	UnitPrice float64 `json:"unit_price"`
}

// fail 统一错误渲染：响应体只带领域文案（不泄漏内部细节）。
//
// span 状态不再由这里标 —— 交给收尾闭包 end(err)：调用方只需在函数体里把失败
// 赋给 err，不必记得"还要调一次 fail 才算标上"。
func fail(c *app.RequestContext, status int, err error) {
	c.JSON(status, map[string]string{"error": err.Error()})
}

// statusOf 领域错误 → HTTP 状态码。未知错误一律 500，不猜。
func statusOf(err error) int {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, service.ErrNotFound):
		return http.StatusNotFound
	case strings.Contains(err.Error(), "component unavailable"):
		// 组件未装配/降级：不是调用方错，也不是服务错，是环境缺件 —— 501 最贴切，
		// 返回 500 会让调用方误以为服务崩了。
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}
