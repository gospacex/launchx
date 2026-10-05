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
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/monolith/internal/model"
	"github.com/gospacex/launchx/examples/monolith/internal/service"
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

// List GET /orders —— 单表查询入口（E1 的 JSON 数组断言落在这里）。
func (h *OrderHandler) List(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.ListOrders")
	defer span.End()

	limit, _ := strconv.Atoi(c.Query("limit"))
	orders, err := h.svc.List(ctx, limit)
	if err != nil {
		fail(span, c, http.StatusInternalServerError, err)
		return
	}
	if orders == nil {
		orders = []model.Order{} // 空结果渲染成 [] 而不是 null
	}
	c.JSON(http.StatusOK, orders)
}

// Create POST /orders —— 创建订单（事务写 orders + order_items）。
func (h *OrderHandler) Create(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.CreateOrder")
	defer span.End()

	var req createRequest
	if err := c.Bind(&req); err != nil {
		fail(span, c, http.StatusBadRequest, service.ErrInvalidInput)
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
		fail(span, c, statusOf(err), err)
		return
	}
	c.JSON(http.StatusCreated, order)
}

// Get GET /orders/:order_no —— 单表单条查询。
func (h *OrderHandler) Get(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.GetOrder")
	defer span.End()

	order, err := h.svc.Get(ctx, c.Param("order_no"))
	if err != nil {
		fail(span, c, statusOf(err), err)
		return
	}
	c.JSON(http.StatusOK, order)
}

// GetWithUser GET /orders/:order_no/join —— 连表查询（orders JOIN users）。
func (h *OrderHandler) GetWithUser(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.GetOrderWithUser")
	defer span.End()

	order, err := h.svc.GetWithUser(ctx, c.Param("order_no"))
	if err != nil {
		fail(span, c, statusOf(err), err)
		return
	}
	c.JSON(http.StatusOK, order)
}

// Search GET /orders/search?q=xxx —— 经 ES 检索订单。
//
// 路径刻意是独立的 /orders/search 而不是 /orders?q=：路由上 /orders/:order_no
// 会先命中 "search"，把检索请求当订单号去查库。
func (h *OrderHandler) Search(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.SearchOrders")
	defer span.End()

	limit, _ := strconv.Atoi(c.Query("limit"))
	out, err := h.svc.Search(ctx, c.Query("q"), limit)
	if err != nil {
		fail(span, c, statusOf(err), err)
		return
	}
	if out == nil {
		out = []map[string]any{} // 空结果渲染成 [] 而不是 null，与 List 保持一致
	}
	c.JSON(http.StatusOK, out)
}

// Snapshot GET /orders/:order_no/snapshot —— 读该单最近一份 Mongo 快照。
func (h *OrderHandler) Snapshot(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.GetOrderSnapshot")
	defer span.End()

	doc, err := h.svc.Snapshot(ctx, c.Param("order_no"))
	if err != nil {
		fail(span, c, statusOf(err), err)
		return
	}
	c.JSON(http.StatusOK, doc)
}

// Update PUT /orders/:order_no —— 更新订单状态。
func (h *OrderHandler) Update(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.UpdateOrder")
	defer span.End()

	var req struct {
		Status string `json:"status"`
	}
	_ = c.Bind(&req)
	order, err := h.svc.UpdateStatus(ctx, c.Param("order_no"), req.Status)
	if err != nil {
		fail(span, c, statusOf(err), err)
		return
	}
	c.JSON(http.StatusOK, order)
}

// Delete DELETE /orders/:order_no —— 删除订单与明细。
func (h *OrderHandler) Delete(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.DeleteOrder")
	defer span.End()

	if err := h.svc.Delete(ctx, c.Param("order_no")); err != nil {
		fail(span, c, statusOf(err), err)
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

// fail 统一错误渲染：错误码记进 span，响应体只带领域文案（不泄漏内部细节）。
func fail(span trace.Span, c *app.RequestContext, status int, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
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
