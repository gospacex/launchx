// Package handler HTTP 层：只做参数绑定、状态码映射与响应封装，业务一律
// 下沉到 downstream 层；每个 handler 自开 `handler.mall.<Op>` span。
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
	"github.com/gospacex/launchx/examples/micro/pkg/auth"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	orderpb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/order"
	pointspb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/points"
	productpb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/product"
	stockpb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/stock"

	"github.com/gospacex/otelx/layer"
)

// MallHandler mall 聚合 HTTP 入口。
type MallHandler struct {
	d       *downstream.Downstream
	traceOn bool
}

// New 构造 handler。
//
// 不再收一个 trace.Tracer（与 downstream 同因）：tracer 一律向 otel 全局现取，
// 全局 provider 由 launchx 的 telemetry 步骤注册。从装配层手里接 provider 进来，
// 会让本层与 CLIENT span 分家的风险变成"取决于装配顺序"。
func New(d *downstream.Downstream, traceOn bool) *MallHandler {
	return &MallHandler{d: d, traceOn: traceOn}
}

// span 开本层 span（handler.mall.<Op>）。
//
// traceOn=false 时返回 noop span 而不是 nil：本层各处都要 `defer span.End()`
// 并把 span 传给 fail / parseInt64，返回 nil 会让每一处都得判空 —— 漏一处
// 就是 panic。noop span 的 End / RecordError / SetStatus 全是空实现。
func (h *MallHandler) span(ctx context.Context, op string) (context.Context, trace.Span) {
	if !h.traceOn {
		return ctx, trace.SpanFromContext(context.Background())
	}
	return layer.Start(ctx, "handler", "mall", op)
}

// Ping GET /ping —— 自响应，不触达下游。
//
// 它的作用是给「服务活着且 HTTP 那一跳能成 span」留一个不依赖任何下游的判据：
// 下游挂掉时 /ping 仍须 200。
func (h *MallHandler) Ping(ctx context.Context, c *app.RequestContext) {
	_, span := h.span(ctx, "Ping")
	defer span.End()
	c.JSON(http.StatusOK, map[string]string{"service": "demo-bff"})
}

// GetProduct GET /product/:id —— 经商品域取单个商品。
func (h *MallHandler) GetProduct(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.span(ctx, "GetProduct")
	defer span.End()

	id, ok := parseInt64(span, c, "id")
	if !ok {
		return
	}
	// 归属用户**只**认鉴权身份（见 userIDOf）：信请求参数就等于把"我是谁"
	// 交给客户端决定，鉴权白做。
	userID := userIDOf(ctx, c, "user_id", 0)

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
	ctx, span := h.span(ctx, "GetStock")
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
	ctx, span := h.span(ctx, "GetOrder")
	defer span.End()

	id, ok := parseInt64(span, c, "id")
	if !ok {
		return
	}
	userID := userIDOf(ctx, c, "user_id", 0)
	if userID <= 0 {
		fail(span, c, http.StatusBadRequest, errors.New("user_id required (or authenticate first)"))
		return
	}

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
//
// 路径里的 user_id **只作展示用**，归属取鉴权身份（userIDOf）：用它直查等于
// 把"查谁的账户"交给 URL 决定 —— 换一个数字就能读别人的余额（IDOR），
// 鉴权形同虚设。这就是 userIDOf 注释里写的那句"否则鉴权白做"的现实版本。
// 结果按身份过滤后再校验 > 0：既没有身份也没有声明值时，宁可 400 也不要
// 拿一个 0 去下游碰运气。
func (h *MallHandler) GetAccount(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.span(ctx, "GetAccount")
	defer span.End()

	if _, ok := parseInt64(span, c, "user_id"); !ok {
		return
	}
	userID := userIDOf(ctx, c, "user_id", 0)
	if userID <= 0 {
		fail(span, c, http.StatusBadRequest, errors.New("user_id must be positive (or authenticate first)"))
		return
	}
	r, err := h.d.GetAccount(ctx, &pointspb.GetAccountReq{UserId: userID})
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
	ctx, span := h.span(ctx, "PlaceOrder")
	defer span.End()

	var req placeOrderReq
	if err := c.BindJSON(&req); err != nil {
		fail(span, c, http.StatusBadRequest, err)
		return
	}
	// 归属用户**只**取鉴权签发的身份：请求体里的 user_id 是调用方自己声明的，
	// 信它就等于把"我是谁"交给客户端决定，鉴权白做。无身份时（auth.require=false
	// 的本机排障）才回落到请求体（与 GET 侧的查询串兜底同一口径）。
	userID := userIDOf(ctx, c, "", req.UserId)
	req.UserId = userID
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
		// 幂等键：与库存预占、积分入账**同一个号**。三个域各用各的关联号时，
		// 重放一次 = 新单 + 新预占 + 被幂等掉的积分，对账永远对不平；共用一个
		// 号之后，重试在订单域走 CreateOrderTx 的查重分支（回同一张单）。
		OrderNo: orderNo,
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
	ctx, cspan := h.span(ctx, "CompensateReleaseStock")
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
// 判据是「谁能区分故障归属，就映射谁」：**客户端错误必须映射成 4xx**，
// 只有真正的下游故障才留在 502。原因是 502 会被读成"下游挂了"，
// 把一个「你没带 user_id」渲染成 502，排查方向会被直接带偏 —— 曾经在
// 真机验收里就出现过「网关报 502，误判为 srv 不通，实际是缺 user_id」
// 的一次完整误诊。
//
// 未列出的一律按下游故障透传 502：猜出来的状态码比 502 更难排查。
func statusOf(code int64) int {
	switch code {
	// —— 幂等命中：语义是"已经做过了"，不是失败 ——
	//
	// 这几码落到 default(502) 是**有害**的误导：502 等于告诉调用方"上游坏了，
	// 可以重试"，而重放一个 5003（同订单号已入账）只会再撞一次幂等键。
	// 幂等命中一律按成功语义渲 200，让调用方把它当成"请求已生效"。
	case codes.CodePointsDuplicated, // 同订单号已入账/已消费
		codes.CodeProductStatusDup: // 上下架状态已是目标态
		return http.StatusOK
	// —— 客户端错误：参数 / 身份 / 状态冲突 ——
	case codes.CodeInvalidParam,
		codes.CodeOrderItemsEmpty,     // 下单明细为空
		codes.CodeProductTitleInvalid, // 标题非法
		codes.CodePointsAmountInvalid: // 积分数量非法
		return http.StatusBadRequest
	case codes.CodeUnauthorized:
		return http.StatusUnauthorized
	case codes.CodeNotFound,
		codes.CodeProductNotFound, codes.CodeProductOffShelf,
		codes.CodeCategoryNotFound,
		codes.CodeOrderNotFound,
		codes.CodeStockNotFound,
		codes.CodePointsAccountNF:
		return http.StatusNotFound
	case codes.CodeStockInsufficient, codes.CodePointsInsufficient,
		codes.CodeOrderCancelled, codes.CodeOrderStatus, // 状态机不允许该操作
		codes.CodeStockOverRelease,   // 释放量超过预占量
		codes.CodeStockReservationNF, // 预占流水不存在（重复释放/确认不对账）
		codes.CodeOrderStockFailed:   // 下单前置的库存预占失败
		return http.StatusConflict
	case codes.CodeRateLimited, codes.CodeUpstream,
		codes.CodeOrderConcurrent, // 同用户并发下单（可重试）
		codes.CodeStockLockFailed: // 分布式锁未获取（可重试）
		return http.StatusTooManyRequests
	// —— 熔断：基础设施故障语义，可重试且应当退避 ——
	//
	// 503 而不是 500/502：它明确表示"暂时不可用，稍后重试"，调用方才有机会
	// 按 Retry-After 退避；渲 502 会被当成"这个上游实现有问题"而放弃重试，
	// 渲 500 则与"本域写库失败"混为一谈，看日志分不清该查 DB 还是该等恢复。
	case codes.CodeStockCircuitOpen:
		return http.StatusServiceUnavailable
	// —— 服务端错误：本域写库失败 / 内部错误 ——
	case codes.CodeInternal,
		codes.CodeProductWriteFailed,
		codes.CodeStockReserveFailed,
		codes.CodePointsWriteFailed:
		return http.StatusInternalServerError
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

// userIDOf 取本次请求的归属用户。
//
// 优先级：**鉴权签发的身份 > 请求里自带的 user_id > 调用方给的 fallback**。
// 请求自带值只在没有身份时兜底（auth.require=false 的本机排障路径），有身份时
// 一律以身份为准 —— 否则客户端只要在 URL/body 上换个 user_id 就能读别人的订单，
// 鉴权形同虚设。
//
// key 传 "" 表示不从请求里取值（例如 POST 的 body 字段已由调用方解析好，
// 经 fallback 传入）。返回 0 表示拿不到任何主体，调用方须显式拦下，
// 不要拿 0 去下游碰运气。
func userIDOf(ctx context.Context, c *app.RequestContext, key string, fallback int64) int64 {
	if id, ok := auth.IdentityFrom(ctx); ok && id.UserID > 0 {
		return id.UserID
	}
	if key != "" {
		// 路径参数优先于查询串：商品/订单/积分三处路由里 user_id 的落点不统一，
		// 只看 Query 会让路径形态的取值恒为 0。
		for _, raw := range []string{c.Param(key), c.Query(key)} {
			if raw == "" {
				continue
			}
			if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return fallback
}
