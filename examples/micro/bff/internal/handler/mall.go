// Package handler HTTP 层：只做参数绑定、状态码映射与响应封装，业务一律
// 下沉到 downstream 层；每个 handler 自开 `handler.*` span。
//
// 复合编排 PlaceOrder 是全链路验收的锚点：一次 HTTP 请求把
// 商品 → 库存 → 订单 → 积分 四个域服串进同一根 trace，
// 失败路径还会补一次库存释放（补偿也入链，可整段回放）。
//
// 本包是 webx/hertz 唯一出现的地方（handler 签名是 hertz 的 app.HandlerFunc）。
package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/micro/bff/internal/downstream"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	orderpb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/order"
	pointspb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/points"
	productpb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/product"
	stockpb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/stock"
)

// MallHandler mall 聚合 HTTP 入口。
type MallHandler struct {
	d      *downstream.Downstream
	tracer trace.Tracer
}

// New 构造 handler。
func New(d *downstream.Downstream, tracer trace.Tracer) *MallHandler {
	return &MallHandler{d: d, tracer: tracer}
}

// Ping GET /ping —— 自响应，不触达下游。
//
// 它的作用是给「服务活着且 HTTP 那一跳能成 span」留一个不依赖任何下游的判据：
// 下游挂掉时 /ping 仍须 200。
func (h *MallHandler) Ping(ctx context.Context, c *app.RequestContext) {
	_, span := h.tracer.Start(ctx, "handler.Ping")
	defer span.End()
	c.JSON(http.StatusOK, map[string]string{"service": "demo-bff"})
}

// GetProduct GET /product/:id —— 经商品域取单个商品。
func (h *MallHandler) GetProduct(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.GetProduct")
	defer span.End()

	id, ok := parseInt64(span, c, "id")
	if !ok {
		return
	}
	userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)

	r, err := h.d.GetProduct(ctx, &productpb.GetProductReq{UserId: userID, ProductId: id})
	if err != nil {
		fail(span, c, http.StatusBadGateway, err)
		return
	}
	if code := r.GetCode(); code != codes.CodeOK {
		fail(span, c, statusOf(code), errors.New(r.GetMessage()))
		return
	}
	c.JSON(http.StatusOK, r)
}

// GetStock GET /stock/:product_id —— 经库存域查库存快照。
func (h *MallHandler) GetStock(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.GetStock")
	defer span.End()

	id, ok := parseInt64(span, c, "product_id")
	if !ok {
		return
	}
	r, err := h.d.GetStock(ctx, &stockpb.GetStockReq{ProductId: id})
	if err != nil {
		fail(span, c, http.StatusBadGateway, err)
		return
	}
	if code := r.GetCode(); code != codes.CodeOK {
		fail(span, c, statusOf(code), errors.New(r.GetMessage()))
		return
	}
	c.JSON(http.StatusOK, r)
}

// GetOrder GET /order/:id —— 经订单域取单个订单 + 明细。
func (h *MallHandler) GetOrder(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.GetOrder")
	defer span.End()

	id, ok := parseInt64(span, c, "id")
	if !ok {
		return
	}
	userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)

	r, err := h.d.GetOrder(ctx, &orderpb.GetOrderReq{OrderId: id, UserId: userID})
	if err != nil {
		fail(span, c, http.StatusBadGateway, err)
		return
	}
	if code := r.GetCode(); code != codes.CodeOK {
		fail(span, c, statusOf(code), errors.New(r.GetMessage()))
		return
	}
	c.JSON(http.StatusOK, r)
}

// GetAccount GET /points/:user_id —— 经积分域查积分账户。
func (h *MallHandler) GetAccount(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.GetAccount")
	defer span.End()

	id, ok := parseInt64(span, c, "user_id")
	if !ok {
		return
	}
	r, err := h.d.GetAccount(ctx, &pointspb.GetAccountReq{UserId: id})
	if err != nil {
		fail(span, c, http.StatusBadGateway, err)
		return
	}
	if code := r.GetCode(); code != codes.CodeOK {
		fail(span, c, statusOf(code), errors.New(r.GetMessage()))
		return
	}
	c.JSON(http.StatusOK, r)
}

// placeOrderReq POST /order 请求体。
type placeOrderReq struct {
	UserId    int64 `json:"user_id"`
	ProductID int64 `json:"product_id"`
	Quantity  int32 `json:"quantity"`
}

// PlaceOrder POST /order —— 复合编排：一次请求串起四个域服。
//
// 成功路径（同一根 trace 上的顺序）：
//
//	handler.PlaceOrder（bff 根 span 由 webx otel 中间件开）
//	├── downstream.product.GetProduct   → srv-product（服务端 suite span + service/db/cache span）
//	├── downstream.stock.ReserveStock   → srv-stock（锁 + 3 表事务 + outbox）
//	├── downstream.order.CreateOrder    → srv-order（3 表事务 + outbox）
//	└── downstream.points.Earn          → srv-points（幂等键 + 2 表事务）
//
// 失败补偿：库存已预占而订单没建成时，必须 ReleaseStock 回滚预占 ——
// 补偿也走同一条 trace（span 属性 biz.compensation=true），排查时整段可回放。
// 成功后积分入账用同一 orderNo：srv-points 的唯一键使重放天然幂等。
func (h *MallHandler) PlaceOrder(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.PlaceOrder")
	defer span.End()

	var req placeOrderReq
	if err := c.BindJSON(&req); err != nil {
		fail(span, c, http.StatusBadRequest, err)
		return
	}
	if req.UserId <= 0 || req.ProductID <= 0 || req.Quantity <= 0 {
		fail(span, c, http.StatusBadRequest, errors.New("user_id/product_id/quantity must be positive"))
		return
	}
	orderNo := newOrderNo()
	span.SetAttributes(
		attribute.String("biz.order_no", orderNo),
		attribute.Int64("biz.user_id", req.UserId),
		attribute.Int64("biz.product_id", req.ProductID),
	)

	// ① 商品域：存在且上架才往下走（也是给整条链一个便宜的预检）。
	p, err := h.d.GetProduct(ctx, &productpb.GetProductReq{UserId: req.UserId, ProductId: req.ProductID})
	if err != nil {
		fail(span, c, http.StatusBadGateway, err)
		return
	}
	if code := p.GetCode(); code != codes.CodeOK {
		fail(span, c, statusOf(code), errors.New(p.GetMessage()))
		return
	}

	// ② 库存域：预占（失败即终止，没有需要补偿的东西）。
	rs, err := h.d.ReserveStock(ctx, &stockpb.ReserveStockReq{
		UserId: req.UserId, OrderNo: orderNo, ProductId: req.ProductID, Quantity: req.Quantity,
	})
	if err != nil {
		fail(span, c, http.StatusBadGateway, err)
		return
	}
	if code := rs.GetCode(); code != codes.CodeOK {
		fail(span, c, statusOf(code), errors.New(rs.GetMessage()))
		return
	}

	// ③④ 订单 + 积分：任一失败都要释放预占（补偿入链）。
	r, err := h.d.CreateOrder(ctx, &orderpb.CreateOrderReq{
		UserId: req.UserId,
		Items: []*orderpb.OrderItem{{
			ProductId: req.ProductID,
			Name:      p.GetProduct().GetTitle(),
			Quantity:  req.Quantity,
			UnitPrice: p.GetProduct().GetPrice(),
		}},
	})
	if err != nil {
		h.compensate(ctx, span, req, orderNo)
		fail(span, c, http.StatusBadGateway, err)
		return
	}
	if code := r.GetCode(); code != codes.CodeOK {
		h.compensate(ctx, span, req, orderNo)
		fail(span, c, statusOf(code), errors.New(r.GetMessage()))
		return
	}

	// 积分入账是「尽力而为」：失败不回滚订单（事件侧还有 outbox 兜底重试的
	// 语义位），但在 trace 上必须留痕，否则对账时会莫名其妙。
	pe, err := h.d.Earn(ctx, &pointspb.EarnReq{
		UserId: req.UserId, OrderNo: orderNo,
		Amount: r.GetTotalAmount() / 100, // 1% 返积分（示例口径），分→积分取整
		Remark: "place-order",
	})
	if err != nil {
		span.RecordError(err)
		span.SetAttributes(attribute.Bool("biz.points_failed", true))
	} else if code := pe.GetCode(); code != codes.CodeOK {
		span.SetAttributes(attribute.Bool("biz.points_failed", true))
	}

	c.JSON(http.StatusOK, map[string]any{
		"code":           codes.CodeOK,
		"order_id":       r.GetOrderId(),
		"order_no":       r.GetOrderNo(),
		"total_amount":   r.GetTotalAmount(),
		"reservation_id": rs.GetReservationId(),
	})
}

// compensate 库存预占的补偿路径：biz.compensation=true 让它在 Jaeger 上
// 一眼可辨。补偿失败只记进 span —— 补偿失败的善后属于对账域，不属于本次请求。
func (h *MallHandler) compensate(ctx context.Context, span trace.Span, req placeOrderReq, orderNo string) {
	ctx, cspan := h.tracer.Start(ctx, "handler.ReleaseStock.compensate")
	defer cspan.End()
	cspan.SetAttributes(
		attribute.Bool("biz.compensation", true),
		attribute.String("biz.order_no", orderNo),
	)
	r, err := h.d.ReleaseStock(ctx, &stockpb.ReleaseStockReq{
		OrderNo: orderNo, ProductId: req.ProductID, Quantity: req.Quantity,
	})
	switch {
	case err != nil:
		cspan.RecordError(err)
		cspan.SetStatus(otelcodes.Error, err.Error())
	default:
		// 领域码非 0 也是失败：补偿没成功必须在对账时能检索到。
		if code := r.GetCode(); code != codes.CodeOK {
			cspan.SetStatus(otelcodes.Error, fmt.Sprintf("code=%d msg=%s", code, r.GetMessage()))
		}
	}
}

// parseInt64 解析路径参数：解析失败直接渲染 400 并标记 span（各 handler 共用）。
func parseInt64(span trace.Span, c *app.RequestContext, name string) (int64, bool) {
	v, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || v <= 0 {
		err = errors.New("path param " + name + " must be a positive integer")
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		return 0, false
	}
	return v, true
}

// statusOf 领域错误码 → HTTP 状态码。
//
// 只映射语义确定的两类，其余一律按下游故障透传 502 —— 猜出来的状态码比
// 502 更难排查。
func statusOf(code int64) int {
	switch code {
	case codes.CodeInvalidParam:
		return http.StatusBadRequest
	case codes.CodeNotFound,
		codes.CodeProductNotFound, codes.CodeProductOffShelf,
		codes.CodeOrderNotFound,
		codes.CodeStockNotFound,
		codes.CodePointsAccountNF:
		return http.StatusNotFound
	case codes.CodeRateLimited, codes.CodeOrderConcurrent:
		return http.StatusTooManyRequests
	case codes.CodeStockInsufficient, codes.CodePointsInsufficient:
		return http.StatusConflict
	default:
		return http.StatusBadGateway
	}
}

// fail 统一错误渲染：错误码记进 span，响应体只带文案（不泄漏内部细节）。
func fail(span trace.Span, c *app.RequestContext, status int, err error) {
	span.RecordError(err)
	span.SetStatus(otelcodes.Error, err.Error())
	c.JSON(status, map[string]string{"error": err.Error()})
}

// newOrderNo 生成 bff 侧的幂等单号：订单域的锁按 user 维度，库存/积分的
// 幂等键按 orderNo 维度，同一单重放不会重复扣减。
func newOrderNo() string {
	return "bff" + strconv.FormatInt(time.Now().UnixNano(), 36)
}
