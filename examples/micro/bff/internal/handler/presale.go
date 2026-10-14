package handler

// 本文件是 bff 的**第二条复合编排入口**：预售锁定 POST /api/presale/lock。
//
// 与同包的 mall.go（POST /order，Saga 四分支）构成 spec dtm-tcc-presale 要求的
// 「同一 bff 与四域服上语义对照」：两条链共用同一套 dtm 地址配置、同一套 trace
// 续链口径、同一个失败姿态（5xx + Warn 带病运行），但**分支方法完全独立**
// （TCC 用 TryFreeze/ConfirmFreeze/CancelFreeze + order.TryPlace，Saga 用
// Deduct/CreateOrder/…）—— 绝不复用扣减接口充当冻结。
//
// 与 mall.go 的差异集中在三点，都写在各自的注释里：TCC 只发 Try（Confirm/Cancel
// 由 dtm 驱动）、TCC 的失败有三档后果文案、响应体只给 gid 而不给任何业务 id。

import (
	"context"
	"errors"
	"net/http"

	"github.com/cloudwego/hertz/pkg/app"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/micro/bff/internal/downstream"
	dtmorch "github.com/gospacex/launchx/examples/micro/bff/internal/dtm"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	productpb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/product"

	"github.com/gospacex/otelx/layer"
)

// PresaleHandler 预售聚合 HTTP 入口。
type PresaleHandler struct {
	d       *downstream.Downstream
	tcc     *dtmorch.TCCOrchestrator
	traceOn bool
}

// NewPresale 构造 handler。
//
// tcc 是必传项而不是可选能力（与 mall.go 的 New 同因）：预售锁单是本端点的
// 唯一实现，传 nil 进来只可能是一次装配疏忽，那次疏忽表现为「POST
// /api/presale/lock 永远 panic」。dtm 不可用时装配层传
// dtmorch.UnavailableTCC —— 提交恒 5xx、进程照常服务其余路由。
//
// 不收 trace.Tracer（同 mall.go）：tracer 一律向 otel 全局现取。
func NewPresale(d *downstream.Downstream, tcc *dtmorch.TCCOrchestrator, traceOn bool) *PresaleHandler {
	return &PresaleHandler{d: d, tcc: tcc, traceOn: traceOn}
}

// span 开本层 span（handler.presale.<Op>）。
//
// 与 mall.go 同一个坑、同一个解法：traceOn=false 时返回 noop span 而不是 nil
// —— 本文件各处都要 `defer span.End()`，返回 nil 会让第一处 `defer` 就 panic。
func (h *PresaleHandler) span(ctx context.Context, op string) (context.Context, trace.Span) {
	if !h.traceOn {
		return ctx, trace.SpanFromContext(context.Background())
	}
	return layer.Start(ctx, "handler", "presale", op)
}

// lockReq POST /api/presale/lock 请求体。
//
// 与 mall.go 的 placeOrderReq 同构（user_id / product_id / quantity）而不是
// 另立一套：两条链锁的是同一批资源、同一份下单语义，字段分叉会让调用方学会
// 「预售要用另一种参数名」，而两者的失败模式完全一样。
type lockReq struct {
	UserID    int64 `json:"user_id"`
	ProductID int64 `json:"product_id"`
	Quantity  int32 `json:"quantity"`
	// FailInject 演示用：点名一个分支方法名（TryFreeze / TryPlace），该分支
	// 直接返业务失败，用来点火 dtm 的逆序 Cancel 链。
	//
	// 真机恒为空，且**绝不能**从真实业务参数派生：载荷里 fail_inject 非空就
	// 等于「点名让某个分支失败」，一个透传失误就是提交即全局解冻。
	FailInject string `json:"fail_inject,omitempty"`
}

// Lock POST /api/presale/lock —— 预售锁定：预检 + 提交 dtm TCC。
//
// 两段，边界与 mall.go 的 PlaceOrder 一致：
//
//		① 预检（**不进事务**）：product.GetProduct 走现有 kitex RPC，商品不存在 /
//		   已下架直接 4xx，此时 dtm 里没有全局事务、四个库都没有写入。它只是一次
//		   便宜的普通读 —— TCC 的二阶段是 dtm 异步驱动的，把它当分支塞进去要付
//		   barrier 与「预检失败也要走一遍 Cancel」两重代价。
//
//		② 提交 TCC：bff 只发三个 Try（stock.TryFreeze → points.TryFreeze →
//		   order.TryPlace），**Confirm / Cancel 全部由 dtm 在 Submit 之后驱动**。
//	  Try 全绿 → dtm 逐分支回调 Confirm（占位单转正式 paid 单，outbox 此刻才写）；
//	  任一 Try 失败 → dtm 逆序回调 Cancel（三域解冻、占位单作废、零脏数据）。
//	  **本层不再有任何 confirm / cancel 调用**：那是第二次提交同一条全局事务。
//
// 返回体只给 gid、order_no 与分支清单，**不给 order_id / reservation_id**：
// TCC 是异步的，提交返回时三个分支一个都没跑完，此刻报一个 id 只会是编的。
// 与 Saga 侧同一条口径（PlaceOrder 同样只给 gid）。
func (h *PresaleHandler) Lock(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.span(ctx, "Lock")
	defer span.End()

	var req lockReq
	if err := c.BindJSON(&req); err != nil {
		fail(span, c, http.StatusBadRequest, err)
		return
	}
	// 归属用户**只取鉴权签发的身份**（userIDOf 注释里那条「否则鉴权白做」）：
	// 请求体里的 user_id 是调用方自己声明的，有身份时一律以身份为准。
	userID := userIDOf(ctx, c, "", req.UserID)
	req.UserID = userID
	if req.UserID <= 0 || req.ProductID <= 0 || req.Quantity <= 0 {
		fail(span, c, http.StatusBadRequest, errors.New("user_id/product_id/quantity must be positive"))
		return
	}
	orderNo := newOrderNo()
	span.SetAttributes(
		attribute.String("biz.order_no", orderNo),
		attribute.Int64("biz.user_id", req.UserID),
		attribute.Int64("biz.product_id", req.ProductID),
		attribute.Int("biz.quantity", int(req.Quantity)),
	)

	// ① 预检：商品存在且上架才往下走。失败一律 4xx/5xx，**不提交事务**。
	// 单价与标题都取回这里 —— 它们要进订单明细（订单域拒 unit_price < 0 与空
	// 明细），而让调用方自报单价等于把可篡改字段写进账本。
	p, err := h.d.GetProduct(ctx, &productpb.GetProductReq{UserId: req.UserID, ProductId: req.ProductID})
	if err != nil {
		fail(span, c, http.StatusBadGateway, err)
		return
	}
	if code := p.GetCode(); code != codes.CodeOK {
		fail(span, c, statusOf(code), errors.New(p.GetMessage()))
		return
	}
	unitPrice := p.GetProduct().GetPrice()

	// ② 提交 TCC。分支成败由 dtm 兜，本层只判「事务提交成功没有」。
	//
	// 失败一律 5xx，但**错误文案带后果**：编排器把「事务未建立 / 可原样重试」
	// 与「已 Abort、正在解冻、请勿立刻重试」两档分别编进 error，这里原样上抛
	// 进响应体。让调用方一律查 gid 再决定重试，比在这里猜一个可能把冻结做两遍
	// 的状态安全。
	//
	// fail_inject 由请求体带入，仅用于演示点火 Cancel 链；真机留空。
	res, err := h.tcc.Lock(ctx, dtmorch.LockCmd{
		UserID:     req.UserID,
		ProductID:  req.ProductID,
		Quantity:   req.Quantity,
		OrderNo:    orderNo,
		Title:      p.GetProduct().GetTitle(),
		UnitPrice:  unitPrice,
		PointsCost: pointsReward(unitPrice, req.Quantity),
		FailInject: req.FailInject,
	})
	if err != nil {
		fail(span, c, http.StatusInternalServerError, err)
		return
	}
	span.SetAttributes(
		attribute.String("biz.dtm.gid", res.GID),
		attribute.Int("biz.dtm.branches", len(res.Steps)),
	)

	c.JSON(http.StatusOK, map[string]any{
		"code":         codes.CodeOK,
		"dtm_gid":      res.GID,
		"order_no":     orderNo,
		"total_amount": unitPrice * int64(req.Quantity),
		"branches":     presaleBranchView(res.Steps),
	})
}

// presaleBranchView 分支列表回显：域 + try/confirm/cancel 三段名。
//
// 带出来是因为提交响应是**唯一**能确认「编排了什么」的地方：三域是否都参与、
// 按什么次序 Try、以及最要紧的 —— Confirm / Cancel 会回调到哪个方法名（dtm
// 只按 URL 回调，编排侧配错了它在提交时毫无异样，要等二阶段才炸）。三段一起
// 回显正是为了在真机上一眼比对 dtm server 日志里的实际回调。
func presaleBranchView(steps []dtmorch.TCCStep) []map[string]string {
	out := make([]map[string]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, map[string]string{
			"domain":  s.Domain,
			"try":     s.Methods.Try,
			"confirm": s.Methods.Confirm,
			"cancel":  s.Methods.Cancel,
		})
	}
	return out
}
