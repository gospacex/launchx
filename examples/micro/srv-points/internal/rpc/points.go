// Package rpc srv-points 的 kitex 传输适配层。
//
// 在分层链上的位置：**rpc** → route → middleware → handler → service →
// repository → model。它是链的最外一节，也是唯一认识 kitex 的一层。
//
// 职责边界（写窄，别让业务渗进来）：
//   - 开 rpc.points.<Method> span，把 kitex 带进来的 ctx 接进链路
//   - 经 route.Router.Enter 走路由与中间件链，被拒时回限流码
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
	"github.com/gospacex/launchx/examples/micro/pkg/tracex"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/handler"
)

// Server 实现生成的 points.Points 接口
type Server struct {
	Handler *handler.PointsHandler
	Router  *routing.Router
}

// GetAccount 查积分账户（联表聚合流水笔数与最近变动）
func (s *Server) GetAccount(ctx context.Context, req *pointspb.GetAccountReq) (*pointspb.GetAccountResp, error) {
	resp := &pointspb.GetAccountResp{UserId: req.GetUserId()}
	ctx, span := tracex.Start(ctx, "rpc", "points", "GetAccount")
	defer span.End()
	v := s.Router.Enter(ctx, "GetAccount", req.GetUserId())
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.GetAccount(v.Ctx, req)
}

// Earn 入账（两层幂等 + 用户锁 + 2 表事务 + outbox）
func (s *Server) Earn(ctx context.Context, req *pointspb.EarnReq) (*pointspb.EarnResp, error) {
	resp := &pointspb.EarnResp{UserId: req.GetUserId()}
	ctx, span := tracex.Start(ctx, "rpc", "points", "Earn")
	defer span.End()
	v := s.Router.Enter(ctx, "Earn", req.GetUserId())
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.Earn(v.Ctx, req)
}

// Consume 消费（同上反向）
func (s *Server) Consume(ctx context.Context, req *pointspb.ConsumeReq) (*pointspb.ConsumeResp, error) {
	resp := &pointspb.ConsumeResp{UserId: req.GetUserId()}
	ctx, span := tracex.Start(ctx, "rpc", "points", "Consume")
	defer span.End()
	v := s.Router.Enter(ctx, "Consume", req.GetUserId())
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.Consume(v.Ctx, req)
}

// ListLedgers 流水列表（联表带出账户余额）
func (s *Server) ListLedgers(ctx context.Context, req *pointspb.ListLedgersReq) (*pointspb.ListLedgersResp, error) {
	resp := &pointspb.ListLedgersResp{UserId: req.GetUserId()}
	ctx, span := tracex.Start(ctx, "rpc", "points", "ListLedgers")
	defer span.End()
	v := s.Router.Enter(ctx, "ListLedgers", req.GetUserId())
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.ListLedgers(v.Ctx, req)
}

// Report 2 表 JOIN 报表
func (s *Server) Report(ctx context.Context, req *pointspb.ReportReq) (*pointspb.ReportResp, error) {
	resp := &pointspb.ReportResp{}
	ctx, span := tracex.Start(ctx, "rpc", "points", "Report")
	defer span.End()
	// 报表可以不带 user_id（出全量账户），此时主体为 0 —— 0 号桶由这类
	// 低频的运营请求共享，不会挤占普通账户的配额。
	v := s.Router.Enter(ctx, "Report", req.GetUserId())
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.Report(v.Ctx, req)
}

// 编译期断言：Server 必须满足生成的 points.Points 接口
var _ pointspb.Points = (*Server)(nil)

// rateLimited 统一限流响应码（pkg/codes 1005）
func rateLimited(code *int64, message *string) {
	c := codes.Fail(codes.CodeRateLimited, "rate limited")
	*code, *message = c.Code, c.Message
}
