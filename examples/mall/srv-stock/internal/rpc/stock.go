// Package rpc srv-stock 的 kitex 服务端桩：实现 pkg/kitex_gen 生成的
// stock.Stock 接口（GetStock / ListStocks / ReserveStock / ReleaseStock /
// ConfirmStock / AdjustStock / Report）。
//
// 职责边界：
//   - 每方法入口 tracex.Start(ctx, "service", "stock", "<Method>") 分层埋点
//   - 身份校验在 handler 内读 req.GetOperatorId()（kitex 无 metadata 透传约定）
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
	stockpb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/stock"
	"github.com/gospacex/launchx/examples/mall/pkg/tracex"
	"github.com/gospacex/launchx/examples/mall/srv-stock/internal/middleware"
	"github.com/gospacex/launchx/examples/mall/srv-stock/internal/service"
)

// Server 实现生成的 stock.Stock 接口
type Server struct {
	Svc     *service.StockService
	Limiter *middleware.RateLimiter
}

// guard 方法入口公共逻辑：分层 span（调用方必须 defer span.End()）+ 限流。
//
// span 必须由调用方 End：丢弃句柄会让 span 永不导出，且下游子 span 挂在
// 悬空 parent 上（Jaeger 上 missing-parent）。
//
// 限流主体按商品维度取（库存域的热点天然是"某个爆款商品"，按用户分桶
// 反而拦不住集中打一个商品的流量）。
func (s *Server) guard(ctx context.Context, method string, subject int64) (context.Context, trace.Span, bool) {
	ctx, span := tracex.Start(ctx, "service", "stock", method)
	if s.Limiter != nil && !s.Limiter.Allow(ctx, subject) {
		return ctx, span, false
	}
	return ctx, span, true
}

// GetStock 查库存快照
func (s *Server) GetStock(ctx context.Context, req *stockpb.GetStockReq) (*stockpb.GetStockResp, error) {
	resp := &stockpb.GetStockResp{ProductId: req.GetProductId()}
	ctx, span, ok := s.guard(ctx, "GetStock", req.GetProductId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.GetStock(ctx, req)
}

// ListStocks 单表分页
func (s *Server) ListStocks(ctx context.Context, req *stockpb.ListStocksReq) (*stockpb.ListStocksResp, error) {
	resp := &stockpb.ListStocksResp{}
	ctx, span, ok := s.guard(ctx, "ListStocks", 0)
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.ListStocks(ctx, req)
}

// ReserveStock 预占（加锁 + 3 表事务 + outbox）
func (s *Server) ReserveStock(ctx context.Context, req *stockpb.ReserveStockReq) (*stockpb.ReserveStockResp, error) {
	resp := &stockpb.ReserveStockResp{ProductId: req.GetProductId()}
	ctx, span, ok := s.guard(ctx, "ReserveStock", req.GetProductId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.ReserveStock(ctx, req)
}

// ReleaseStock 释放预占
func (s *Server) ReleaseStock(ctx context.Context, req *stockpb.ReleaseStockReq) (*stockpb.ReleaseStockResp, error) {
	resp := &stockpb.ReleaseStockResp{ProductId: req.GetProductId()}
	ctx, span, ok := s.guard(ctx, "ReleaseStock", req.GetProductId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.ReleaseStock(ctx, req)
}

// ConfirmStock 确认预占（出库）
func (s *Server) ConfirmStock(ctx context.Context, req *stockpb.ConfirmStockReq) (*stockpb.ConfirmStockResp, error) {
	resp := &stockpb.ConfirmStockResp{ProductId: req.GetProductId()}
	ctx, span, ok := s.guard(ctx, "ConfirmStock", req.GetProductId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.ConfirmStock(ctx, req)
}

// AdjustStock 入库 / 盘点
func (s *Server) AdjustStock(ctx context.Context, req *stockpb.AdjustStockReq) (*stockpb.AdjustStockResp, error) {
	resp := &stockpb.AdjustStockResp{ProductId: req.GetProductId()}
	ctx, span, ok := s.guard(ctx, "AdjustStock", req.GetProductId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.AdjustStock(ctx, req)
}

// Report 3 表 JOIN 报表
func (s *Server) Report(ctx context.Context, req *stockpb.ReportReq) (*stockpb.ReportResp, error) {
	resp := &stockpb.ReportResp{}
	ctx, span, ok := s.guard(ctx, "Report", 0)
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.Report(ctx, req)
}

// 编译期断言：Server 必须满足生成的 stock.Stock 接口
var _ stockpb.Stock = (*Server)(nil)

// rateLimited 统一限流响应码（pkg/codes 1005）
func rateLimited() (code int64, msg string) {
	c := codes.Fail(codes.CodeRateLimited, "rate limited")
	return c.Code, c.Message
}
