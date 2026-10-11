// Package rpc 入向传输适配层：链的最外一节，也是唯一认识 kitex 的一层。
//
// 职责边界（写窄，别让业务渗进来）：
//   - 实现生成的 points.Points 接口**全部方法**（kitex 按接口签名约束 NewServer
//     入参，少一个方法编译期即失败）；
//   - 开 rpc.points.<Op> 分层 span，把 kitex 带进来的 ctx 接进链路；
//   - GetAccount 经 route.Guard 走准入闸门，被拒时回**对应的**拒绝码
//     （身份非法 401 / 越权 403 / 超主体上限 429）；未接线的四个方法只开 span 后
//     原样委派 handler（不过闸门 —— 未接线、无业务主体可限，开了只会误伤）；
//   - 把请求原样交给 handler（不做任何字段加工）。
//
// 2026-10-10 起本层还承担**归属校验**（IDOR 防护）：身份是传输层的事，只有本层
// 认识 metainfo；service 层按纪律只认入参。详见 GetAccount 注释。
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
// 经 resolveSubject 解析主体 → 过闸门 → 委派 handler。结果列表命名，供 defer
// 里的 end(err) 覆盖所有 return 路径（Go 要求「全命名或全不命名」）。
//
// 归属校验（IDOR 防护，2026-10-10 加固）：余额是 PII，主体可信但请求的账户不属于
// 该主体时回 403。校验点在本层而非 service —— 身份是传输层的事（metainfo / kitex），
// service 按纪律只认入参，不该解析 metainfo（与 order 域同款，见 order/rpc 注释）。
func (s *Server) GetAccount(ctx context.Context, req *points.GetAccountReq) (resp *points.GetAccountResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "points", "GetAccount", s.traceOn, nil)
	defer func() { end(err) }()

	id, authed, aerr := s.resolveSubject(ctx, req.GetUserId())
	if aerr != nil {
		return nil, aerr
	}
	if !s.guard.Admit(ctx, id) {
		return nil, codes.New(codes.RateLimited, "rate limited")
	}
	resp, err = s.handler.GetAccount(ctx, req)
	if err != nil {
		return nil, err
	}
	if authed && uint64(resp.GetUserId()) != id.UserID {
		return nil, codes.New(codes.Forbidden,
			"account does not belong to the authenticated subject")
	}
	return resp, nil
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

// resolveSubject 解析本次调用的主体，并回答「这个主体是不是**可信来源**」。
//
// 身份来源按优先级取**三处**（前者非零即短路，不再往下取）：
//  1. 同进程 ctx 里的 routing identity（`routing.IdentityFrom`）—— 单测 / 直连时有效 → 可信；
//  2. kitex metainfo 的 `mesh-user-id`（由 bff 在出向调用前写入 —— 跨进程的身份载体）。
//     **解析失败即 Unauthorized**：身份头坏了要出声，不许静默降级成匿名 → 可信；
//  3. 回落到本次请求 IDL 里的 `user_id` → **不可信**（调用方自己声明的，不是鉴权结果）。
//
// 第三个返回值决定**归属校验要不要执行**：只有可信来源的主体才拿去和资源比对。
// 若把回落值也当真，越权者只要在请求里写上被害者的 user_id，归属校验就被自己的
// 入参喂成全真 —— 那种校验有还不如没有。
//
// authed=false 且 id 为零（既没身份也没声明）时仍放行并按 "svc" 桶计数：
// apps 的鉴权闸门在 **bff**（无凭据 401），srv 侧对「没带身份」保持 fail-open，
// 只为限流分桶。
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

// identityMetaKey bff 透传身份的 kitex metainfo 键名；与写入侧 MUST 一致。
// 值只有一处定义（rpcwire.IdentityMetaKey）：bff 写、四域读，写死五份字面量
// 就有「改一处漏四处」的空间，而漏掉的症状是身份永远为空（限流退化成服务级）。
const identityMetaKey = rpcwire.IdentityMetaKey
