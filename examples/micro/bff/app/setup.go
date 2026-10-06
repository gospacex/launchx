// Package app 业务装配层：把 launchx 装配好的句柄（webx 引擎、下游 RPC 客户端
// 选项）接成 handler → downstream 两层，并注册路由。
//
// 这一层不含任何形态分支（不出现 monolith / bff / gateway 等取值）：
// 换形态只改 config.yaml 的 service.mode，与本装配无关。
package app

import (
	"errors"
	"fmt"

	"github.com/cloudwego/kitex/client"
	"github.com/gospacex/webx"
	"go.opentelemetry.io/otel"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/micro/bff/internal/downstream"
	"github.com/gospacex/launchx/examples/micro/bff/internal/handler"
	orderrpc "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/order/order"
	pointsrpc "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/points/points"
	productrpc "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/product/product"
	stockrpc "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/stock/stock"
)

// downstreamServices 四个下游域服：既是 App.RPCClientOptions 的入参
// （须与各 srv 注册进 consul 的名字一致），也是 config.yaml 里
// service.upstream.<name> 直连兜底键的后缀。
//
// 顺序即全链路的调用次序：商品 → 库存 → 订单 → 积分（下单编排依此串联）。
var downstreamServices = []string{"demo-product", "demo-stock", "demo-order", "demo-points"}

// Setup 是注册给 App.OnSetup 的装配回调：
// 取 webx 引擎 + 四个下游 RPC 客户端选项 → 构造下游客户端 → 挂路由。
//
// 失败即返回 error：装配期报错好过带着半装配的状态把服务起出去。
//
// 本函数不注册/注销 consul —— bff 档的注册由 launchx 按 mode 自己做
// （地址取 service.self.addr → service.addr），micro 侧只管业务装配。
func Setup(a *launchx.App) error {
	// webx 引擎由 bff 档装配产出，路由须在 Run 之前挂完。
	engine, ok := a.Engine().(*webx.App)
	if !ok || engine == nil {
		return errors.New("bff: webx engine is unavailable in this mode")
	}

	// 下游客户端选项只能经 App.RPCClientOptions 取（发现经 rpcx 的 coordx
	// resolver，地址来自 xmiddleware.coordx 段），业务不自建注册中心客户端，
	// 也不拼任何目标地址。实测返回类型是 []client.Option（以 any 承载）。
	newClient := func(name string, build func(opts []client.Option) (any, error)) (any, error) {
		raw, err := a.RPCClientOptions(name)
		if err != nil {
			return nil, fmt.Errorf("bff: rpc client options for %s: %w", name, err)
		}
		opts, ok := raw.([]client.Option)
		if !ok {
			return nil, fmt.Errorf("bff: rpc client options for %s are unavailable, check service.mode=bff in config.yaml", name)
		}
		return build(opts)
	}

	productCliAny, err := newClient("demo-product", func(o []client.Option) (any, error) {
		return productrpc.NewClient("demo-product", o...)
	})
	if err != nil {
		return err
	}
	productCli, ok := productCliAny.(productrpc.Client)
	if !ok {
		return errors.New("bff: product client type assertion failed")
	}

	stockCliAny, err := newClient("demo-stock", func(o []client.Option) (any, error) {
		return stockrpc.NewClient("demo-stock", o...)
	})
	if err != nil {
		return err
	}
	stockCli, ok := stockCliAny.(stockrpc.Client)
	if !ok {
		return errors.New("bff: stock client type assertion failed")
	}

	orderCliAny, err := newClient("demo-order", func(o []client.Option) (any, error) {
		return orderrpc.NewClient("demo-order", o...)
	})
	if err != nil {
		return err
	}
	orderCli, ok := orderCliAny.(orderrpc.Client)
	if !ok {
		return errors.New("bff: order client type assertion failed")
	}

	pointsCliAny, err := newClient("demo-points", func(o []client.Option) (any, error) {
		return pointsrpc.NewClient("demo-points", o...)
	})
	if err != nil {
		return err
	}
	pointsCli, ok := pointsCliAny.(pointsrpc.Client)
	if !ok {
		return errors.New("bff: points client type assertion failed")
	}

	// tracer 同样经装配层交接：bff 的 provider 产三类 span —— handler.*（HTTP 入口）、
	// downstream.*（本档业务层）与 CLIENT span（rpcx 的 ClientSpanMiddleware 产出，
	// 选项里已挂，见 rpcx/options.go）。后两者必须同出一个 provider，否则 CLIENT
	// span 掉进全局 noop，Jaeger 上只见 bff 收了请求、见不到四个 srv 各自的跳。
	// 未启用 telemetry 时 App.Tracer() 为 nil，回落到 otel 全局（no-op）：
	// 埋点缺失可以降级，不能让聚合层起不来。
	tp := otel.GetTracerProvider()
	if tr := a.Tracer(); tr != nil {
		tp = tr.TracerProvider()
	}

	h := handler.New(
		downstream.New(downstream.Clients{
			Product: productCli,
			Stock:   stockCli,
			Order:   orderCli,
			Points:  pointsCli,
		}, tp.Tracer("downstream")),
		tp.Tracer("bff"),
	)

	engine.Engine.GET("/ping", h.Ping)
	engine.Engine.GET("/product/:id", h.GetProduct)
	engine.Engine.GET("/stock/:product_id", h.GetStock)
	engine.Engine.GET("/order/:id", h.GetOrder)
	engine.Engine.GET("/points/:user_id", h.GetAccount)
	// 复合编排：一次请求串起四个域服 —— 全链路验收就看这一条。
	engine.Engine.POST("/order", h.PlaceOrder)
	return nil
}
