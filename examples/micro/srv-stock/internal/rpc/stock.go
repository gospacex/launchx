// Package rpc srv-stock 的 kitex 传输适配层。
//
// 在分层链上的位置：**rpc** → route → middleware → handler → service →
// repository → model。它是链的最外一节，也是唯一认识 kitex 的一层。
//
// 职责边界（写窄，别让业务渗进来）：
//   - 开 rpc.stock.<Method> span，把 kitex 带进来的 ctx 接进链路
//   - 经 route.Router.Enter 走路由与中间件链，被拒时回限流码
//   - 把请求原样交给 handler（不做任何字段加工）
//
// 限流主体一律按**商品维度**取（库存域的热点天然是"某个爆款商品"，按用户
// 分桶反而拦不住集中打一个商品的流量）。列表与报表没有商品维度，主体取 0
// —— 它们本来就限页数，共享 0 号桶不额外引入错杀。
//
// 本层不碰监听地址：server 选项由 launchx 的 srv 档装配产出（地址取
// config.yaml 的 service.self.addr）。
package rpc

import (
	"context"

	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	stockpb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/stock"
	"github.com/gospacex/launchx/examples/micro/pkg/routing"
	"github.com/gospacex/launchx/examples/micro/pkg/tracex"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/handler"
)

// Server 实现生成的 stock.Stock 接口
type Server struct {
	Handler *handler.StockHandler
	Router  *routing.Router
}

// GetStock 查库存快照
func (s *Server) GetStock(ctx context.Context, req *stockpb.GetStockReq) (*stockpb.GetStockResp, error) {
	resp := &stockpb.GetStockResp{ProductId: req.GetProductId()}
	ctx, span := tracex.Start(ctx, "rpc", "stock", "GetStock")
	defer span.End()
	v := s.Router.Enter(ctx, "GetStock", req.GetProductId())
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.GetStock(v.Ctx, req)
}

// ListStocks 库存列表（联表聚合分页）
func (s *Server) ListStocks(ctx context.Context, req *stockpb.ListStocksReq) (*stockpb.ListStocksResp, error) {
	resp := &stockpb.ListStocksResp{}
	ctx, span := tracex.Start(ctx, "rpc", "stock", "ListStocks")
	defer span.End()
	v := s.Router.Enter(ctx, "ListStocks", 0)
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.ListStocks(v.Ctx, req)
}

// ReserveStock 预占（加锁 + 3 表事务 + outbox）
func (s *Server) ReserveStock(ctx context.Context, req *stockpb.ReserveStockReq) (*stockpb.ReserveStockResp, error) {
	resp := &stockpb.ReserveStockResp{ProductId: req.GetProductId()}
	ctx, span := tracex.Start(ctx, "rpc", "stock", "ReserveStock")
	defer span.End()
	v := s.Router.Enter(ctx, "ReserveStock", req.GetProductId())
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.ReserveStock(v.Ctx, req)
}

// ReleaseStock 释放预占（幂等）
func (s *Server) ReleaseStock(ctx context.Context, req *stockpb.ReleaseStockReq) (*stockpb.ReleaseStockResp, error) {
	resp := &stockpb.ReleaseStockResp{ProductId: req.GetProductId()}
	ctx, span := tracex.Start(ctx, "rpc", "stock", "ReleaseStock")
	defer span.End()
	v := s.Router.Enter(ctx, "ReleaseStock", req.GetProductId())
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.ReleaseStock(v.Ctx, req)
}

// ConfirmStock 确认预占（出库）
func (s *Server) ConfirmStock(ctx context.Context, req *stockpb.ConfirmStockReq) (*stockpb.ConfirmStockResp, error) {
	resp := &stockpb.ConfirmStockResp{ProductId: req.GetProductId()}
	ctx, span := tracex.Start(ctx, "rpc", "stock", "ConfirmStock")
	defer span.End()
	v := s.Router.Enter(ctx, "ConfirmStock", req.GetProductId())
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.ConfirmStock(v.Ctx, req)
}

// AdjustStock 入库 / 盘点
func (s *Server) AdjustStock(ctx context.Context, req *stockpb.AdjustStockReq) (*stockpb.AdjustStockResp, error) {
	resp := &stockpb.AdjustStockResp{ProductId: req.GetProductId()}
	ctx, span := tracex.Start(ctx, "rpc", "stock", "AdjustStock")
	defer span.End()
	v := s.Router.Enter(ctx, "AdjustStock", req.GetProductId())
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.AdjustStock(v.Ctx, req)
}

// Report 3 表 JOIN 报表
func (s *Server) Report(ctx context.Context, req *stockpb.ReportReq) (*stockpb.ReportResp, error) {
	resp := &stockpb.ReportResp{}
	ctx, span := tracex.Start(ctx, "rpc", "stock", "Report")
	defer span.End()
	v := s.Router.Enter(ctx, "Report", 0)
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.Report(v.Ctx, req)
}

// 编译期断言：Server 必须满足生成的 stock.Stock 接口
var _ stockpb.Stock = (*Server)(nil)

// rateLimited 统一限流响应码（pkg/codes 1005）
func rateLimited(code *int64, message *string) {
	c := codes.Fail(codes.CodeRateLimited, "rate limited")
	*code, *message = c.Code, c.Message
}
