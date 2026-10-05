package mode

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gospacex/logx"
	"github.com/gospacex/rpcx"
	"github.com/gospacex/webx"
)

// assembleMonolith 单体档：仅 HTTP 服务端，不注册自身。
func assembleMonolith(h *AppHolders) error {
	engine, err := newWebx(h)
	if err != nil {
		return err
	}
	h.Out.Webx = engine
	return nil
}

// assembleBFF BFF 档：HTTP 入口 + 下游 RPC 客户端，并注册自身供网关发现。
func assembleBFF(h *AppHolders) error {
	engine, err := newWebx(h)
	if err != nil {
		return err
	}
	h.Out.Webx = engine
	h.Out.Registered = true
	h.Out.RPCClientOptions = clientOptionsFactory(h)
	return nil
}

// assembleGateway 网关档：HTTP 入口 + 上游服务发现，自身不注册。
//
// 上游发现与 bff 走同一条路：rpcx 的 coordx resolver（按被调服务名解析），
// 网关与 BFF 的差别只在「是否把自己注册进注册中心」。
func assembleGateway(h *AppHolders) error {
	engine, err := newWebx(h)
	if err != nil {
		return err
	}
	h.Out.Webx = engine
	h.Out.RPCClientOptions = clientOptionsFactory(h)
	return nil
}

// newWebx 装配 webx 引擎。
//
// 只装配、不监听：监听由 Run 触发。Logger/Tracer 为空时回落到各 SDK 的全局
// 缺省（logx.GetGlobal / otel 全局 provider），装配期不因此失败。
func newWebx(h *AppHolders) (any, error) {
	// presence-based：没配 webx 段就用零值配置（FillDefaults 补齐地址/前缀等），
	// 而不是让 ReadConfigFromConfigx 报 "key webx not found" 打断启动。
	cfg := webx.Config{}
	if h.Cfg.IsSet("webx") {
		parsed, err := webx.ReadConfigFromConfigx(h.Cfg)
		if err != nil {
			return nil, fmt.Errorf("launchx: webx config: %w", err)
		}
		cfg = parsed
	}
	if strings.TrimSpace(cfg.Addr) == "" {
		// 未配 webx.addr 时回落到 service.addr：配置里通常只声明一处监听地址，
		// 两处不一致会让"改了配置没生效"变成静默故障。
		cfg.Addr = strings.TrimSpace(h.Cfg.GetString("service.addr"))
	}
	opts := []webx.Option{webx.WithConfig(cfg)}
	if h.Logger != nil {
		opts = append(opts, webx.WithLogger(h.Logger))
	}
	if h.Tracer != nil {
		opts = append(opts, webx.WithTracer(h.Tracer.TracerProvider().Tracer(tracerName(h))))
	}
	for _, fn := range h.PreRun {
		opts = append(opts, webx.WithPreRun(func(a *webx.App) { fn(a) }))
	}
	app, err := webx.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("launchx: webx new: %w", err)
	}
	return app, nil
}

// tracerName 埋点名：优先服务名，缺省回落 webx。
func tracerName(h *AppHolders) string {
	if h.ServiceName != "" {
		return h.ServiceName
	}
	return "webx"
}

// clientOptionsFactory 产出下游 RPC 客户端选项工厂。
//
// 按被调服务名产出 []kitex client.Option（以 any 承载，调用方按需断言）：
// 注册中心地址非空时经 rpcx 的 coordx resolver 做发现；为空时不挂 resolver，
// 只用 service.upstream.<service> 作为直连兜底目标（无则不带目标选项）。
func clientOptionsFactory(h *AppHolders) func(service string) (any, error) {
	return func(service string) (any, error) {
		if strings.TrimSpace(service) == "" {
			return nil, errors.New("launchx: rpc client service name is empty")
		}
		addr := h.Out.RegistryAddr
		if addr == "" && h.Logger != nil {
			// 未配注册中心：不挂 resolver，由调用方自行保证目标可达
			//（刻意不静默补默认地址，见 registryAddr 注释）。
			h.Logger.Warn("launchx: no registry configured, rpc client has no discovery",
				logx.String("service", service))
		}
		return rpcx.BuildClientOptions(rpcx.ClientOptions{
			ServiceName:  h.ServiceName,
			RegistryAddr: addr,
			Target:       h.Cfg.GetString("service.upstream." + service),
			TraceEnabled: h.Cfg.GetBool("trace.enabled"),
		}), nil
	}
}
