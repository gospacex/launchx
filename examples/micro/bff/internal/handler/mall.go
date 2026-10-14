// Package handler HTTP 层：只做参数绑定、状态码映射与响应封装，业务一律
// 下沉到 downstream 层与 dtm 编排层；每个 handler 自开 `handler.mall.<Op>` span。
//
// 复合编排 PlaceOrder 是全链路验收的锚点：一次 HTTP 请求先经商品域做一次
// **普通 RPC 预检**，再把一次下单编成 dtm Saga 提交，由 dtm 推进
// 库存扣减 → 建单 → 销量 → 扣积分四段，失败由 dtm 逆序补偿。
//
// 本包是 webx/hertz 唯一出现的地方（handler 签名是 hertz 的 app.HandlerFunc）。
package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/micro/bff/internal/downstream"
	dtmorch "github.com/gospacex/launchx/examples/micro/bff/internal/dtm"
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
	orch    *dtmorch.Orchestrator
	traceOn bool
}

// New 构造 handler。
//
// saga 是必传项而不是可选能力：下单是本服务唯一的复合编排入口，配了编排器却
// 传 nil 进来只可能是一次装配疏忽，而那次疏忽表现为「POST /order 永远 panic」。
// dtm 不可用时装配层传 dtmorch.Unavailable —— 提交恒 5xx、进程照常服务其余路由。
//
// 不再收一个 trace.Tracer（与 downstream 同因）：tracer 一律向 otel 全局现取，
// 全局 provider 由 launchx 的 telemetry 步骤注册。从装配层手里接 provider 进来，
// 会让本层与 CLIENT span 分家的风险变成"取决于装配顺序"。
func New(d *downstream.Downstream, saga *dtmorch.Orchestrator, traceOn bool) *MallHandler {
	return &MallHandler{d: d, orch: saga, traceOn: traceOn}
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
	// FailInject 演示用：点名一个分支方法名（Deduct / CreateOrder / AddSales /
	// Debit），该分支直接返业务失败，用来点火 dtm 的逆序补偿链。
	//
	// 真机恒为空，且**绝不能**从真实业务参数派生：载荷里 fail_inject 非空就
	// 等于「点名让某个分支失败」，一个透传失误就是提交即全局回滚。
	FailInject string `json:"fail_inject,omitempty"`
}

// PlaceOrder POST /order —— 复合编排：预检 + 提交 dtm Saga。
//
// 两段，边界清楚：
//
//	① 预检（**不进事务**）：product.GetProduct 走现有 kitex RPC，商品不存在 /
//	   已下架直接 4xx，此时 dtm 里没有任何全局事务、四个库都没有写入。
//	   它只是一次便宜的普通读 —— 放进事务当分支要付 barrier 与重试的代价，
//	   换来的是「查得到才扣」这一点预检已经完全做到。
//
//	② 提交 Saga：dtm 按 stock.Deduct → order.CreateOrder → product.AddSales →
//	   points.Debit 推进，失败逆序补偿 stock.Release / CancelOrder /
//	   ReduceSales / Refund。**补偿由 dtm 发起，本层不再有任何补偿调用**：
//	   手动 ReleaseStock 已退役，两套补偿并存就是重复回补。
//
// 返回体给的是 gid 而不是 order_id：Saga 是异步的，提交返回时四个分支一个都
// 还没跑完，此刻报一个 order_id 只会是编的。要查结果拿 gid 去问 dtm。
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

	// ① 预检：商品存在且上架才往下走。失败一律 4xx/5xx，**不提交事务**。
	p, err := h.d.GetProduct(ctx, &productpb.GetProductReq{UserId: req.UserId, ProductId: req.ProductID})
	if err != nil {
		fail(span, c, http.StatusBadGateway, err)
		return
	}
	if code := p.GetCode(); code != codes.CodeOK {
		fail(span, c, statusOf(code), errors.New(p.GetMessage()))
		return
	}
	unitPrice := p.GetProduct().GetPrice()

	// ② 提交 Saga。分支出错由 dtm 兜，本层只判「事务提交成功没有」。
	//
	// fail_inject 由请求体带入，仅用于演示点火补偿链；真机留空。
	res, err := h.orch.Submit(ctx, dtmorch.OrderCmd{
		UserID:     req.UserId,
		ProductID:  req.ProductID,
		Quantity:   req.Quantity,
		OrderNo:    orderNo,
		Title:      p.GetProduct().GetTitle(),
		UnitPrice:  unitPrice,
		PointsCost: pointsReward(unitPrice, req.Quantity),
		FailInject: req.FailInject,
	})
	if err != nil {
		// 5xx：dtm 不可达或提交被拒 —— 全局事务没建立，四域没有任何写入，
		// 调用方原样重试是安全的（且订单号每次都是新的，不存在重复单）。
		// 这里**不能**回落手动编排链：手动补偿已退役，回落就是两套补偿。
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
		"branches":     branchView(res.Steps),
	})
}

// branchView 分支列表回显：域 + 正向分支名。
//
// 带出来是因为提交响应是**唯一**能确认「编排了什么」的地方：四域是否都参与
// （少一条就是某个域被静默漏掉）、按什么次序（决定回滚次序），都在这行里。
func branchView(steps []dtmorch.Step) []map[string]string {
	out := make([]map[string]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, map[string]string{
			"domain": s.Domain,
			"branch": s.Action,
		})
	}
	return out
}

// pointsReward 本单奖励积分数（示例口径：成交额的 1%，分 → 积分取整）。
//
// 下限 1 是必需的而不是美化：积分域分支的 validateDtmBiz 拒 cost<=0，
// 而 0 在那里**不是「不扣积分」而是「这笔分支必然业务失败」**—— 整条 Saga
// 会被判失败并把已经跑成功的三个分支回滚，代价远大于给一分钱。
// 真机上 1% 的最小成交额（100 分）恰好压在这个下限上，低于它就该有积分。
func pointsReward(unitPrice int64, quantity int32) int64 {
	reward := unitPrice * int64(quantity) / 100
	if reward < 1 {
		reward = 1
	}
	return reward
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
