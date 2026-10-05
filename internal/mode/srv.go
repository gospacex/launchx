package mode

import (
	"strings"

	"github.com/gospacex/rpcx"
)

// assembleSRV 领域服务档：只有 RPC 服务端，不建 webx。
//
// kitex server 本体由业务 Setup 用 ServerOptions 构造（业务代码与形态无关，
// 换形态只换这里的装配），句柄回存 App.RPCServer，由 Run 启动、Close 停止。
func assembleSRV(h *AppHolders) error {
	h.Out.RPCServer = rpcx.BuildServerOptions(ServerOptions(h))
	h.Out.Registered = true
	return nil
}

// ServerOptions 产出 srv 档的 kitex 服务端装配参数。
//
// 注册地址非空时挂 coordx 注册器（经 rpcx 适配）；为空则不注册，
// 服务「起了但不被发现」——由 strict/部署清单兜底，不在此静默补默认地址。
func ServerOptions(h *AppHolders) rpcx.ServerOptions {
	return rpcx.ServerOptions{
		ServiceName:  h.ServiceName,
		Addr:         strings.TrimSpace(h.Cfg.GetString("service.addr")),
		RegistryAddr: h.Out.RegistryAddr,
		TraceEnabled: h.Cfg.GetBool("trace.enabled"),
	}
}
