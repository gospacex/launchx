// Package rpc srv-product 的 kitex 服务端桩：实现 pkg/kitex_gen 生成的
// product.Product 接口（CreateProduct / GetProduct / UpdateProduct /
// UpdateStatus / ListProducts / Report）。
//
// 职责边界：
//   - 每方法入口 tracex.Start(ctx, "service", "product", "<Method>") 分层埋点
//   - 身份校验在 handler 内读 req.GetOperatorId()（kitex 无 metadata 透传约定）
//   - 业务错误以 Response.Code/Message 表达（pkg/codes）；网络/内部错误走
//     Go error（kitex 错误体），两套不混用
//   - 限流由 middleware.RateLimiter 在入口完成（kvx 计数、fail-open）
//
// 本包只出 impl，不碰监听地址：server 选项由 launchx 的 srv 档装配产出
// （地址取 config.yaml 的 service.addr）。
package rpc

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/mall/pkg/codes"
	productpb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/product"
	"github.com/gospacex/launchx/examples/mall/pkg/tracex"
	"github.com/gospacex/launchx/examples/mall/srv-product/internal/middleware"
	"github.com/gospacex/launchx/examples/mall/srv-product/internal/service"
)

// Server 实现生成的 product.Product 接口
type Server struct {
	Svc     *service.ProductService
	Limiter *middleware.RateLimiter
}

// guard 方法入口公共逻辑：分层 span（调用方必须 defer span.End()）+ 限流。
//
// span 必须由调用方 End：写成 `ctx, _ = tracex.Start(...)` 会丢句柄，
// span 永不导出，且下游子 span 挂在悬空 parent 上（Jaeger 上 missing-parent）。
//
// 限流主体按方法语义取：写操作按 operator（防止一个后台账号刷爆），
// 读操作按 user（防止单用户刷详情）。两者都不该按"全局"计数 ——
// 那样一个异常来源会把所有人的请求一起拒掉。
func (s *Server) guard(ctx context.Context, method string, subject int64) (context.Context, trace.Span, bool) {
	ctx, span := tracex.Start(ctx, "service", "product", method)
	if s.Limiter != nil && !s.Limiter.Allow(ctx, subject) {
		return ctx, span, false
	}
	return ctx, span, true
}

// CreateProduct 建商品（3 表事务 + outbox 事件）
func (s *Server) CreateProduct(ctx context.Context, req *productpb.CreateProductReq) (*productpb.CreateProductResp, error) {
	resp := &productpb.CreateProductResp{}
	ctx, span, ok := s.guard(ctx, "CreateProduct", req.GetOperatorId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.CreateProduct(ctx, req)
}

// GetProduct 查商品详情（缓存优先 + 热度计数）
func (s *Server) GetProduct(ctx context.Context, req *productpb.GetProductReq) (*productpb.GetProductResp, error) {
	resp := &productpb.GetProductResp{UserId: req.GetUserId()}
	ctx, span, ok := s.guard(ctx, "GetProduct", subjectOf(req.GetUserId(), req.GetProductId()))
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.GetProduct(ctx, req)
}

// UpdateProduct 改商品（2 表事务 + 缓存失效）
func (s *Server) UpdateProduct(ctx context.Context, req *productpb.UpdateProductReq) (*productpb.UpdateProductResp, error) {
	resp := &productpb.UpdateProductResp{ProductId: req.GetProductId()}
	ctx, span, ok := s.guard(ctx, "UpdateProduct", req.GetOperatorId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.UpdateProduct(ctx, req)
}

// UpdateStatus 上下架（商品维度加锁 + 2 表事务）
func (s *Server) UpdateStatus(ctx context.Context, req *productpb.UpdateStatusReq) (*productpb.UpdateStatusResp, error) {
	resp := &productpb.UpdateStatusResp{ProductId: req.GetProductId(), Status: req.GetStatus()}
	ctx, span, ok := s.guard(ctx, "UpdateStatus", req.GetOperatorId())
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.UpdateStatus(ctx, req)
}

// ListProducts 单表分页列表
func (s *Server) ListProducts(ctx context.Context, req *productpb.ListProductsReq) (*productpb.ListProductsResp, error) {
	resp := &productpb.ListProductsResp{UserId: req.GetUserId()}
	ctx, span, ok := s.guard(ctx, "ListProducts", subjectOf(req.GetUserId(), 0))
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.ListProducts(ctx, req)
}

// Report 4 表 JOIN 报表
func (s *Server) Report(ctx context.Context, req *productpb.ReportReq) (*productpb.ReportResp, error) {
	resp := &productpb.ReportResp{UserId: req.GetUserId()}
	ctx, span, ok := s.guard(ctx, "Report", subjectOf(req.GetUserId(), 0))
	defer span.End()
	if !ok {
		resp.Code, resp.Message = rateLimited()
		return resp, nil
	}
	return s.Svc.Report(ctx, req)
}

// 编译期断言：Server 必须满足生成的 product.Product 接口
var _ productpb.Product = (*Server)(nil)

// rateLimited 统一限流响应码（pkg/codes 1005）
func rateLimited() (code int64, msg string) {
	c := codes.Fail(codes.CodeRateLimited, "rate limited")
	return c.Code, c.Message
}

// subjectOf 选限流主体：优先 user_id，缺失时回落到 fallback（商品/类目维度）。
//
// 回落到 0 也没关系：限流器按 subject 分桶，0 号桶只会被"没有身份的读请求"
// 共享，写操作一律带 operator_id，不会落进这个桶。
func subjectOf(userID, fallback int64) int64 {
	if userID > 0 {
		return userID
	}
	return fallback
}
