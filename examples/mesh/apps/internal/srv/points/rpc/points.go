// Package rpc 入向传输适配层：链的最外一节，也是唯一认识 kitex 的一层。
//
// 职责边界（写窄，别让业务渗进来）：
//   - 实现生成的 points.Points 接口**全部方法**（kitex 按接口签名约束 NewServer
//     入参，少一个方法编译期即失败）；
//   - 开 rpc.points.<Op> 分层 span，把 kitex 带进来的 ctx 接进链路；
//   - GetAccount 经 route.Guard 走准入闸门，被拒时回**对应的**拒绝码
//     （身份非法 401 / 超主体上限 429）；未接线的四个方法只开 span 后原样委派
//     handler（不过闸门 —— 未接线、无业务主体可限，开了只会误伤）；
//   - 把请求原样交给 handler（不做任何字段加工）。
//
// 与 Task 18 的 order 模板同款口径：rpc 的方法**一律委派 handler**，不在本层
// 产出业务码；**codes.Unimplemented 的唯一落点是 handler**。
//
// 本层不做出向调用：apps 的 srv 之间不互调（见 mesh-entry-chain 的同名 Requirement），
// 且 srv 档没有出向客户端的装配来源，故这里只负责入向。
package rpc

import (
	"context"
	"strconv"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/gospacex/otelx/layer"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/handler"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/route"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	points "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/points"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/rpcwire"
)

// Server 实现生成的 points.Points 接口。
type Server struct {
	handler *handler.PointsHandler
	guard   *route.Guard
	traceOn bool // trace.enabled（总闸），由装配层注入
}

// New 构造入向适配层。
func New(h *handler.PointsHandler, g *route.Guard, traceOn bool) *Server {
	return &Server{handler: h, guard: g, traceOn: traceOn}
}

// 编译期断言：接口必须全实现（放在 rpc 层，不放 handler）。
var _ points.Points = (*Server)(nil)

// GetAccount 查积分账户（本域唯一已接线的对外方法，只读）。
//
// 经 admit 闸门后委派 handler。结果列表命名，供 defer 里的 end(err) 覆盖所有
// return 路径（Go 要求「全命名或全不命名」）。
func (s *Server) GetAccount(ctx context.Context, req *points.GetAccountReq) (resp *points.GetAccountResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "points", "GetAccount", s.traceOn, nil)
	defer func() { end(err) }()

	if aerr := s.admit(ctx, req.GetUserId()); aerr != nil {
		return nil, aerr
	}
	return s.handler.GetAccount(ctx, req)
}

// Earn 未接线：本示例不对 RPC 暴露写入路径。与 GetAccount 同形 —— 只开
// rpc.points.Earn span 后原样委派 handler（错误码映射归 handler）；
// **不过 admit 闸门**：未接线、无业务主体可限。
func (s *Server) Earn(ctx context.Context, req *points.EarnReq) (resp *points.EarnResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "points", "Earn", s.traceOn, nil)
	defer func() { end(err) }()
	return s.handler.Earn(ctx, req)
}

// Consume 未接线：同上，只开 span 后委派 handler，不过 admit 闸门。
func (s *Server) Consume(ctx context.Context, req *points.ConsumeReq) (resp *points.ConsumeResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "points", "Consume", s.traceOn, nil)
	defer func() { end(err) }()
	return s.handler.Consume(ctx, req)
}

// ListLedgers 未接线：同上。
func (s *Server) ListLedgers(ctx context.Context, req *points.ListLedgersReq) (resp *points.ListLedgersResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "points", "ListLedgers", s.traceOn, nil)
	defer func() { end(err) }()
	return s.handler.ListLedgers(ctx, req)
}

// Report 未接线：同上。
func (s *Server) Report(ctx context.Context, req *points.ReportReq) (resp *points.ReportResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "points", "Report", s.traceOn, nil)
	defer func() { end(err) }()
	return s.handler.Report(ctx, req)
}

// admit 走准入闸门：身份非法回 Unauthorized，超出主体窗口上限回 RateLimited。
//
// 身份来源按优先级取**三处**（前者非零即短路，不再往下取）：
//  1. 同进程 ctx 里的 routing identity（`routing.IdentityFrom`）—— 单测 / 直连时有效；
//  2. kitex metainfo 的 `mesh-user-id`（由 bff 在出向调用前写入 —— 跨进程的身份载体）。
//     **解析失败即 Unauthorized**：身份头坏了要出声，不许静默降级成匿名；
//  3. 回落到本次请求 IDL 里的 `user_id`。
//
// 两处拒绝码的分工：Unauthorized = 身份**非法**（解析失败 / 为 0）；
// RateLimited = `route.Guard.Admit` 返回 false，即超过该主体的窗口上限
// （本域 middleware 的 60/分钟；无身份时按服务级 "svc" 桶计数）。
//
// 注意 Unauthorized 的语义是「身份**非法**」，不是「没带身份」：没带身份（metainfo
// 键不存在）走第 3 条回落，fail-open。apps 的鉴权口径把传输层身份下沉给 Istio，
// 故应用侧不因匿名而拒绝。
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

// identityMetaKey bff 透传身份的 kitex metainfo 键名；与写入侧 MUST 一致。
// 值只有一处定义（rpcwire.IdentityMetaKey）：bff 写、四域读，写死五份字面量
// 就有「改一处漏四处」的空间，而漏掉的症状是身份永远为空（限流退化成服务级）。
const identityMetaKey = rpcwire.IdentityMetaKey
