// Package rpc srv-product 的 kitex 传输适配层。
//
// 在分层链上的位置：**rpc** → route → middleware → handler → service →
// repository → model。它是链的最外一节，也是唯一认识 kitex 的一层。
//
// 职责边界（写窄，别让业务渗进来）：
//   - 开 rpc.product.<Method> span，把 kitex 带进来的 ctx 接进链路
//   - 经 route.Router.Enter 走路由与中间件链，被拒时回**对应的**拒绝码（鉴权 1004 / 限流 1005，不混成一个）
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
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/handler"
	"github.com/gospacex/otelx/layer"
)

// Server 实现生成的 product.Product 接口
type Server struct {
	Handler *handler.ProductHandler
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
	return layer.Begin(ctx, "rpc", "product", method, s.TraceOn, nil)
}

// CreateProduct 建商品（多表事务 + outbox 事件）
func (s *Server) CreateProduct(ctx context.Context, req *productpb.CreateProductReq) (*productpb.CreateProductResp, error) {
	resp := &productpb.CreateProductResp{}
	ctx, end := s.begin(ctx, "CreateProduct")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "CreateProduct", subjectOf(req.GetOperatorId(), 0))
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.CreateProduct(v.Ctx, req)
	return resp, err
}

// GetProduct 查商品详情（缓存优先 + 热度计数）
func (s *Server) GetProduct(ctx context.Context, req *productpb.GetProductReq) (*productpb.GetProductResp, error) {
	resp := &productpb.GetProductResp{UserId: req.GetUserId()}
	ctx, end := s.begin(ctx, "GetProduct")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "GetProduct", subjectOf(req.GetUserId(), req.GetProductId()))
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.GetProduct(v.Ctx, req)
	return resp, err
}

// UpdateProduct 改商品（2 表事务 + 缓存失效）
func (s *Server) UpdateProduct(ctx context.Context, req *productpb.UpdateProductReq) (*productpb.UpdateProductResp, error) {
	resp := &productpb.UpdateProductResp{ProductId: req.GetProductId()}
	ctx, end := s.begin(ctx, "UpdateProduct")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "UpdateProduct", subjectOf(req.GetOperatorId(), 0))
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.UpdateProduct(v.Ctx, req)
	return resp, err
}

// UpdateStatus 上下架（商品维度加锁 + 3 表事务）
func (s *Server) UpdateStatus(ctx context.Context, req *productpb.UpdateStatusReq) (*productpb.UpdateStatusResp, error) {
	resp := &productpb.UpdateStatusResp{ProductId: req.GetProductId(), Status: req.GetStatus()}
	ctx, end := s.begin(ctx, "UpdateStatus")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "UpdateStatus", subjectOf(req.GetOperatorId(), 0))
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.UpdateStatus(v.Ctx, req)
	return resp, err
}

// ListProducts 商品列表（2 表联查）
func (s *Server) ListProducts(ctx context.Context, req *productpb.ListProductsReq) (*productpb.ListProductsResp, error) {
	resp := &productpb.ListProductsResp{UserId: req.GetUserId()}
	ctx, end := s.begin(ctx, "ListProducts")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "ListProducts", subjectOf(req.GetUserId(), 0))
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.ListProducts(v.Ctx, req)
	return resp, err
}

// Report 4 表 JOIN 报表
func (s *Server) Report(ctx context.Context, req *productpb.ReportReq) (*productpb.ReportResp, error) {
	resp := &productpb.ReportResp{UserId: req.GetUserId()}
	ctx, end := s.begin(ctx, "Report")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "Report", subjectOf(req.GetUserId(), 0))
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.Report(v.Ctx, req)
	return resp, err
}

// 编译期断言：Server 必须满足生成的 product.Product 接口
var _ productpb.Product = (*Server)(nil)

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
