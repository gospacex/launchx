// Package rpc srv-points 的 kitex 传输适配层。
//
// 在分层链上的位置：**rpc** → route → middleware → handler → service →
// repository → model。它是链的最外一节，也是唯一认识 kitex 的一层。
//
// 职责边界（写窄，别让业务渗进来）：
//   - 开 rpc.points.<Method> span，把 kitex 带进来的 ctx 接进链路
//   - 经 route.Router.Enter 走路由与中间件链，被拒时回**对应的**拒绝码（鉴权 1004 / 限流 1005，不混成一个）
//   - 把请求原样交给 handler（不做任何字段加工）
//
// 限流主体一律按 user_id：积分是账户级资源，按用户分桶才能让"某个用户在
// 刷接口"不影响其他人（kitex 无 metadata 透传约定，身份只能从 req 里读）。
//
// 本层不碰监听地址：server 选项由 launchx 的 srv 档装配产出（地址取
// config.yaml 的 service.self.addr）。
package rpc

import (
	"context"

	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	pointspb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/points"
	"github.com/gospacex/launchx/examples/micro/pkg/routing"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/handler"
	"github.com/gospacex/otelx/layer"
)

// Server 实现生成的 points.Points 接口
type Server struct {
	Handler *handler.PointsHandler
	Router  *routing.Router
	TraceOn bool // 分层 span 开关（trace.enabled），由装配层注入
}

// begin 开本层 span，返回「收 error 再收尾」的闭包。
//
// 为什么必须过 traceOn 而不是直接 layer.Start：本层是**链的最外一节**，
// trace.enabled=false 时 kitex 的 server span 中间件也被关
// （internal/mode/srv.go），若这里照开，span 就没有父节点 —— 它会成为一根
// 独立的根 span 被导出，在 Jaeger 上表现为一条只有一节的孤儿 trace。那种
// 噪声比「没有 span」更糟：它会让人以为链路真的通到了这个服务。
func (s *Server) begin(ctx context.Context, method string) (context.Context, layer.End) {
	return layer.Begin(ctx, "rpc", "points", method, s.TraceOn, nil)
}

// GetAccount 查积分账户（联表聚合流水笔数与最近变动）
func (s *Server) GetAccount(ctx context.Context, req *pointspb.GetAccountReq) (*pointspb.GetAccountResp, error) {
	resp := &pointspb.GetAccountResp{UserId: req.GetUserId()}
	ctx, end := s.begin(ctx, "GetAccount")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "GetAccount", req.GetUserId())
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.GetAccount(v.Ctx, req)
	return resp, err
}

// Earn 入账（两层幂等 + 用户锁 + 2 表事务 + outbox）
func (s *Server) Earn(ctx context.Context, req *pointspb.EarnReq) (*pointspb.EarnResp, error) {
	resp := &pointspb.EarnResp{UserId: req.GetUserId()}
	ctx, end := s.begin(ctx, "Earn")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "Earn", req.GetUserId())
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.Earn(v.Ctx, req)
	return resp, err
}

// Consume 消费（同上反向）
func (s *Server) Consume(ctx context.Context, req *pointspb.ConsumeReq) (*pointspb.ConsumeResp, error) {
	resp := &pointspb.ConsumeResp{UserId: req.GetUserId()}
	ctx, end := s.begin(ctx, "Consume")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "Consume", req.GetUserId())
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.Consume(v.Ctx, req)
	return resp, err
}

// ListLedgers 流水列表（联表带出账户余额）
func (s *Server) ListLedgers(ctx context.Context, req *pointspb.ListLedgersReq) (*pointspb.ListLedgersResp, error) {
	resp := &pointspb.ListLedgersResp{UserId: req.GetUserId()}
	ctx, end := s.begin(ctx, "ListLedgers")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "ListLedgers", req.GetUserId())
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.ListLedgers(v.Ctx, req)
	return resp, err
}

// Report 2 表 JOIN 报表
func (s *Server) Report(ctx context.Context, req *pointspb.ReportReq) (*pointspb.ReportResp, error) {
	resp := &pointspb.ReportResp{}
	ctx, end := s.begin(ctx, "Report")
	var err error
	defer func() { end(err) }()
	// 报表可以不带 user_id（出全量账户），此时主体为 0 —— 0 号桶由这类
	// 低频的运营请求共享，不会挤占普通账户的配额。
	v := s.Router.Enter(ctx, "Report", req.GetUserId())
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.Report(v.Ctx, req)
	return resp, err
}

// 编译期断言：Server 必须满足生成的 points.Points 接口
var _ pointspb.Points = (*Server)(nil)

// rejected 把「被中间件拒」翻成对应的业务码。
//
// 鉴权拒与限流拒**必须**回不同的码：客户端的重试策略完全依赖这个区分 ——
// 限流（1005）值得退避重试，鉴权（1004）重试一万次也是同一个结果，
// 只会把无效流量放大一万倍。混成一个码，等于把鉴权故障伪装成容量问题。
func rejected(code *int64, message *string, abortedBy string) {
	var c codes.Code
	if abortedBy == routing.MiddlewareNameAuth {
		c = codes.Fail(codes.CodeUnauthorized, "unauthorized")
	} else {
		c = codes.Fail(codes.CodeRateLimited, "rate limited")
	}
	*code, *message = c.Code, c.Message
}
