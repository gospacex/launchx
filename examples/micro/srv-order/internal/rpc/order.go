// Package rpc srv-order 的 kitex 传输适配层。
//
// 在分层链上的位置：**rpc** → route → middleware → handler → service →
// repository → model。它是链的最外一节，也是唯一认识 kitex 的一层。
//
// 职责边界（写窄，别让业务渗进来）：
//   - 开 rpc.order.<Method> span，把 kitex 带进来的 ctx 接进链路
//   - 经 route.Router.Enter 走路由与中间件链，被拒时回**对应的**拒绝码
//   - 把请求原样交给 handler（不做任何字段加工）
//
// 本层不碰监听地址：server 选项由 launchx 的 srv 档装配产出（地址取
// config.yaml 的 service.self.addr）。
package rpc

import (
	"context"

	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	orderpb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/order"
	"github.com/gospacex/launchx/examples/micro/pkg/routing"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/handler"
	"github.com/gospacex/otelx/layer"
)

// Server 实现生成的 order.Order 接口
type Server struct {
	Handler *handler.OrderHandler
	Router  *routing.Router
	TraceOn bool // 分层 span 开关（trace.enabled），由装配层注入
}

// begin 开本层 span，返回「收 error 再收尾」的闭包。
//
// 为什么必须过 traceOn 而不是直接 layer.Start：本层是**链的最外一节**，
// trace.enabled=false 时 kitex 的 server span 中间件也被关（internal/mode/srv.go），
// 若这里照开，span 就没有父节点 —— 它会成为一根独立的根 span 被导出，
// 在 Jaeger 上表现为一条只有一节的孤儿 trace。那种噪声比"没有 span"更糟：
// 它会让人以为链路真的通到了这个服务。
func (s *Server) begin(ctx context.Context, method string) (context.Context, layer.End) {
	return layer.Begin(ctx, "rpc", "order", method, s.TraceOn, nil)
}

// CreateOrder 下单（3 表事务 + outbox 事件）
func (s *Server) CreateOrder(ctx context.Context, req *orderpb.CreateOrderReq) (resp *orderpb.CreateOrderResp, err error) {
	resp = &orderpb.CreateOrderResp{UserId: req.GetUserId()}
	ctx, end := s.begin(ctx, "CreateOrder")
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "CreateOrder", subjectOf(req.GetUserId(), 0))
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	return s.Handler.CreateOrder(v.Ctx, req)
}

// GetOrder 查单个订单 + 明细
func (s *Server) GetOrder(ctx context.Context, req *orderpb.GetOrderReq) (resp *orderpb.GetOrderResp, err error) {
	resp = &orderpb.GetOrderResp{UserId: req.GetUserId(), OrderId: req.GetOrderId()}
	ctx, end := s.begin(ctx, "GetOrder")
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "GetOrder", subjectOf(req.GetUserId(), req.GetOrderId()))
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	return s.Handler.GetOrder(v.Ctx, req)
}

// ListOrders 只读订单流水（orders ⋈ order_items 聚合）
func (s *Server) ListOrders(ctx context.Context, req *orderpb.ListOrdersReq) (resp *orderpb.ListOrdersResp, err error) {
	resp = &orderpb.ListOrdersResp{UserId: req.GetUserId()}
	ctx, end := s.begin(ctx, "ListOrders")
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "ListOrders", subjectOf(req.GetUserId(), 0))
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	return s.Handler.ListOrders(v.Ctx, req)
}

// Report 4 表 JOIN 报表
func (s *Server) Report(ctx context.Context, req *orderpb.ReportReq) (resp *orderpb.ReportResp, err error) {
	resp = &orderpb.ReportResp{UserId: req.GetUserId()}
	ctx, end := s.begin(ctx, "Report")
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "Report", subjectOf(req.GetUserId(), 0))
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	return s.Handler.Report(v.Ctx, req)
}

// 编译期断言：Server 必须满足生成的 order.Order 接口
var _ orderpb.Order = (*Server)(nil)

// rejected 把"被中间件拒"翻成对应的业务码。
//
// 鉴权拒与限流拒**必须**回不同的码：客户端的重试策略完全依赖这个区分 ——
// 限流（1005）值得退避重试，鉴权（1004）重试一万次也是同一个结果，
// 只会把无效的流量放大一万倍。混成一个码，等于把鉴权故障伪装成容量问题。
func rejected(code *int64, message *string, abortedBy string) {
	var c codes.Code
	if abortedBy == routing.MiddlewareNameAuth {
		c = codes.Fail(codes.CodeUnauthorized, "unauthorized")
	} else {
		c = codes.Fail(codes.CodeRateLimited, "rate limited")
	}
	*code, *message = c.Code, c.Message
}

// subjectOf 选限流主体：优先 primary，缺失时回落到 fallback。
//
// 订单域的请求只带 user_id（IDL 里没有 operator 概念），故写入口也按
// user_id 分桶 —— 同一用户既是下单主体也是限流主体，这正好是我们想保护的
// 面（防止一个用户把下单接口打满）。
//
// 回落到 0 也没关系：限流器按 subject 分桶，0 号桶只会被"没有身份的请求"
// 共享，而没有身份的请求在鉴权那一节就被拒了（codes.CodeUnauthorized），
// 不会真的落到业务上。
func subjectOf(primary, fallback int64) int64 {
	if primary > 0 {
		return primary
	}
	return fallback
}
