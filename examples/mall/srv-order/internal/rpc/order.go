// Package rpc srv-order 的 kitex 传输适配层。
//
// 在分层链上的位置：**rpc** → route → middleware → handler → service →
// repository → model。它是链的最外一节，也是唯一认识 kitex 的一层。
//
// 职责边界（写窄，别让业务渗进来）：
//   - 开 rpc.order.<Method> span，把 kitex 带进来的 ctx 接进链路
//   - 经 route.Router.Enter 走路由与中间件链，被拒时回限流码
//   - 把请求原样交给 handler（不做任何字段加工）
//
// 本层不碰监听地址：server 选项由 launchx 的 srv 档装配产出（地址取
// config.yaml 的 service.self.addr）。
package rpc

import (
	"context"

	"github.com/gospacex/launchx/examples/mall/pkg/codes"
	orderpb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/order"
	"github.com/gospacex/launchx/examples/mall/pkg/routing"
	"github.com/gospacex/launchx/examples/mall/srv-order/internal/handler"
	"github.com/gospacex/otelx/layer"
)

// Server 实现生成的 order.Order 接口
type Server struct {
	Handler *handler.OrderHandler
	Router  *routing.Router
}

// CreateOrder 下单（3 表事务 + outbox 事件）
func (s *Server) CreateOrder(ctx context.Context, req *orderpb.CreateOrderReq) (*orderpb.CreateOrderResp, error) {
	resp := &orderpb.CreateOrderResp{UserId: req.GetUserId()}
	ctx, span := layer.Start(ctx, "rpc", "order", "CreateOrder")
	defer span.End()
	v := s.Router.Enter(ctx, "CreateOrder", subjectOf(req.GetUserId(), 0))
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.CreateOrder(v.Ctx, req)
}

// GetOrder 查单个订单 + 明细
func (s *Server) GetOrder(ctx context.Context, req *orderpb.GetOrderReq) (*orderpb.GetOrderResp, error) {
	resp := &orderpb.GetOrderResp{UserId: req.GetUserId(), OrderId: req.GetOrderId()}
	ctx, span := layer.Start(ctx, "rpc", "order", "GetOrder")
	defer span.End()
	v := s.Router.Enter(ctx, "GetOrder", subjectOf(req.GetUserId(), req.GetOrderId()))
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.GetOrder(v.Ctx, req)
}

// ListOrders 只读订单流水（orders ⋈ order_items 聚合）
func (s *Server) ListOrders(ctx context.Context, req *orderpb.ListOrdersReq) (*orderpb.ListOrdersResp, error) {
	resp := &orderpb.ListOrdersResp{UserId: req.GetUserId()}
	ctx, span := layer.Start(ctx, "rpc", "order", "ListOrders")
	defer span.End()
	v := s.Router.Enter(ctx, "ListOrders", subjectOf(req.GetUserId(), 0))
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.ListOrders(v.Ctx, req)
}

// Report 4 表 JOIN 报表
func (s *Server) Report(ctx context.Context, req *orderpb.ReportReq) (*orderpb.ReportResp, error) {
	resp := &orderpb.ReportResp{UserId: req.GetUserId()}
	ctx, span := layer.Start(ctx, "rpc", "order", "Report")
	defer span.End()
	v := s.Router.Enter(ctx, "Report", subjectOf(req.GetUserId(), 0))
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.Report(v.Ctx, req)
}

// 编译期断言：Server 必须满足生成的 order.Order 接口
var _ orderpb.Order = (*Server)(nil)

// rateLimited 统一限流响应码（pkg/codes 1005）
func rateLimited(code *int64, message *string) {
	c := codes.Fail(codes.CodeRateLimited, "rate limited")
	*code, *message = c.Code, c.Message
}

// subjectOf 选限流主体：优先 primary，缺失时回落到 fallback。
//
// 订单域的请求只带 user_id（IDL 里没有 operator 概念），故写入口也按
// user_id 分桶 —— 同一用户既是下单主体也是限流主体，这正好是我们想保护的
// 面（防止一个用户把下单接口打满）。
//
// 回落到 0 也没关系：限流器按 subject 分桶，0 号桶只会被"没有身份的请求"
// 共享，而身份缺失在 service 层就会被拒（1004），不会真的落到业务上。
func subjectOf(primary, fallback int64) int64 {
	if primary > 0 {
		return primary
	}
	return fallback
}
