// Package rpc 入向传输适配层：链的最外一节，也是唯一认识 kitex 的一层。
//
// 职责边界（写窄，别让业务渗进来）：
//   - 实现生成的 stock.Stock 接口**全部 7 个方法**（kitex 按接口签名约束 NewServer
//     入参，少一个方法编译期即失败）；
//   - 开 rpc.stock.<Op> 分层 span，把 kitex 带进来的 ctx 接进链路；
//   - GetStock 经 route.Guard 走准入闸门，被拒时回**对应的**拒绝码
//     （身份非法 401 / 超主体上限 429）；未接线的六个方法只开 span 后原样委派
//     handler（不过闸门 —— 未接线、无业务主体可限，开了只会误伤限流桶）；
//   - 把请求原样交给 handler（不做任何字段加工）。
//
// **codes.Unimplemented 的唯一落点是 handler**：本层的方法一律委派 handler，
// 不在本层产出任何业务码。
//
// 本层不做出向调用：apps 的 srv 之间不互调，且 srv 档没有出向客户端的装配来源，
// 故这里只负责入向。
package rpc

import (
	"context"
	"strconv"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/gospacex/otelx/layer"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/handler"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/route"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	stockpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/stock"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/rpcwire"
)

// Server 实现生成的 stock.Stock 接口。
type Server struct {
	handler *handler.StockHandler
	guard   *route.Guard
	traceOn bool // trace.enabled（总闸），由装配层注入
}

// New 构造入向适配层。
func New(h *handler.StockHandler, g *route.Guard, traceOn bool) *Server {
	return &Server{handler: h, guard: g, traceOn: traceOn}
}

// 编译期断言：接口必须全实现（放在 rpc 层，不放 handler）。
var _ stockpb.Stock = (*Server)(nil)

// GetStock 查库存（本域唯一已接线的对外方法，只读）。
//
// 经 admit 闸门后委派 handler。结果列表命名，供 defer 里的 end(err) 覆盖所有
// return 路径（Go 要求「全命名或全不命名」）。
func (s *Server) GetStock(ctx context.Context, req *stockpb.GetStockReq) (resp *stockpb.GetStockResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "stock", "GetStock", s.traceOn, nil)
	defer func() { end(err) }()

	if aerr := s.admit(ctx); aerr != nil {
		return nil, aerr
	}
	return s.handler.GetStock(ctx, req)
}

// ListStocks 未接线：本示例不提供库存列表。只开 rpc.stock.ListStocks span 后
// 原样委派 handler（错误码映射归 handler）；**不过 admit 闸门**。
func (s *Server) ListStocks(ctx context.Context, req *stockpb.ListStocksReq) (resp *stockpb.ListStocksResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "stock", "ListStocks", s.traceOn, nil)
	defer func() { end(err) }()
	return s.handler.ListStocks(ctx, req)
}

// ReserveStock 未接线：同上。
func (s *Server) ReserveStock(ctx context.Context, req *stockpb.ReserveStockReq) (resp *stockpb.ReserveStockResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "stock", "ReserveStock", s.traceOn, nil)
	defer func() { end(err) }()
	return s.handler.ReserveStock(ctx, req)
}

// ReleaseStock 未接线：同上。
func (s *Server) ReleaseStock(ctx context.Context, req *stockpb.ReleaseStockReq) (resp *stockpb.ReleaseStockResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "stock", "ReleaseStock", s.traceOn, nil)
	defer func() { end(err) }()
	return s.handler.ReleaseStock(ctx, req)
}

// ConfirmStock 未接线：同上。
func (s *Server) ConfirmStock(ctx context.Context, req *stockpb.ConfirmStockReq) (resp *stockpb.ConfirmStockResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "stock", "ConfirmStock", s.traceOn, nil)
	defer func() { end(err) }()
	return s.handler.ConfirmStock(ctx, req)
}

// AdjustStock 未接线：同上。
func (s *Server) AdjustStock(ctx context.Context, req *stockpb.AdjustStockReq) (resp *stockpb.AdjustStockResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "stock", "AdjustStock", s.traceOn, nil)
	defer func() { end(err) }()
	return s.handler.AdjustStock(ctx, req)
}

// Report 未接线：同上。
func (s *Server) Report(ctx context.Context, req *stockpb.ReportReq) (resp *stockpb.ReportResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "stock", "Report", s.traceOn, nil)
	defer func() { end(err) }()
	return s.handler.Report(ctx, req)
}

// admit 走准入闸门：身份非法回 Unauthorized，超出主体窗口上限回 RateLimited。
//
// 身份来源按优先级取**两处**（前者非零即短路，不再往下取）：
//  1. 同进程 ctx 里的 routing identity（`routing.IdentityFrom`）—— 单测 / 直连时有效；
//  2. kitex metainfo 的 `mesh-user-id`（由 bff 在出向调用前写入 —— 跨进程的身份载体）。
//     **解析失败即 Unauthorized**：身份头坏了要出声，不许静默降级成匿名。
//
// ⚠️ 与 order/points/product 的真实差异：本域请求 `GetStockReq` **只有 product_id、
// 不带 user_id**，故这里**没有**那三域里「回落到本次请求 IDL 里的 user_id」这第三级。
// 无身份时（metainfo 键不存在）落 "svc" 桶，fail-open —— 这是刻意留白而非漏写：
// 没有可回落的主体字段，硬造一个只会把匿名流量错记到某个商品维度上。
//
// 两处拒绝码的分工：Unauthorized = 身份**非法**（解析失败 / 为 0）；
// RateLimited = `route.Guard.Admit` 返回 false，即超过该主体的窗口上限
// （本域 middleware 的 180/分钟；无身份时按服务级 "svc" 桶计数）。
func (s *Server) admit(ctx context.Context) error {
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
	if s.guard.Admit(ctx, id) {
		return nil
	}
	return codes.New(codes.RateLimited, "rate limited")
}

// identityMetaKey bff 透传身份的 kitex metainfo 键名；与写入侧 MUST 一致。
// 值只有一处定义（rpcwire.IdentityMetaKey）：bff 写、四域读，写死五份字面量
// 就有「改一处漏四处」的空间，而漏掉的症状是身份永远为空（限流退化成服务级）。
const identityMetaKey = rpcwire.IdentityMetaKey
