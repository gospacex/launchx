// Package handler HTTP 出入口：解析参数 → 调下游 → 序列化。
//
// 零业务判断（不出现「金额大于多少就…」这类分支），与 srv 侧的 handler 同纪律。
//
// 六条路由 MUST 覆盖四域（见 mesh-entry-chain）：任何一个 Clients 字段没有调用点，
// 就意味着那个域的业务逻辑永远无法从入口触达 —— 编译器不会提醒这件事。
//
// 三件事各自只有**一个**落点，改动时别拆散（拆开就是静默失效）：
//
//  1. 业务码还原：跨进程回来的错误 MUST 经 `rpcwire.FromRPC` 还原。kitex 默认
//     不装 TTHeader handler，业务码会被降级成一段字符串（客户端 `errors.As`
//     找不到 *codes.Error），直接用 `codes.From` 会让整张状态码映射表失效 ——
//     每个业务拒绝都变 502，而链路与日志看起来全正常。见 rpcwire 包注释。
//  2. 身份透传：出向调用前把边界解析出的主体写进 metainfo（withIdentity），
//     这是跨进程身份的唯一载体，四域 srv 的 `rpc.admit` 靠它做按主体限流。
//  3. 状态码映射：`statusOf` 是「业务码 → HTTP」的**唯一**映射表。srv 侧不关心
//     HTTP，故这张表只在这里有；搬进 srv 或各写一份都会出现两个真相。
package handler

import (
	"context"
	"net/http"
	"strconv"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/gospacex/otelx/layer"
	"github.com/gospacex/webx"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/bff/downstream"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/rpcwire"

	orderpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/order"
	pointspb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/points"
	productpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/product"
	stockpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/stock"
)

// MallHandler mall 聚合 HTTP 入口。
type MallHandler struct {
	clients *downstream.Clients
	traceOn bool // trace.enabled（总闸），由装配层注入
}

// New 构造 handler。
//
// 不再收一个 trace.Tracer：tracer 一律向 otel 全局现取（otelx/layer 就是这么做的），
// 全局 provider 由 launchx 的 telemetry 步骤注册。从装配层接 provider 进来，会让
// 本层与 CLIENT span 分家的风险变成「取决于装配顺序」。
func New(c *downstream.Clients, traceOn bool) *MallHandler {
	return &MallHandler{clients: c, traceOn: traceOn}
}

// Register 把六条路由挂到 webx 引擎上。
//
// 入参是 webx.App：引擎经 `App.Engine()` 以 any 交出来，业务断言成 *webx.App
// （与 launchx 库源码不 import hertz 是同一纪律）。回调签名用 hertz 的
// `*app.RequestContext` —— 现场核对结论：webx **没有**自己的 RequestContext 类型
// （webx.go 只内嵌 *server.Hertz，路由与中间件都直接用 hertz 的类型），故它就是
// 引擎与业务之间唯一可能的约定。
//
// webx 已自带 `/healthz/{live,ready}`，无需在此重复注册（K8s 探针用的就是它）。
func (h *MallHandler) Register(e *webx.App) {
	e.GET("/ping", h.ping)
	e.GET("/order/:id", h.getOrder)
	e.POST("/order", h.createOrder)
	e.GET("/points/:user_id", h.getPoints)
	e.GET("/product/:id", h.getProduct)
	e.GET("/stock/:product_id", h.getStock)
}

// ping GET /ping —— 自响应，不触达下游。
//
// 它的作用是给「服务活着且 HTTP 那一跳能成 span」留一个不依赖任何下游的判据：
// 四个域服全挂时 /ping 仍须 200。trace 判据对它豁免（单跳自响应）。
func (h *MallHandler) ping(ctx context.Context, c *app.RequestContext) {
	_, end := layer.Begin(ctx, "handler", "bff", "Ping", h.traceOn, nil)
	defer func() { end(nil) }()
	c.JSON(http.StatusOK, map[string]string{"msg": "pong"})
}

// getOrder GET /order/:id —— 经订单域取单个订单 + 明细。
//
// 订单明细由**订单域自己**的 repository 取（`order_items` 是它的表），
// 不存在 srv-order → srv-product/stock 这一跳：apps 的跨域编排全在 bff
// （见 mesh-entry-chain 的同名 Requirement），故这一条路由的 trace 里
// 只应出现 order 一个域的 service span。
func (h *MallHandler) getOrder(ctx context.Context, c *app.RequestContext) {
	ctx, end := layer.Begin(ctx, "handler", "bff", "GetOrder", h.traceOn, nil)
	var err error
	defer func() { end(err) }()

	id, err := pathUint(c, "id")
	if err != nil {
		writeErr(c, err)
		return
	}
	resp, err := h.clients.Order.GetOrder(withIdentity(ctx), &orderpb.GetOrderReq{OrderId: id})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// placeOrderReq POST /order 请求体。
type placeOrderReq struct {
	UserID int64            `json:"user_id"`
	Items  []placeOrderItem `json:"items"`
}

// placeOrderItem 单条明细。与 IDL 的 `OrderItem` 分开写：请求体是对外契约，
// 结构体是进程间契约，两者同形但不必同源（改名时不该互相牵动）。
type placeOrderItem struct {
	ProductID int64  `json:"product_id"`
	Name      string `json:"name"`
	Quantity  int32  `json:"quantity"`
	UnitPrice int64  `json:"unit_price"`
}

// createOrder POST /order —— 下单。
//
// 这是 `order → MQ → points` 异步链的**唯一入口**（也是 Task 15 验收矩阵里
// 「异步链」那一行的触发点）：本方法只把请求交给订单域，事件投递发生在
// srv-order 的 service 层；积分入账由 srv-points 的消费循环完成，不经 HTTP。
//
// 本示例不带 outbox：投递失败只告警（见 apps/README「删不掉的四类」）。
func (h *MallHandler) createOrder(ctx context.Context, c *app.RequestContext) {
	ctx, end := layer.Begin(ctx, "handler", "bff", "CreateOrder", h.traceOn, nil)
	var err error
	defer func() { end(err) }()

	var in placeOrderReq
	// 包装后写回 err（而不是只传给 writeErr）：defer 里的 end(err) 用的是这个变量，
	// 落进 span 的应当是**带业务码的那个错误**，否则 Jaeger 上只有一段 JSON 解析
	// 失败，看不出它被归到 400。
	if err = c.BindJSON(&in); err != nil {
		err = codes.Wrap(codes.InvalidArgument, "bad body", err)
		writeErr(c, err)
		return
	}
	// 归属用户**优先取边界解析出的身份**：请求体里的 user_id 是调用方自己声明的，
	// 信它就等于把「我是谁」交给客户端决定。身份取不到时（本机无 Authorization
	// 的排障路径）才回落到请求体 —— 这一档与 mesh-app-handles 的 fail-open 口径
	// 一致：apps 把传输层身份下沉给 Istio，应用侧对匿名请求不拒绝。
	// 这是**边界授权**判断，不是业务判断（业务判断只允许出现在 srv 的 service 层）。
	if id := routing.IdentityFrom(ctx); id.UserID != 0 {
		in.UserID = int64(id.UserID)
	}
	if in.UserID == 0 || len(in.Items) == 0 {
		err = codes.New(codes.InvalidArgument, "user_id and items are required")
		writeErr(c, err)
		return
	}

	items := make([]*orderpb.OrderItem, 0, len(in.Items))
	for _, it := range in.Items {
		items = append(items, &orderpb.OrderItem{
			ProductId: it.ProductID, Name: it.Name,
			Quantity: it.Quantity, UnitPrice: it.UnitPrice,
		})
	}
	resp, err := h.clients.Order.CreateOrder(withIdentity(ctx), &orderpb.CreateOrderReq{
		UserId: in.UserID, Items: items,
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// getPoints GET /points/:user_id —— 积分只读路径。
//
// span 名取 RPC 方法名 `GetAccount`（不是 Go 方法名 getPoints）：与 srv 侧的
// `handler.points.GetAccount` 同名，Jaeger 里能一眼对上「同一根链的两端」。
//
// points 域的**写路径**在 MQ 消费侧（`ConsumeOrderCompleted`），不经 HTTP ——
// 这也是它必须单列一条异步链判据的原因。
func (h *MallHandler) getPoints(ctx context.Context, c *app.RequestContext) {
	ctx, end := layer.Begin(ctx, "handler", "bff", "GetAccount", h.traceOn, nil)
	var err error
	defer func() { end(err) }()

	uid, err := pathUint(c, "user_id")
	if err != nil {
		writeErr(c, err)
		return
	}
	resp, err := h.clients.Points.GetAccount(withIdentity(ctx), &pointspb.GetAccountReq{UserId: uid})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// getProduct GET /product/:id —— 商品详情。
//
// srv-product 侧走「读缓存 → 回源 → 回填」，故本路由的 trace 里通常能看到
// `cache.*` 组件 span；缓存未命中/不可用只是慢，不是失败。
func (h *MallHandler) getProduct(ctx context.Context, c *app.RequestContext) {
	ctx, end := layer.Begin(ctx, "handler", "bff", "GetProduct", h.traceOn, nil)
	var err error
	defer func() { end(err) }()

	id, err := pathUint(c, "id")
	if err != nil {
		writeErr(c, err)
		return
	}
	resp, err := h.clients.Product.GetProduct(withIdentity(ctx), &productpb.GetProductReq{ProductId: id})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// getStock GET /stock/:product_id —— 库存快照。
//
// 「无库存记录」在 srv-stock 是**零值视图而非错误**（available=0 在业务上就是
// 扣不动），故这一条路由对未建过库存行的商品仍回 200，别按 404 去排查。
func (h *MallHandler) getStock(ctx context.Context, c *app.RequestContext) {
	ctx, end := layer.Begin(ctx, "handler", "bff", "GetStock", h.traceOn, nil)
	var err error
	defer func() { end(err) }()

	pid, err := pathUint(c, "product_id")
	if err != nil {
		writeErr(c, err)
		return
	}
	resp, err := h.clients.Stock.GetStock(withIdentity(ctx), &stockpb.GetStockReq{ProductId: pid})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// withIdentity 把边界解析出的主体写进出向调用的 metainfo —— 跨进程身份的唯一载体。
//
// 键名取 `rpcwire.IdentityMetaKey`（**单一来源**）：四域 srv 的 `rpc.admit` 读同
// 一个常量。五处各写一份字面量就有「改一处漏四处」的空间，漏掉的症状是身份永远
// 为空 —— 限流静默退化成服务级计数，不报错。
//
// 匿名（UserID==0）时**不写键**：写 "0" 会让 srv 侧把它当成「身份非法」回
// Unauthorized（admit 对解析失败/为 0 是出声拒绝，对**没带键**才是 fail-open）。
func withIdentity(ctx context.Context) context.Context {
	id := routing.IdentityFrom(ctx)
	if id.UserID == 0 {
		return ctx
	}
	return metainfo.WithValue(ctx, rpcwire.IdentityMetaKey, strconv.FormatUint(id.UserID, 10))
}

// pathUint 解析路径参数。抽出来只因为六条路由里有四条做同一件事 ——
// 其余一律不抽（业务判断一条都不抽，这是分层纪律）。
func pathUint(c *app.RequestContext, name string) (int64, error) {
	v, err := strconv.ParseUint(c.Param(name), 10, 64)
	if err != nil || v == 0 {
		return 0, codes.New(codes.InvalidArgument, "path param "+name+" must be a positive integer")
	}
	return int64(v), nil
}

// writeErr 业务码 → HTTP 状态码 + 带 `code` 字段的响应体。
//
// 还原码 MUST 经 rpcwire.FromRPC（不是 codes.From）：下游回来的业务码在客户端
// 是 `*kerrors.BizStatusError`，只有它认得。详见包注释第 1 条。
func writeErr(c *app.RequestContext, err error) {
	code, msg := rpcwire.FromRPC(err)
	c.JSON(statusOf(code), map[string]any{"code": int32(code), "error": msg})
}

// statusOf 业务码 → HTTP 状态码。这张表**只在 bff 有** —— srv 侧不关心 HTTP。
//
// 判据是「谁能区分故障归属，就映射谁」：**客户端错误必须映射成 4xx**，只有真正
// 的下游故障才留在 5xx。把一个「路径参数写错了」渲染成 502，排查方向会被直接
// 带偏（曾经出现过「网关报 502，误判为 srv 不通，实际是缺 user_id」的完整误诊）。
//
// 4xx 内部还要分开，因为**客户端的重试策略相反**：401（身份非法）重试一万次
// 也是同一个结果；429（被限流）等一会儿可以再来；409（状态冲突）要先补货/充值。
// 把三者揉成一个码，等于让调用方只能盲目重试或一律放弃。
func statusOf(code codes.Code) int {
	switch code {
	case codes.InvalidArgument:
		return http.StatusBadRequest // 400：参数不合法
	case codes.Unauthorized:
		return http.StatusUnauthorized // 401：身份非法（metainfo 解析不出来）
	case codes.OrderNotFound, codes.ProductNotFound:
		return http.StatusNotFound // 404：目标不存在
	case codes.StockNotEnough, codes.PointsNotEnough:
		return http.StatusConflict // 409：状态冲突（可重试的前提是先补货/充值，不是退避）
	case codes.RateLimited:
		return http.StatusTooManyRequests // 429：值得退避重试
	case codes.Unimplemented:
		return http.StatusNotImplemented // 501：下游如实声明「本示例未实现」，别伪装成 404/502
	case codes.OrderCreateFail:
		return http.StatusInternalServerError // 500：下游写库失败 —— 它自己坏了，不是上游
	case codes.Internal:
		return http.StatusBadGateway // 502：含传输层失败（连不上/超时）与下游内部错误
	default:
		// 未列出的码一律按**下游故障**透传 502，而不是 500：能走到这里的只有
		// 「跨进程回来的、我们不认识的业务码」，对客户端它是「网关背后那一跳出
		// 问题」。报成 500（「本服务崩了」）会把排查方向带到 bff 自己身上。
		return http.StatusBadGateway
	}
}
