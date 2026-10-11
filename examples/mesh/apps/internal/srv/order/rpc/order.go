// Package rpc 入向传输适配层：链的最外一节，也是唯一认识 kitex 的一层。
//
// 职责边界（写窄，别让业务渗进来）：
//   - 实现生成的 order.Order 接口**全部方法**（kitex 按接口签名约束 NewServer 入参，
//     少一个方法编译期即失败）；
//   - 开 rpc.order.<Op> 分层 span，把 kitex 带进来的 ctx 接进链路；
//   - 经 route.Guard 走准入闸门，被拒时回**对应的**拒绝码（身份非法 401 / 越权 403 /
//     被限流 429）；
//   - 把请求原样交给 handler（不做任何字段加工）。
//
// 本层不做出向调用：apps 的 srv 之间不互调（见 mesh-entry-chain 的同名 Requirement），
// 且 srv 档没有出向客户端的装配来源，故这里只负责入向。
//
// 2026-10-10 起本层还承担**归属校验**（IDOR 防护）：身份是传输层的事，只有本层
// 认识 metainfo；service 层按纪律只认入参，不该去解析 metainfo。详见 GetOrder 注释。
package rpc

import (
	"context"
	"strconv"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/gospacex/otelx/layer"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/handler"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/route"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	orderpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/order"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/rpcwire"
)

// Server 实现生成的 order.Order 接口。
type Server struct {
	handler *handler.OrderHandler
	guard   *route.Guard
	traceOn bool // trace.enabled（总闸），由装配层注入
}

// New 构造入向适配层。
func New(h *handler.OrderHandler, g *route.Guard, traceOn bool) *Server {
	return &Server{handler: h, guard: g, traceOn: traceOn}
}

// 编译期断言：接口必须全实现（放在 rpc 层，不放 handler）。
var _ orderpb.Order = (*Server)(nil)

// CreateOrder 下单（3 表事务 + 事件投递）。
//
// 结果列表命名：与 ListOrders / Report 同形，让 defer 里的 end(err) 覆盖所有
// return 路径（Go 要求「全命名或全不命名」）。
func (s *Server) CreateOrder(ctx context.Context, req *orderpb.CreateOrderReq) (resp *orderpb.CreateOrderResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "order", "CreateOrder", s.traceOn, nil)
	defer func() { end(err) }()

	id, authed, aerr := s.resolveSubject(ctx, req.GetUserId())
	if aerr != nil {
		return nil, aerr
	}
	// 主体可信时，请求里声明的 user_id MUST 与它一致：bff 正常会把 body 的
	// user_id 覆盖成主体，一旦两者不等，只可能是 bff 漏改或有人绕过 bff 直连
	// —— 都是不该放行的形态。静默改写等于把「代他人下单」变成一条无痕路径。
	if authed && uint64(req.GetUserId()) != id.UserID {
		return nil, codes.New(codes.Forbidden,
			"user_id does not match the authenticated subject")
	}
	if !s.guard.Admit(ctx, id) {
		return nil, codes.New(codes.RateLimited, "rate limited")
	}
	return s.handler.CreateOrder(ctx, req)
}

// GetOrder 查单。
//
// 归属校验（IDOR 防护，2026-10-10 加固）：主体可信且订单不属于该主体时回 403。
// 校验点放在**本层**而不是 service：身份是传输层的事（metainfo / kitex），
// service 层不该认识 metainfo —— 那会把「业务判断只认入参」这条纪律破掉。
// 代价是多读一次订单行来比对归属；它与 handler 本来就要读的那次是同一行，
// 后续若成为热点再考虑把归属下推到 SQL 的 WHERE（`AND user_id = ?`）。
func (s *Server) GetOrder(ctx context.Context, req *orderpb.GetOrderReq) (resp *orderpb.GetOrderResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "order", "GetOrder", s.traceOn, nil)
	defer func() { end(err) }()

	id, authed, aerr := s.resolveSubject(ctx, req.GetUserId())
	if aerr != nil {
		return nil, aerr
	}
	if !s.guard.Admit(ctx, id) {
		return nil, codes.New(codes.RateLimited, "rate limited")
	}
	resp, err = s.handler.GetOrder(ctx, req)
	if err != nil {
		return nil, err
	}
	if authed && uint64(resp.GetUserId()) != id.UserID {
		return nil, codes.New(codes.Forbidden,
			"order does not belong to the authenticated subject")
	}
	return resp, nil
}

// ListOrders 未接线：本示例不提供列表页。与 CreateOrder / GetOrder 同形 ——
// 只开 rpc.order.ListOrders span 后原样委派 handler（错误码映射归 handler）；
// **不过 admit 闸门**：未接线、无业务主体可限，开了只会误伤限流桶。
func (s *Server) ListOrders(ctx context.Context, req *orderpb.ListOrdersReq) (resp *orderpb.ListOrdersResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "order", "ListOrders", s.traceOn, nil)
	defer func() { end(err) }()
	return s.handler.ListOrders(ctx, req)
}

// Report 未接线：同上，只开 span 后委派 handler，不过 admit 闸门。
func (s *Server) Report(ctx context.Context, req *orderpb.ReportReq) (resp *orderpb.ReportResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "order", "Report", s.traceOn, nil)
	defer func() { end(err) }()
	return s.handler.Report(ctx, req)
}

// resolveSubject 解析本次调用的主体，并回答「这个主体是不是**可信来源**」。
//
// 身份来源按优先级取**三处**（前者非零即短路，不再往下取）：
//  1. 同进程 ctx 里的 routing identity（`routing.IdentityFrom`）—— 单测 / 直连时有效；
//     它的值过不了进程边界，所以线上这条通常是空的 → 可信；
//  2. kitex metainfo 的 `mesh-user-id`（由 bff 在出向调用前写入 —— 跨进程的身份载体）；
//     **解析失败即 Unauthorized**：身份头坏了要出声，不许静默降级成匿名 → 可信；
//  3. 回落到本次请求 IDL 里的 `user_id`（订单域的请求本就带 user_id，够用作限流主体）
//     → **不可信**（这是调用方自己声明的，不是鉴权结果）。
//
// 第三个返回值就是「可信与否」，它决定**归属校验要不要执行**：
// 只有经鉴权来源得到的主体才拿去和资源比对。若把回落值也当真，越权者只要在
// 请求里写上被害者的 user_id，归属校验就被自己的入参喂成全真 —— 那种校验
// 有还不如没有，因为它看起来是防住了。
//
// authed=false 且 id 为零（既没身份也没声明）时仍放行并按 "svc" 桶计数，
// 这一点没变：apps 的鉴权闸门在 **bff**（无凭据 401），srv 侧对「没带身份」
// 保持 fail-open，只为限流分桶。
func (s *Server) resolveSubject(ctx context.Context, fallbackUserID int64) (routing.Identity, bool, error) {
	id := routing.IdentityFrom(ctx)
	if id.UserID != 0 {
		return id, true, nil
	}
	if v, ok := metainfo.GetValue(ctx, identityMetaKey); ok {
		n, perr := strconv.ParseUint(v, 10, 64)
		if perr != nil || n == 0 {
			return routing.Identity{}, false, codes.New(codes.Unauthorized,
				"malformed identity in metainfo key "+identityMetaKey)
		}
		return routing.Identity{UserID: n}, true, nil
	}
	if fallbackUserID > 0 {
		return routing.Identity{UserID: uint64(fallbackUserID)}, false, nil
	}
	return routing.Identity{}, false, nil
}

// identityMetaKey bff 透传身份的 kitex metainfo 键名；与 Task 7 的写入侧 MUST 一致。
// 值只有一处定义（rpcwire.IdentityMetaKey）：bff 写、四域读，写死五份字面量
// 就有「改一处漏四处」的空间，而漏掉的症状是身份永远为空（限流退化成服务级）。
const identityMetaKey = rpcwire.IdentityMetaKey
