// Package rpc 入向传输适配层：链的最外一节，也是唯一认识 kitex 的一层。
//
// 职责边界（写窄，别让业务渗进来）：
//   - 实现生成的 order.Order 接口**全部方法**（kitex 按接口签名约束 NewServer 入参，
//     少一个方法编译期即失败）；
//   - 开 rpc.order.<Op> 分层 span，把 kitex 带进来的 ctx 接进链路；
//   - 经 route.Guard 走准入闸门，被拒时回**对应的**拒绝码（身份非法 401 / 被限流 429）；
//   - 把请求原样交给 handler（不做任何字段加工）。
//
// 本层不做出向调用：apps 的 srv 之间不互调（见 mesh-entry-chain 的同名 Requirement），
// 且 srv 档没有出向客户端的装配来源，故这里只负责入向。
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

	if aerr := s.admit(ctx, req.GetUserId()); aerr != nil {
		return nil, aerr
	}
	return s.handler.CreateOrder(ctx, req)
}

// GetOrder 查单。
func (s *Server) GetOrder(ctx context.Context, req *orderpb.GetOrderReq) (resp *orderpb.GetOrderResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "order", "GetOrder", s.traceOn, nil)
	defer func() { end(err) }()

	if aerr := s.admit(ctx, req.GetUserId()); aerr != nil {
		return nil, aerr
	}
	return s.handler.GetOrder(ctx, req)
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

// admit 走准入闸门。
//
// 限流主体的来源按优先级取**四处**（前者非零即短路，不再往下取）：
//  1. 同进程 ctx 里的 routing identity（`routing.IdentityFrom`）—— 单测 / 直连时有效；
//     它的值过不了进程边界，所以线上这条通常是空的；
//  2. kitex metainfo 的 `mesh-user-id`（由 bff 在出向调用前写入 —— 跨进程的身份载体）。
//     **解析失败即 Unauthorized**：身份头坏了要出声，不许静默降级成匿名；
//  3. 回落到本次请求 IDL 里的 `user_id`（订单域的请求本就带 user_id，够用作限流主体）；
//  4. 都没有时按 `"svc"` 分桶（服务级计数）—— 这才是「没带身份」的正当处理：放行 + 服务级计数。
//
// 注意 Unauthorized 的语义是「身份**非法**」，不是「没带身份」：没带身份（metainfo
// 键不存在）走第 3/4 条回落，fail-open。apps 的鉴权口径把传输层身份下沉给 Istio，
// 故应用侧不因匿名而拒绝（见 Task 3 的 route/route.go 注释）。
func (s *Server) admit(ctx context.Context, userID int64) error {
	id := routing.IdentityFrom(ctx) // 同进程内调用时有效（单测 / 直连）
	if id.UserID == 0 {
		if v, ok := metainfo.GetValue(ctx, identityMetaKey); ok {
			n, perr := strconv.ParseUint(v, 10, 64)
			if perr != nil || n == 0 {
				return codes.New(codes.Unauthorized,
					"malformed identity in metainfo key "+identityMetaKey)
			}
			id.UserID = n
		}
	}
	if id.UserID == 0 && userID > 0 {
		id.UserID = uint64(userID)
	}
	if s.guard.Admit(ctx, id) {
		return nil
	}
	return codes.New(codes.RateLimited, "rate limited")
}

// identityMetaKey bff 透传身份的 kitex metainfo 键名；与 Task 7 的写入侧 MUST 一致。
// 值只有一处定义（rpcwire.IdentityMetaKey）：bff 写、四域读，写死五份字面量
// 就有「改一处漏四处」的空间，而漏掉的症状是身份永远为空（限流退化成服务级）。
const identityMetaKey = rpcwire.IdentityMetaKey
