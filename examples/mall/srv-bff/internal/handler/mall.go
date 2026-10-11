// Package handler HTTP 层：只做参数绑定、状态码映射与响应封装，业务一律
// 下沉到 downstream 层；每个 handler 自开 `handler.bff.<Op>` span。
//
// 复合编排 PlaceOrder 是全链路验收的锚点：一次 HTTP 请求把
// 商品 → 库存 → 订单 → 积分 四个域服串进同一根 trace，
// 失败路径还会补一次库存释放（补偿也入链，可整段回放）。
//
// 本包是 webx/hertz 唯一出现的地方（handler 签名是 hertz 的 app.HandlerFunc）。
//
// 身份纪律（本包最重要的一条）：user_id **只从 ctx 里的身份取**，不经请求
// 参数。请求参数里的 user_id 由 pkg/auth 中间件在进 handler 之前校验并覆盖 ——
// 直接从 query/body 取等于把"我是谁"交给调用方声明，鉴权白做（IDOR）。
package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/mall/pkg/auth"
	"github.com/gospacex/launchx/examples/mall/pkg/codes"
	orderpb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/order"
	pointspb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/points"
	productpb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/product"
	stockpb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/stock"
	"github.com/gospacex/launchx/examples/mall/pkg/routing"
	"github.com/gospacex/launchx/examples/mall/srv-bff/internal/downstream"
)

// productStatusOnSale 商品「上架」状态值。
//
// 与 srv-product 的 model.StatusOnSale 同义（1 上架 / 2 下架，见
// scripts/schema.sql 的 products.status 注释）。bff **不 import** 任何 srv
// 的 internal（那是别人的私有库），跨域只认 ID 与这类契约值，故在本侧重新
// 申明一次并把出处写进注释 —— 抄错值的症状是"下架商品还能下单"。
const productStatusOnSale int32 = 1

// MallHandler mall 聚合 HTTP 入口。
type MallHandler struct {
	d      *downstream.Downstream
	tracer trace.Tracer
	// router 入口路由表（audit / rate_limit 中间件链）。nil 时各 handler 直跑，
	// 只是少一层可观测 —— 不因此拒绝服务。
	router *routing.Router
	logger logx.Logger
}

// New 构造 handler。
func New(d *downstream.Downstream, tracer trace.Tracer, router *routing.Router, logger logx.Logger) *MallHandler {
	if logger == nil {
		logger = logx.GetGlobal()
	}
	return &MallHandler{d: d, tracer: tracer, router: router, logger: logger}
}

// Ping GET /ping —— 自响应，不触达下游。
//
// 它的作用是给「服务活着且 HTTP 那一跳能成 span」留一个不依赖任何下游的判据：
// 下游挂掉时 /ping 仍须 200。同理它豁免鉴权（见 app.Setup 的 authSkip）。
func (h *MallHandler) Ping(ctx context.Context, c *app.RequestContext) {
	_, span := h.tracer.Start(ctx, "handler.bff.Ping")
	defer span.End()
	c.JSON(http.StatusOK, map[string]string{"service": "mall-bff"})
}

// GetProduct GET /product/:id —— 经商品域取单个商品。
func (h *MallHandler) GetProduct(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.bff.GetProduct")
	defer span.End()

	id, ok := parseInt64(span, c, "id")
	if !ok {
		return
	}
	userID := identityOr(ctx, 0)
	v := h.enter(ctx, "GetProduct", userID)
	defer v.End()
	if !v.Allowed {
		rejected(span, c, v.AbortedBy)
		return
	}

	r, err := h.d.GetProduct(v.Ctx, &productpb.GetProductReq{UserId: userID, ProductId: id})
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
//
// 该路径没有 user_id（库存是商品维度的数据），故限流主体取 0 号共享桶 ——
// 它的配额被刻意压到远小于 per-user（见 app.Setup 的 rateLimitAnonymous）。
func (h *MallHandler) GetStock(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.bff.GetStock")
	defer span.End()

	id, ok := parseInt64(span, c, "product_id")
	if !ok {
		return
	}
	v := h.enter(ctx, "GetStock", identityOr(ctx, 0))
	defer v.End()
	if !v.Allowed {
		rejected(span, c, v.AbortedBy)
		return
	}

	r, err := h.d.GetStock(v.Ctx, &stockpb.GetStockReq{ProductId: id})
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
//
// 订单是用户私有数据：user_id 一律取身份，调用方在 query 里带什么都不作数。
func (h *MallHandler) GetOrder(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.bff.GetOrder")
	defer span.End()

	id, ok := parseInt64(span, c, "id")
	if !ok {
		return
	}
	userID := identityOr(ctx, 0)
	v := h.enter(ctx, "GetOrder", userID)
	defer v.End()
	if !v.Allowed {
		rejected(span, c, v.AbortedBy)
		return
	}

	r, err := h.d.GetOrder(v.Ctx, &orderpb.GetOrderReq{OrderId: id, UserId: userID})
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
// 路径里的 user_id 已由 pkg/auth 与身份比对过（不符直接 403），这里仍以身份
// 为准再取一次：中间件的覆盖是对"漏改的 handler"的兜底，本 handler 不该依赖
// 兜底。
func (h *MallHandler) GetAccount(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.bff.GetAccount")
	defer span.End()

	id, ok := parseInt64(span, c, "user_id")
	if !ok {
		return
	}
	userID := identityOr(ctx, id)
	v := h.enter(ctx, "GetAccount", userID)
	defer v.End()
	if !v.Allowed {
		rejected(span, c, v.AbortedBy)
		return
	}

	r, err := h.d.GetAccount(v.Ctx, &pointspb.GetAccountReq{UserId: userID})
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
//
// UserId 只作为「无身份时的回落」使用（auth.require=false 的本机排障路径）：
// 有身份时一律以身份为准（见 PlaceOrder），调用方自带的值与身份不符会在
// 中间件层直接 403。
type placeOrderReq struct {
	UserId    int64 `json:"user_id"`
	ProductID int64 `json:"product_id"`
	Quantity  int32 `json:"quantity"`
}

// PlaceOrder POST /order —— 复合编排：一次请求串起四个域服。
//
// 成功路径（同一根 trace 上的顺序）：
//
//	handler.bff.PlaceOrder（bff 根 span 由 webx otel 中间件开）
//	├── route.bff.PlaceOrder → middleware.bff.audit → middleware.bff.rate_limit
//	├── downstream.product.GetProduct   → srv-product（服务端 suite span + service/db/cache span）
//	├── downstream.stock.ReserveStock   → srv-stock（锁 + 3 表事务 + outbox）
//	├── downstream.order.CreateOrder    → srv-order（3 表事务 + outbox）
//	└── downstream.points.Earn          → srv-points（幂等键 + 2 表事务）
//
// 失败补偿：库存已预占而订单**被明确拒绝**时，才 ReleaseStock 回滚预占 ——
// 补偿也走同一条 trace（span 属性 biz.compensation=true），排查时整段可回放。
// 传输失败（超时）不补偿，理由见 settle。
func (h *MallHandler) PlaceOrder(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.bff.PlaceOrder")
	defer span.End()

	var req placeOrderReq
	if err := c.BindJSON(&req); err != nil {
		fail(span, c, http.StatusBadRequest, err)
		return
	}
	userID := identityOr(ctx, req.UserId)
	if userID <= 0 || req.ProductID <= 0 || req.Quantity <= 0 {
		fail(span, c, http.StatusBadRequest, errors.New("user_id/product_id/quantity must be positive"))
		return
	}
	orderNo := newOrderNo()
	span.SetAttributes(
		attribute.String("biz.order_no", orderNo),
		attribute.Int64("biz.user_id", userID),
		attribute.Int64("biz.product_id", req.ProductID),
	)

	v := h.enter(ctx, "PlaceOrder", userID)
	defer v.End()
	if !v.Allowed {
		rejected(span, c, v.AbortedBy)
		return
	}
	ctx = v.Ctx

	// ① 商品域：存在 **且上架** 才往下走。
	//
	// 只看 code==CodeOK 是不够的：商品域对已下架商品照样返回 0（下架不是查询
	// 失败，它是一个合法状态值），于是 statusOf 里的 CodeProductOffShelf 分支
	// 永远不会从这条路径上走到（死分支）。下单必须自己判状态 ——
	// 不判就是"下架商品还能被买"，且库存侧已经先扣了。
	p, err := h.d.GetProduct(ctx, &productpb.GetProductReq{UserId: userID, ProductId: req.ProductID})
	if err != nil {
		fail(span, c, http.StatusBadGateway, err)
		return
	}
	if code := p.GetCode(); code != codes.CodeOK {
		fail(span, c, statusOf(code), errors.New(p.GetMessage()))
		return
	}
	if st := p.GetProduct().GetStatus(); st != productStatusOnSale {
		// 400 而不是 404：商品**存在**（详情接口仍可查，故那边是 404 的语义），
		// 但"对下架商品下单"是一次非法请求 —— 用 404 会让调用方以为商品没了、
		// 转而去重试或换 ID，掩盖真正的处置动作（先上架）。
		span.SetAttributes(
			attribute.Int64("biz.product_status", int64(st)),
			attribute.Bool("biz.product_off_shelf", true),
		)
		fail(span, c, http.StatusBadRequest,
			errors.New(fmt.Sprintf("product %d is not on sale (status=%d)", req.ProductID, st)))
		return
	}

	// ② 库存域：预占（失败即终止，没有需要补偿的东西）。
	rs, err := h.d.ReserveStock(ctx, &stockpb.ReserveStockReq{
		UserId: userID, OrderNo: orderNo, ProductId: req.ProductID, Quantity: req.Quantity,
	})
	if err != nil {
		// 预占这一步自身超时也不补偿：预占是否生效未知，此时去释放可能把一次
		// 成功的预占放掉（库存被别人买走），也可能什么都没发生。留给对账。
		span.SetAttributes(attribute.String("biz.compensation", "skipped_uncertain"))
		h.logger.WarnContext(ctx, "place order: reserve result unknown, compensation skipped",
			logx.String("order_no", orderNo),
			logx.String("consequence", "a reservation may stay open; reconcile by order_no"),
			logx.Err(err))
		fail(span, c, http.StatusBadGateway, err)
		return
	}
	if code := rs.GetCode(); code != codes.CodeOK {
		fail(span, c, statusOf(code), errors.New(rs.GetMessage()))
		return
	}

	// ③④ 订单 + 积分：只有**明确收到非 0 业务码**才释放预占。
	r, err := h.d.CreateOrder(ctx, &orderpb.CreateOrderReq{
		UserId: userID,
		// 幂等键：与库存预占、积分入账**同一个号**。三个域各用各的关联号时，
		// 重放一次 = 新单 + 新预占 + 被幂等掉的积分，对账永远对不平；共用一个
		// 号之后，重试在订单域走 CreateOrderTx 的查重分支（回同一张单）。
		// 它同时也是 outcomeUncertain 分支下"这单到底建没建"的唯一查询键。
		OrderNo: orderNo,
		Items: []*orderpb.OrderItem{{
			ProductId: req.ProductID,
			Name:      p.GetProduct().GetTitle(),
			Quantity:  req.Quantity,
			UnitPrice: p.GetProduct().GetPrice(),
		}},
	})
	switch settle(err, r.GetCode()) {
	case outcomeUncertain:
		// 超时/不可达：请求**可能已经到了订单域**。此时释放预占，等于在订单
		// 可能已建成的情况下把库存放掉（有单无货）。宁可留一条待对账的悬空
		// 预占，也不能制造账实不符 —— 前者有 order_no 可查，后者要人工翻库。
		span.SetAttributes(attribute.String("biz.compensation", "skipped_uncertain"))
		h.logger.WarnContext(ctx, "place order: create result unknown, compensation skipped",
			logx.String("order_no", orderNo),
			logx.String("consequence", "reservation stays open on purpose; reconcile by order_no"),
			logx.Err(err))
		fail(span, c, http.StatusBadGateway, err)
		return
	case outcomeRejected:
		h.compensate(ctx, span, req.ProductID, req.Quantity, orderNo)
		fail(span, c, statusOf(r.GetCode()), errors.New(r.GetMessage()))
		return
	}

	// 积分入账是「尽力而为」：失败不回滚订单（事件侧还有 outbox 兜底重试的
	// 语义位），但在 trace 上必须留痕，否则对账时会莫名其妙。
	h.earnPoints(ctx, span, userID, orderNo, r.GetTotalAmount())

	c.JSON(http.StatusOK, map[string]any{
		"code":           codes.CodeOK,
		"order_id":       r.GetOrderId(),
		"order_no":       r.GetOrderNo(),
		"total_amount":   r.GetTotalAmount(),
		"reservation_id": rs.GetReservationId(),
	})
}

// earnPoints 订单成功后的积分入账（尽力而为，失败只留痕不回滚）。
//
// 两条"这不是失败"的分支刻意与真失败分开标记：
//   - 金额折算后 <= 0（总价 < 100 分的低价单）：**根本不发请求**。发出去只会
//     撞积分域的 `Amount <= 0` 校验（5005），于是每一笔正常低价单都在链路上
//     挂一个假失败标记 —— 告警按 biz.points_failed 扫时会全是指标噪声。
//   - 5003 CodePointsDuplicated：语义是「已入账，本次已忽略」，是幂等命中而
//     不是故障。记 biz.points_duplicated 供对账核对，绝不记成失败。
func (h *MallHandler) earnPoints(ctx context.Context, span trace.Span, userID int64, orderNo string, totalAmount int64) {
	amount := totalAmount / 100 // 1% 返积分（示例口径），分→积分取整
	if amount <= 0 {
		span.SetAttributes(
			attribute.Bool("biz.points_skipped", true),
			attribute.Int64("biz.points_amount", 0),
			attribute.String("biz.points_skip_reason", "amount_below_one_point"),
		)
		return
	}
	pe, err := h.d.Earn(ctx, &pointspb.EarnReq{
		UserId: userID, OrderNo: orderNo,
		Amount: amount,
		Remark: "place-order",
	})
	if err != nil {
		span.RecordError(err)
		span.SetAttributes(attribute.Bool("biz.points_failed", true))
		return
	}
	switch code := pe.GetCode(); code {
	case codes.CodeOK:
	case codes.CodePointsDuplicated:
		// 幂等命中 = 这笔积分已经在账上。记账面上它与成功同义，
		// 只是这次没有产生新的流水。
		span.SetAttributes(
			attribute.Bool("biz.points_duplicated", true),
			attribute.String("biz.order_no", orderNo),
		)
	default:
		span.SetAttributes(attribute.Bool("biz.points_failed", true))
	}
}

// outcome 一次下游调用的结果分类（settle 的产出）。
type outcome int

// 结果分类取值
const (
	// outcomeOK 业务成功（有响应且业务码为 0）
	outcomeOK outcome = iota
	// outcomeRejected 明确收到非 0 业务码：下游给了确定答复，据此可安全补偿
	outcomeRejected
	// outcomeUncertain 传输/内部失败（超时、不可达）：**结果未知**，不得补偿
	outcomeUncertain
)

// String 稳定字符串（进 span 属性，别用 %d —— 属性是给人看的）
func (o outcome) String() string {
	switch o {
	case outcomeRejected:
		return "rejected"
	case outcomeUncertain:
		return "uncertain"
	default:
		return "ok"
	}
}

// settle 把「传输错误 err」与「业务码 code」分流成一种结果。
//
// 这是本案例「业务码走响应体、kitex error 只承载传输/内部失败」这条分工的
// 直接后果，也是补偿决策的前提 —— 两种失败的处置动作**相反**，必须在同一处
// 被区分开：
//
//	err != nil（outcomeUncertain）  请求可能已经到了下游，也可能没到。此时
//	    去补偿等于赌它没到，赌输了就是"有单无货"。
//	code != 0（outcomeRejected）    下游明确拒绝了，副作用状态是确定的
//	    （预占确实还在），补偿是安全的。
//
// 把两者混成一条 if 的代价不是"多补偿几次"，而是偶发地把已建成的订单的库存
// 放掉 —— 那是账实不符，且事后只能翻库对。
func settle(err error, code int64) outcome {
	if err != nil {
		return outcomeUncertain
	}
	if code != codes.CodeOK {
		return outcomeRejected
	}
	return outcomeOK
}

// stockReleaser 补偿路径只依赖"释放预占"这一个能力。
//
// 声明窄接口而不是直接吃 *downstream.Downstream：补偿是最需要被单测覆盖的
// 分支（它跑在已经失败一半的请求上），而 Downstream 需要一个真的 kitex
// 客户端才能构造。窄接口让"补偿被 4005 幂等命中"这类用例可以直接驱动。
type stockReleaser interface {
	ReleaseStock(ctx context.Context, req *stockpb.ReleaseStockReq) (*stockpb.ReleaseStockResp, error)
}

// compensate 库存预占的补偿路径：biz.compensation=true 让它在 Jaeger 上
// 一眼可辨。补偿失败只记进 span —— 补偿失败的善后属于对账域，不属于本次请求。
//
// 4005（预占流水不存在）按**成功**处理：它意味着这一单的预占已经不在了
// （已被释放/确认过），正是补偿想要达到的状态。把它记成 Error 会让"补偿成功"
// 在 Jaeger 上和"补偿失败"同色，按 Error 扫出来的全是噪声。
func (h *MallHandler) compensate(ctx context.Context, span trace.Span, productID int64, quantity int32, orderNo string) {
	h.compensateWith(ctx, span, h.d, productID, quantity, orderNo)
}

// compensateWith 补偿的实际实现（把下游抽成参数，便于单测驱动）。
func (h *MallHandler) compensateWith(ctx context.Context, span trace.Span, d stockReleaser, productID int64, quantity int32, orderNo string) {
	ctx, cspan := h.tracer.Start(ctx, "handler.bff.ReleaseStock.compensate")
	defer cspan.End()
	cspan.SetAttributes(
		attribute.Bool("biz.compensation", true),
		attribute.String("biz.order_no", orderNo),
	)
	r, err := d.ReleaseStock(ctx, &stockpb.ReleaseStockReq{
		OrderNo: orderNo, ProductId: productID, Quantity: quantity,
	})
	switch {
	case err != nil:
		cspan.RecordError(err)
		cspan.SetStatus(otelcodes.Error, err.Error())
	case r.GetCode() == codes.CodeOK, r.GetCode() == codes.CodeStockReservationNF:
		cspan.SetAttributes(attribute.Int64("biz.compensation_code", r.GetCode()))
	default:
		// 领域码非 0 也是失败：补偿没成功必须在对账时能检索到。
		cspan.SetStatus(otelcodes.Error, fmt.Sprintf("code=%d msg=%s", r.GetCode(), r.GetMessage()))
	}
}

// enter 走一次路由（开 route span + audit / rate_limit 中间件链）。
//
// router 为 nil（装配未接路由表）时直接放行：路由表是可观测与治理的挂载点，
// 不是正确性的依赖项 —— 缺了它该退化成"没有这层观测"，不是 500。
func (h *MallHandler) enter(ctx context.Context, method string, subject int64) *routing.Visit {
	if h.router == nil {
		return &routing.Visit{Ctx: ctx, Allowed: true}
	}
	return h.router.Enter(ctx, method, subject)
}

// identityOr 取 ctx 里的身份；无身份（auth.require=false 的排障路径）时回落到
// fallback。
//
// 回落值是"没有身份"这一事实的表达，不是默认值兜底：auth.require=false 只对
// 可信网络开放，此时调用方自带 user_id 与旧的（无鉴权）行为一致。
func identityOr(ctx context.Context, fallback int64) int64 {
	if id, ok := auth.IdentityFrom(ctx); ok {
		return id.UserID
	}
	return fallback
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
// 只映射语义确定的几类，其余一律按下游故障透传 502 —— 猜出来的状态码比
// 502 更难排查。
func statusOf(code int64) int {
	switch code {
	case codes.CodeInvalidParam:
		return http.StatusBadRequest
	case codes.CodeUnauthorized:
		return http.StatusUnauthorized
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

// rejected 渲染被中间件链拒绝的响应。
//
// 只在限流位上返回 429：audit 恒放行，故目前能走到这里的只有 rate_limit。
// 保留 abortedBy 进日志是为了将来加中间件时"是谁拒的"仍然可答。
func rejected(span trace.Span, c *app.RequestContext, abortedBy string) {
	err := fmt.Errorf("request rejected by middleware %q", abortedBy)
	span.RecordError(err)
	span.SetStatus(otelcodes.Error, err.Error())
	c.JSON(http.StatusTooManyRequests, map[string]string{"error": "too many requests"})
}

// fail 统一错误渲染：错误码记进 span，响应体只带文案（不泄漏内部细节）。
func fail(span trace.Span, c *app.RequestContext, status int, err error) {
	span.RecordError(err)
	span.SetStatus(otelcodes.Error, err.Error())
	c.JSON(status, map[string]string{"error": err.Error()})
}

// newOrderNo 生成 bff 侧的幂等单号。
//
// 它之所以能保证"同一单重放不会重复扣减"，靠的是**两侧**而不是单号本身唯一：
//  1. 库存侧：stock_reservations 上有 uk_sr_order_product(order_no, product_id)
//     唯一键，且 repository.ReserveTx 在事务内先查同键流水，命中即按重放返回
//     （回既有 reservation_id 与当前 available，不再扣减）—— 单号重复只会拿到
//     同一条预占，不会扣第二份。
//  2. 积分侧：point_ledgers 的 uk_ledger_order_type(order_no, type) 让同一单
//     同一类型的入账只能记一次，重放返回 5003（已入账、已忽略）。
//
// 换句话说：单号是幂等键的**载体**，真正的幂等由数据库唯一键兜底。这也是
// 为什么这里用时间戳 + 进程内自增就够了 —— 它不需要全局唯一，只需要在"同一
// 次业务重放"里保持一致（真重放走的是同一个 order_no，不是重新生成）。
func newOrderNo() string {
	return "bff" + strconv.FormatInt(time.Now().UnixNano(), 36)
}
