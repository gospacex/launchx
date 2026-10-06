// Package rpc demo-srv 的 rpcx 服务端桩：实现 pkg/kitex_gen 生成的
// order.Order 接口（CreateOrder / GetOrder / Report / ListOrders）。
//
// 职责边界（brief）：
//   - 每方法入口 tracex.Start(ctx, "service", "order", "<Method>") 分层埋点
//   - 身份校验在 handler 内读 req.GetUserId()（kitex 无 metadata 透传约定，
//     批 2 已核实）；业务错误以 Response.Code/Message 表达（pkg/codes），
//     网络/内部错误走 Go error（kitex 错误体），两套不混用
//   - 限流由 middleware.RateLimiter 在入口完成（kvx 原子自增，门控关闭/fail-open）
//
// 装配（app/setup.go）：server 选项由 launchx 的 srv 档装配产出（地址取
// config.yaml 的 service.addr），本包只出 impl，不碰地址：
//
//	order.NewServer(&rpc.Server{Svc: svc, Limiter: limiter}, opts...)
package rpc

import (
	"context"

	orderpb "github.com/gospacex/launchx/examples/micro/srv/pkg/kitex_gen/order"

	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/micro/srv/internal/middleware"
	"github.com/gospacex/launchx/examples/micro/srv/internal/service"
	"github.com/gospacex/launchx/examples/micro/srv/internal/tracex"
	"github.com/gospacex/launchx/examples/micro/srv/pkg/codes"
)

// Server 实现生成的 order.Order 接口
type Server struct {
	Svc     *service.OrderService
	Limiter *middleware.RateLimiter
}

// guard 方法入口公共逻辑：分层 span（调用方必须 defer span.End()）+ 限流。
//
// span 必须由调用方 End：此前这里写成 `ctx, _ = tracex.Start(...)`，
// span 句柄被丢弃、永不 End —— 既不导出（Jaeger 看不到这一层），
// 又让下游子 span 挂在悬空 parent 上（missing-parent）。
func (s *Server) guard(ctx context.Context, method string, userID int64) (context.Context, trace.Span, bool) {
	ctx, span := tracex.Start(ctx, "service", "order", method)
	if s.Limiter != nil && !s.Limiter.Allow(ctx, userID) {
		return ctx, span, false
	}
	return ctx, span, true
}

// CreateOrder 下单（预占库存 + 本地事务 + outbox）
func (s *Server) CreateOrder(ctx context.Context, req *orderpb.CreateOrderReq) (*orderpb.CreateOrderResp, error) {
	resp := &orderpb.CreateOrderResp{UserId: req.GetUserId()}
	ctx, span, ok := s.guard(ctx, "CreateOrder", req.GetUserId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.CreateOrder(ctx, req)
}

// GetOrder 查单个订单 + 明细
func (s *Server) GetOrder(ctx context.Context, req *orderpb.GetOrderReq) (*orderpb.GetOrderResp, error) {
	resp := &orderpb.GetOrderResp{UserId: req.GetUserId(), OrderId: req.GetOrderId()}
	ctx, span, ok := s.guard(ctx, "GetOrder", req.GetUserId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.GetOrder(ctx, req)
}

// Report 4 表 JOIN 报表
func (s *Server) Report(ctx context.Context, req *orderpb.ReportReq) (*orderpb.ReportResp, error) {
	resp := &orderpb.ReportResp{UserId: req.GetUserId()}
	ctx, span, ok := s.guard(ctx, "Report", req.GetUserId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.Report(ctx, req)
}

// ListOrders 只读订单流水
func (s *Server) ListOrders(ctx context.Context, req *orderpb.ListOrdersReq) (*orderpb.ListOrdersResp, error) {
	resp := &orderpb.ListOrdersResp{UserId: req.GetUserId()}
	ctx, span, ok := s.guard(ctx, "ListOrders", req.GetUserId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.ListOrders(ctx, req)
}

// 编译期断言：Server 必须满足生成的 order.Order 接口
// （kitex order.NewServer(handler order.Order) 的 handler 类型）
var _ orderpb.Order = (*Server)(nil)

// rateLimited 统一限流响应码（pkg/codes 1005）
func rateLimited() (code int64, msg string) {
	c := codes.Fail(codes.CodeRateLimited, "rate limited")
	return c.Code, c.Message
}
