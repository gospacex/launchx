// Package downstream 下游调用层：持有 demo-srv 的订单 RPC 客户端，把业务
// 参数翻译成 RPC 请求，并把传输错误标记进 span。
//
// 客户端只能由 launchx 产出的 kitex 选项构造（装配见 app.Setup）：本包不碰
// 任何地址，也不自建注册中心客户端。
package downstream

import (
	"context"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	srvcodes "github.com/gospacex/launchx/examples/micro/srv/pkg/codes"
	orderpb "github.com/gospacex/launchx/examples/micro/srv/pkg/kitex_gen/order"
	orderrpc "github.com/gospacex/launchx/examples/micro/srv/pkg/kitex_gen/order/order"
)

// Order 订单域下游客户端。
type Order struct {
	cli    orderrpc.Client
	tracer trace.Tracer
}

// NewOrder 构造订单域下游客户端。cli 由 app.Setup 用 launchx 的客户端选项构造。
func NewOrder(cli orderrpc.Client, tracer trace.Tracer) *Order {
	return &Order{cli: cli, tracer: tracer}
}

// GetOrder 向下游订单服务取单个订单。
//
// 本方法开的 span 是 bff 的业务层：上承 handler.* 、下接下游 RPC 的 client
// span（由 rpcx 的 ClientSpanMiddleware 产出，故这里刻意不再包一层 span 覆盖
// 它 —— 包了三段父子链就退化成两段）。下游返回的领域错误码不做翻译，
// 原样交给调用方决定 HTTP 状态码。
func (o *Order) GetOrder(ctx context.Context, req *orderpb.GetOrderReq) (*orderpb.GetOrderResp, error) {
	ctx, span := o.tracer.Start(ctx, "downstream.Order.GetOrder")
	defer span.End()

	resp, err := o.cli.GetOrder(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	// 领域错误不是传输故障：不下发 error，但要在本层留下可检索的失败标记，
	// 否则 Jaeger 上「调用成功但没查到」会看不出和成功请求的区别。
	if code := resp.GetCode(); code != srvcodes.CodeOK {
		span.SetStatus(codes.Error, resp.GetMessage())
	}
	return resp, nil
}
