// Package rpc srv-stock 的 kitex 传输适配层。
//
// 在分层链上的位置：**rpc** → route → middleware → handler → service →
// repository → model。它是链的最外一节，也是唯一认识 kitex 的一层。
//
// 职责边界（写窄，别让业务渗进来）：
//   - 开 rpc.stock.<Method> span，把 kitex 带进来的 ctx 接进链路
//   - 经 route.Router.Enter 走路由与中间件链，被拒时回**对应的**拒绝码（鉴权 1004 / 限流 1005，不混成一个）
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
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/handler"
	"github.com/gospacex/otelx/layer"
)

// Server 实现生成的 stock.Stock 接口
type Server struct {
	Handler *handler.StockHandler
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
	return layer.Begin(ctx, "rpc", "stock", method, s.TraceOn, nil)
}

// GetStock 查库存快照
func (s *Server) GetStock(ctx context.Context, req *stockpb.GetStockReq) (*stockpb.GetStockResp, error) {
	resp := &stockpb.GetStockResp{ProductId: req.GetProductId()}
	ctx, end := s.begin(ctx, "GetStock")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "GetStock", req.GetProductId())
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.GetStock(v.Ctx, req)
	return resp, err
}

// ListStocks 库存列表（联表聚合分页）
func (s *Server) ListStocks(ctx context.Context, req *stockpb.ListStocksReq) (*stockpb.ListStocksResp, error) {
	resp := &stockpb.ListStocksResp{}
	ctx, end := s.begin(ctx, "ListStocks")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "ListStocks", 0)
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.ListStocks(v.Ctx, req)
	return resp, err
}

// ReserveStock 预占（加锁 + 3 表事务 + outbox）
func (s *Server) ReserveStock(ctx context.Context, req *stockpb.ReserveStockReq) (*stockpb.ReserveStockResp, error) {
	resp := &stockpb.ReserveStockResp{ProductId: req.GetProductId()}
	ctx, end := s.begin(ctx, "ReserveStock")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "ReserveStock", req.GetProductId())
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.ReserveStock(v.Ctx, req)
	return resp, err
}

// ReleaseStock 释放预占（幂等）
func (s *Server) ReleaseStock(ctx context.Context, req *stockpb.ReleaseStockReq) (*stockpb.ReleaseStockResp, error) {
	resp := &stockpb.ReleaseStockResp{ProductId: req.GetProductId()}
	ctx, end := s.begin(ctx, "ReleaseStock")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "ReleaseStock", req.GetProductId())
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.ReleaseStock(v.Ctx, req)
	return resp, err
}

// ConfirmStock 确认预占（出库）
func (s *Server) ConfirmStock(ctx context.Context, req *stockpb.ConfirmStockReq) (*stockpb.ConfirmStockResp, error) {
	resp := &stockpb.ConfirmStockResp{ProductId: req.GetProductId()}
	ctx, end := s.begin(ctx, "ConfirmStock")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "ConfirmStock", req.GetProductId())
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.ConfirmStock(v.Ctx, req)
	return resp, err
}

// AdjustStock 入库 / 盘点
func (s *Server) AdjustStock(ctx context.Context, req *stockpb.AdjustStockReq) (*stockpb.AdjustStockResp, error) {
	resp := &stockpb.AdjustStockResp{ProductId: req.GetProductId()}
	ctx, end := s.begin(ctx, "AdjustStock")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "AdjustStock", req.GetProductId())
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.AdjustStock(v.Ctx, req)
	return resp, err
}

// Report 3 表 JOIN 报表
func (s *Server) Report(ctx context.Context, req *stockpb.ReportReq) (*stockpb.ReportResp, error) {
	resp := &stockpb.ReportResp{}
	ctx, end := s.begin(ctx, "Report")
	var err error
	defer func() { end(err) }()
	v := s.Router.Enter(ctx, "Report", 0)
	defer v.End()
	if !v.Allowed {
		rejected(&resp.Code, &resp.Message, v.AbortedBy)
		return resp, nil
	}
	resp, err = s.Handler.Report(v.Ctx, req)
	return resp, err
}

// 编译期断言：Server 必须满足生成的 stock.Stock 接口
var _ stockpb.Stock = (*Server)(nil)

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
