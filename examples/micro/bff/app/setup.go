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

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/micro/bff/internal/downstream"
	"github.com/gospacex/launchx/examples/micro/bff/internal/handler"
	orderrpc "github.com/gospacex/launchx/examples/micro/srv/pkg/kitex_gen/order/order"
)

// downstreamService 下游订单域名：既是 App.RPCClientOptions 的入参
// （须与 demo-srv 注册进 consul 的名字一致），也是 config.yaml 里
// service.upstream.<name> 直连兜底键的后缀。
const downstreamService = "demo-srv"

// Setup 是注册给 App.OnSetup 的装配回调：
// 取 webx 引擎 + 下游 RPC 客户端选项 → 构造下游客户端 → 挂路由。
//
// 失败即返回 error：装配期报错好过带着半装配的状态把服务起出去。
//
// 本函数不注册/注销 consul —— bff 档的注册由 launchx 按 mode 自己做
// （地址取 service.self.addr → service.addr），demo 侧只管业务装配。
func Setup(a *launchx.App) error {
	// webx 引擎由 bff 档装配产出，路由须在 Run 之前挂完。
	engine, ok := a.Engine().(*webx.App)
	if !ok || engine == nil {
		return errors.New("bff: webx engine is unavailable in this mode")
	}

	// 下游客户端选项只能经 App.RPCClientOptions 取（发现经 rpcx 的 coordx
	// resolver，地址来自 xmiddleware.coordx 段），业务不自建注册中心客户端，
	// 也不拼任何目标地址。实测返回类型是 []client.Option（以 any 承载）。
	raw, err := a.RPCClientOptions(downstreamService)
	if err != nil {
		return err
	}
	opts, ok := raw.([]client.Option)
	if !ok {
		return errors.New("bff: rpc client options are unavailable, check service.mode=bff in config.yaml")
	}
	cli, err := orderrpc.NewClient(downstreamService, opts...)
	if err != nil {
		return fmt.Errorf("bff: new order client: %w", err)
	}

	tp := a.Tracer().TracerProvider()
	// /ping 回显的形态标识取自在档形态：既不写死字面量，业务分层里也不会
	// 因此出现任何 mode 分支。
	h := handler.New(downstream.NewOrder(cli, tp.Tracer("downstream")), tp.Tracer("bff"), string(a.Mode()))

	engine.Engine.GET("/ping", h.Ping)
	engine.Engine.GET("/order/:id", h.GetOrder)
	return nil
}
