// Package rpc srv-product 的 kitex 传输适配层。
//
// 在分层链上的位置：**rpc** → route → middleware → handler → service →
// repository → model。它是链的最外一节，也是唯一认识 kitex 的一层。
//
// 职责边界（写窄，别让业务渗进来）：
//   - 开 rpc.product.<Method> span，把 kitex 带进来的 ctx 接进链路
//   - 经 route.Router.Enter 走路由与中间件链，被拒时回限流码
//   - 把请求原样交给 handler（不做任何字段加工）
//
// 本层不碰监听地址：server 选项由 launchx 的 srv 档装配产出（地址取
// config.yaml 的 service.self.addr）。
package rpc

import (
	"context"

	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	productpb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/product"
	"github.com/gospacex/launchx/examples/micro/pkg/routing"
	"github.com/gospacex/launchx/examples/micro/pkg/tracex"
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/handler"
)

// Server 实现生成的 product.Product 接口
type Server struct {
	Handler *handler.ProductHandler
	Router  *routing.Router
}

// CreateProduct 建商品（多表事务 + outbox 事件）
func (s *Server) CreateProduct(ctx context.Context, req *productpb.CreateProductReq) (*productpb.CreateProductResp, error) {
	resp := &productpb.CreateProductResp{}
	ctx, span := tracex.Start(ctx, "rpc", "product", "CreateProduct")
	defer span.End()
	v := s.Router.Enter(ctx, "CreateProduct", subjectOf(req.GetOperatorId(), 0))
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.CreateProduct(v.Ctx, req)
}

// GetProduct 查商品详情（缓存优先 + 热度计数）
func (s *Server) GetProduct(ctx context.Context, req *productpb.GetProductReq) (*productpb.GetProductResp, error) {
	resp := &productpb.GetProductResp{UserId: req.GetUserId()}
	ctx, span := tracex.Start(ctx, "rpc", "product", "GetProduct")
	defer span.End()
	v := s.Router.Enter(ctx, "GetProduct", subjectOf(req.GetUserId(), req.GetProductId()))
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.GetProduct(v.Ctx, req)
}

// UpdateProduct 改商品（2 表事务 + 缓存失效）
func (s *Server) UpdateProduct(ctx context.Context, req *productpb.UpdateProductReq) (*productpb.UpdateProductResp, error) {
	resp := &productpb.UpdateProductResp{ProductId: req.GetProductId()}
	ctx, span := tracex.Start(ctx, "rpc", "product", "UpdateProduct")
	defer span.End()
	v := s.Router.Enter(ctx, "UpdateProduct", subjectOf(req.GetOperatorId(), 0))
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.UpdateProduct(v.Ctx, req)
}

// UpdateStatus 上下架（商品维度加锁 + 3 表事务）
func (s *Server) UpdateStatus(ctx context.Context, req *productpb.UpdateStatusReq) (*productpb.UpdateStatusResp, error) {
	resp := &productpb.UpdateStatusResp{ProductId: req.GetProductId(), Status: req.GetStatus()}
	ctx, span := tracex.Start(ctx, "rpc", "product", "UpdateStatus")
	defer span.End()
	v := s.Router.Enter(ctx, "UpdateStatus", subjectOf(req.GetOperatorId(), 0))
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.UpdateStatus(v.Ctx, req)
}

// ListProducts 商品列表（2 表联查）
func (s *Server) ListProducts(ctx context.Context, req *productpb.ListProductsReq) (*productpb.ListProductsResp, error) {
	resp := &productpb.ListProductsResp{UserId: req.GetUserId()}
	ctx, span := tracex.Start(ctx, "rpc", "product", "ListProducts")
	defer span.End()
	v := s.Router.Enter(ctx, "ListProducts", subjectOf(req.GetUserId(), 0))
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.ListProducts(v.Ctx, req)
}

// Report 4 表 JOIN 报表
func (s *Server) Report(ctx context.Context, req *productpb.ReportReq) (*productpb.ReportResp, error) {
	resp := &productpb.ReportResp{UserId: req.GetUserId()}
	ctx, span := tracex.Start(ctx, "rpc", "product", "Report")
	defer span.End()
	v := s.Router.Enter(ctx, "Report", subjectOf(req.GetUserId(), 0))
	defer v.End()
	if !v.Allowed {
		rateLimited(&resp.Code, &resp.Message)
		return resp, nil
	}
	return s.Handler.Report(v.Ctx, req)
}

// 编译期断言：Server 必须满足生成的 product.Product 接口
var _ productpb.Product = (*Server)(nil)

// rateLimited 统一限流响应码（pkg/codes 1005）
func rateLimited(code *int64, message *string) {
	c := codes.Fail(codes.CodeRateLimited, "rate limited")
	*code, *message = c.Code, c.Message
}

// subjectOf 选限流主体：优先 primary，缺失时回落到 fallback。
//
// 回落到 0 也没关系：限流器按 subject 分桶，0 号桶只会被"没有身份的请求"
// 共享，写操作一律带 operator_id，不会落进这个桶。
func subjectOf(primary, fallback int64) int64 {
	if primary > 0 {
		return primary
	}
	return fallback
}
