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

	"github.com/gospacex/otelx/layer"

	"github.com/gospacex/launchx/examples/monolith/internal/model"
	"github.com/gospacex/launchx/examples/monolith/internal/service"
)

// 列表接口的条数边界（List / Search 共用）。
//
// 为什么必须有上界：`limit` 直接来自 query，早先的实现是 `strconv.Atoi` 后
// 原样下传（解析失败得到 0，仓储侧又不判 0）⇒ `?limit=10000000` 一次把整张表
// 拉进内存，`?limit=abc` 则退化成"按驱动缺省取"。无上界不是"调错参数"，
// 是任何人都能发一次的放大面 —— 而这两个接口都不在鉴权分组下（route.Entry
// 的 demo 路径），代价由服务独自承担。
const (
	limitDefault = 20
	limitMax     = 200
)

// limitOf 归一化条数：非法 / 非正 → 默认；超过上界 → 截断到上界。
//
// 刻意不把「超上界」判成 400：分页条数是**建议值**，截断后照常返回前 N 条，
// 客户端拿到的结果短一点但可用；为了一个无害的参数去拒绝整个请求，代价不划算。
func limitOf(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return limitDefault
	}
	if n > limitMax {
		return limitMax
	}
	return n
}

// OrderHandler 订单 HTTP 入口。
type OrderHandler struct {
	svc     *service.Service
	traceOn bool
}

// New 构造订单 handler。
func New(svc *service.Service, traceOn bool) *OrderHandler {
	return &OrderHandler{svc: svc, traceOn: traceOn}
}

// begin 开 handler 层 span，返回「收 error 再收尾」的闭包。
//
// span 名由另一处改为三段式 handler.order.<Op>（与 service / repository 对齐），
// 本次只统一调用形态，不动命名。
//
// handler 是 hertz 回调（无返回值），故每个方法里显式 `var err error` 并在失败
// 分支赋值 —— 少这一步，失败路径在 Jaeger 上就是"成功"的绿色。
func (h *OrderHandler) begin(ctx context.Context, op string) (context.Context, layer.End) {
	return layer.Begin(ctx, "handler", "order", op, h.traceOn, nil)
}

// List GET /orders —— 单表查询入口（E1 的 JSON 数组断言落在这里）。
//
// 鉴权分组下的调用会把 user_id 从 request context 取出来传给 service：认证产生
// 的身份如果不被使用，鉴权就只是"门口查了一次身份证"，进去之后照样能翻别人的单。
// 无鉴权（demo）路径取不到这个键，userID 保持 0 = 不按用户收敛。
func (h *OrderHandler) List(ctx context.Context, c *app.RequestContext) {
	ctx, end := h.begin(ctx, "ListOrders")
	var err error
	defer func() { end(err) }()

	limit := limitOf(c.Query("limit"))
	orders, err := h.svc.List(ctx, userIDFrom(c), limit)
	if err != nil {
		respondError(c, err)
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
		fail(c, http.StatusBadRequest, errTextOf(service.ErrInvalidInput))
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
		respondError(c, err)
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
		respondError(c, err)
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
		respondError(c, err)
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

	limit := limitOf(c.Query("limit"))
	out, err := h.svc.Search(ctx, c.Query("q"), limit)
	if err != nil {
		respondError(c, err)
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
		respondError(c, err)
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
	// 显式判绑定错误，而不是 `_ = c.Bind(&req)`：吞掉之后一个畸形 body 会被当成
	// `{}`，走到 service 拿到 ErrInvalidInput → 400。状态码碰巧对了，但响应文案
	// 说的是"入参不合法"，排查时会被带偏到业务校验上，且绑定失败的具体原因
	// （字段类型不对 / JSON 语法错）彻底消失 —— 与 Create 的口径不一致的那一处。
	if err = c.Bind(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	order, err := h.svc.UpdateStatus(ctx, c.Param("order_no"), req.Status)
	if err != nil {
		respondError(c, err)
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
		respondError(c, err)
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

// userIDFrom 取鉴权中间件塞进 request context 的用户身份。
//
// 取不到返回 0（= 不按用户收敛）：无鉴权的 demo 路由上本来就没有经过中间件。
// 这里**不**把它当成异常 —— 同一份 handler 挂在鉴权与非鉴权两条路径上，
// 判断自身在场的是否 mutant 由路由表决定（见 route.Entry.Insecure 的回环约束）。
func userIDFrom(c *app.RequestContext) int64 {
	v, ok := c.Get("user_id")
	if !ok {
		return 0
	}
	switch id := v.(type) {
	case int64:
		return id
	case int:
		return int64(id)
	default:
		return 0
	}
}

// fail 统一错误渲染：响应体只带**固定文案**，不回吐 err.Error()。
//
// 注释早先承诺「不泄漏内部细节」，实现却是 {"error": err.Error()}：unknown 分支
// 里的 err 是驱动原文（dial tcp <db-host>:<port>: connect: connection refused、
// Unknown column 'item' in 'field list'），等于把库地址、表结构、依赖拓扑原样
// 交给调用方 —— 既是不该有的信息暴露，也让客户端不敢拿这个字符串做判定
// （升级个驱动改一句文案就会翻日志）。
//
// span 状态不再由这里标 —— 交给收尾闭包 end(err)：调用方只需在函数体里把失败
// 赋给 err，不必记得"还要调一次 fail 才算标上"。
func fail(c *app.RequestContext, status int, msg string) {
	c.JSON(status, map[string]string{"error": msg})
}

// errTextOf 取领域错误的对外文案。
//
// 只有**领域错误**（ErrInvalidInput / ErrNotFound）可以原文返回：它们的文案是
// 给调用方看的，不含任何内部细节。其余一律走 failureOf 的固定串。
func errTextOf(err error) string {
	_, msg := failureOf(err)
	return msg
}

// respondError 走「整条链路只看一次类型」的失败出口：状态码与文案从同一张表里来，
// 不会再出现"状态判成了 501、文案却是另一个错误的原文"这种中间态。
func respondError(c *app.RequestContext, err error) {
	status, msg := failureOf(err)
	fail(c, status, msg)
}

// failureOf 领域错误 → HTTP 状态码 + 对外文案。
//
// 返回值是二元组而不是只回状态码：早先 statusOf 只判状态码、文案由 fail 直接取
// err.Error()，两者在 unknown 分支上必然不一致 —— 状态码说"服务内部错了"，
// 响应体却把内部错误原文摊出来。绑成一张表，二者就分不开了。
//
// 详情的去处只有两处：span（end(err) 的 RecordError 带完整 error）与进程日志
// （hlog.CtxErrorf）。这里刻意不打日志：本项目没有给 handler 注入 logger，
// 打日志要在装配层多接一路依赖，收益是把同一条信息从 span 搬到日志；
// span 已经带完整原文，Jaeger 上看得到就够了。
func failureOf(err error) (int, string) {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, service.ErrNotFound):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, service.ErrComponentUnavailable):
		// 组件未装配/降级：不是调用方错，也不是服务错，是环境缺件 —— 501 最贴切，
		// 返回 500 会让调用方误以为服务崩了。文案也不给细节，只说"不可用"，
		// 缺哪个组件由日志/span 回答。
		return http.StatusNotImplemented, "service temporarily unavailable"
	default:
		return http.StatusInternalServerError, "internal error"
	}
}
