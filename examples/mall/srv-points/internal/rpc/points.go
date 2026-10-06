// Package rpc srv-points 的 kitex 服务端桩：实现 pkg/kitex_gen 生成的
// points.Points 接口（GetAccount / Earn / Consume / ListLedgers / Report）。
//
// 职责边界：
//   - 每方法入口 tracex.Start(ctx, "service", "points", "<Method>") 分层埋点
//   - 身份校验在 handler 内读 req.GetUserId()（kitex 无 metadata 透传约定）
//   - 业务错误以 Response.Code/Message 表达（pkg/codes）；网络/内部错误走
//     Go error（kitex 错误体），两套不混用
//   - 限流由 middleware.RateLimiter 在入口完成（kvx 计数、fail-open）
//
// 本包只出 impl，不碰监听地址：server 选项由 launchx 的 srv 档装配产出。
package rpc

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/mall/pkg/codes"
	pointspb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/points"
	"github.com/gospacex/launchx/examples/mall/pkg/tracex"
	"github.com/gospacex/launchx/examples/mall/srv-points/internal/middleware"
	"github.com/gospacex/launchx/examples/mall/srv-points/internal/service"
)

// Server 实现生成的 points.Points 接口
type Server struct {
	Svc     *service.PointsService
	Limiter *middleware.RateLimiter
}

// guard 方法入口公共逻辑：分层 span（调用方必须 defer span.End()）+ 限流。
//
// span 必须由调用方 End：丢弃句柄会让 span 永不导出，且下游子 span 挂在
// 悬空 parent 上（Jaeger 上 missing-parent）。
//
// 限流主体一律按 user_id：积分是账户级资源，按用户分桶才能让"某个用户在
// 刷接口"不影响其他人。
func (s *Server) guard(ctx context.Context, method string, userID int64) (context.Context, trace.Span, bool) {
	ctx, span := tracex.Start(ctx, "service", "points", method)
	if s.Limiter != nil && !s.Limiter.Allow(ctx, userID) {
		return ctx, span, false
	}
	return ctx, span, true
}

// GetAccount 查积分账户
func (s *Server) GetAccount(ctx context.Context, req *pointspb.GetAccountReq) (*pointspb.GetAccountResp, error) {
	resp := &pointspb.GetAccountResp{UserId: req.GetUserId()}
	ctx, span, ok := s.guard(ctx, "GetAccount", req.GetUserId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.GetAccount(ctx, req)
}

// Earn 入账（两层幂等 + 用户锁 + 2 表事务 + outbox）
func (s *Server) Earn(ctx context.Context, req *pointspb.EarnReq) (*pointspb.EarnResp, error) {
	resp := &pointspb.EarnResp{UserId: req.GetUserId()}
	ctx, span, ok := s.guard(ctx, "Earn", req.GetUserId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.Earn(ctx, req)
}

// Consume 消费（同上反向）
func (s *Server) Consume(ctx context.Context, req *pointspb.ConsumeReq) (*pointspb.ConsumeResp, error) {
	resp := &pointspb.ConsumeResp{UserId: req.GetUserId()}
	ctx, span, ok := s.guard(ctx, "Consume", req.GetUserId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.Consume(ctx, req)
}

// ListLedgers 单表分页流水
func (s *Server) ListLedgers(ctx context.Context, req *pointspb.ListLedgersReq) (*pointspb.ListLedgersResp, error) {
	resp := &pointspb.ListLedgersResp{UserId: req.GetUserId()}
	ctx, span, ok := s.guard(ctx, "ListLedgers", req.GetUserId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.ListLedgers(ctx, req)
}

// Report 2 表 JOIN 报表
func (s *Server) Report(ctx context.Context, req *pointspb.ReportReq) (*pointspb.ReportResp, error) {
	resp := &pointspb.ReportResp{}
	ctx, span, ok := s.guard(ctx, "Report", req.GetUserId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.Report(ctx, req)
}

// 编译期断言：Server 必须满足生成的 points.Points 接口
var _ pointspb.Points = (*Server)(nil)

// rateLimited 统一限流响应码（pkg/codes 1005）
func rateLimited() (code int64, msg string) {
	c := codes.Fail(codes.CodeRateLimited, "rate limited")
	return c.Code, c.Message
}
