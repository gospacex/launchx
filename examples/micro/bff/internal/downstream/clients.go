// Package downstream 下游调用层：持有四个域服的 RPC 客户端，把业务参数
// 翻译成 RPC 请求，并把传输/业务错误标记进 span。
//
// 客户端只能由 launchx 产出的 kitex 选项构造（装配见 app.Setup）：本包不碰
// 任何地址，也不自建注册中心客户端。
//
// 每个 srv 方法开一个 downstream.<域>.<方法> span：上承 handler.*、下接下游
// RPC 的 CLIENT span（rpcx 的 ClientSpanMiddleware 产出）。刻意不再给 CLIENT
// span 外面包第三层 —— 包了三层父子链就退化成两段，链路反而看不清。
package downstream

import (
	"context"
	"fmt"

	otelcodes "go.opentelemetry.io/otel/codes" // 传输层错误标记（与业务码包 codes 区分）
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	orderpb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/order"
	orderrpc "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/order/order"
	pointspb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/points"
	pointsrpc "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/points/points"
	productpb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/product"
	productrpc "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/product/product"
	stockpb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/stock"
	stockrpc "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/stock/stock"
)

// Clients 四域客户端的集合（由 app.Setup 用 launchx 的客户端选项构造）。
type Clients struct {
	Product productrpc.Client
	Stock   stockrpc.Client
	Order   orderrpc.Client
	Points  pointsrpc.Client
}

// Downstream 四域下游门面：全部方法都带 span，业务码非 0 记为 span Error。
type Downstream struct {
	c      Clients
	tracer trace.Tracer
}

// New 构造四域门面。
func New(c Clients, tracer trace.Tracer) *Downstream {
	return &Downstream{c: c, tracer: tracer}
}

// span 便捷入口：起 span + 生命周期钩子。
func (d *Downstream) span(ctx context.Context, name string) (context.Context, trace.Span) {
	return d.tracer.Start(ctx, name)
}

// markBiz 领域错误不是传输故障：不下发 error，但要在本层留下可检索的失败标记，
// 否则 Jaeger 上「调用成功但没查到」会看不出和成功请求的区别。
func markBiz(span trace.Span, code int64, msg string) {
	if code != codes.CodeOK {
		span.SetStatus(otelcodes.Error, fmt.Sprintf("code=%d msg=%s", code, msg))
	}
}

// GetProduct 商品域：取单个商品（商品域内部含缓存与多表 JOIN）。
func (d *Downstream) GetProduct(ctx context.Context, req *productpb.GetProductReq) (*productpb.GetProductResp, error) {
	ctx, span := d.span(ctx, "downstream.product.GetProduct")
	defer span.End()
	r, err := d.c.Product.GetProduct(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, err
	}
	markBiz(span, r.GetCode(), r.GetMessage())
	return r, nil
}

// GetStock 库存域：查某商品的库存快照。
func (d *Downstream) GetStock(ctx context.Context, req *stockpb.GetStockReq) (*stockpb.GetStockResp, error) {
	ctx, span := d.span(ctx, "downstream.stock.GetStock")
	defer span.End()
	r, err := d.c.Stock.GetStock(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, err
	}
	markBiz(span, r.GetCode(), r.GetMessage())
	return r, nil
}

// ReserveStock 库存域：预占库存（锁 + 3 表事务，失败可重试的领域码在 r.Code）。
func (d *Downstream) ReserveStock(ctx context.Context, req *stockpb.ReserveStockReq) (*stockpb.ReserveStockResp, error) {
	ctx, span := d.span(ctx, "downstream.stock.ReserveStock")
	defer span.End()
	r, err := d.c.Stock.ReserveStock(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, err
	}
	markBiz(span, r.GetCode(), r.GetMessage())
	return r, nil
}

// ReleaseStock 库存域：释放预占（补偿路径 —— 下单失败必须把它调上）。
func (d *Downstream) ReleaseStock(ctx context.Context, req *stockpb.ReleaseStockReq) (*stockpb.ReleaseStockResp, error) {
	ctx, span := d.span(ctx, "downstream.stock.ReleaseStock")
	defer span.End()
	r, err := d.c.Stock.ReleaseStock(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, err
	}
	markBiz(span, r.GetCode(), r.GetMessage())
	return r, nil
}

// CreateOrder 订单域：创建订单（多表事务 + outbox 事件）。
func (d *Downstream) CreateOrder(ctx context.Context, req *orderpb.CreateOrderReq) (*orderpb.CreateOrderResp, error) {
	ctx, span := d.span(ctx, "downstream.order.CreateOrder")
	defer span.End()
	r, err := d.c.Order.CreateOrder(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, err
	}
	markBiz(span, r.GetCode(), r.GetMessage())
	return r, nil
}

// GetOrder 订单域：查单个订单 + 明细。
func (d *Downstream) GetOrder(ctx context.Context, req *orderpb.GetOrderReq) (*orderpb.GetOrderResp, error) {
	ctx, span := d.span(ctx, "downstream.order.GetOrder")
	defer span.End()
	r, err := d.c.Order.GetOrder(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, err
	}
	markBiz(span, r.GetCode(), r.GetMessage())
	return r, nil
}

// GetAccount 积分域：查某用户的积分账户。
func (d *Downstream) GetAccount(ctx context.Context, req *pointspb.GetAccountReq) (*pointspb.GetAccountResp, error) {
	ctx, span := d.span(ctx, "downstream.points.GetAccount")
	defer span.End()
	r, err := d.c.Points.GetAccount(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, err
	}
	markBiz(span, r.GetCode(), r.GetMessage())
	return r, nil
}

// Earn 积分域：入账（幂等键 = order_no + type，重放安全）。
func (d *Downstream) Earn(ctx context.Context, req *pointspb.EarnReq) (*pointspb.EarnResp, error) {
	ctx, span := d.span(ctx, "downstream.points.Earn")
	defer span.End()
	r, err := d.c.Points.Earn(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, err
	}
	markBiz(span, r.GetCode(), r.GetMessage())
	return r, nil
}
