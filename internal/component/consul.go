package component

import (
	"context"

	"github.com/gospacex/coordx"
	_ "github.com/gospacex/coordx/consul" // 注册 consul 驱动（blank import）
	"github.com/gospacex/logx"
)

func init() { Register("registry", buildRegistry) }

// registrySectionKeys 兼容两种段键名：launchx 用 xmiddleware.coordx，
// 历史配置里也见过 xmiddleware.consul。
var registrySectionKeys = []string{"xmiddleware.coordx", "xmiddleware.consul"}

// buildRegistry 构造服务注册（yaml 段：xmiddleware.coordx）。
//
// 失败默认降级：进程带病运行（服务不被发现 → 上游 502），而不是启动即退；
// strict.registry=true 时才即退。
func buildRegistry(env *Env) error {
	key := pickSection(env.Cfg, registrySectionKeys...)
	if key == "" {
		return nil
	}
	cfg := &coordx.Config{
		Driver:      env.Cfg.GetString(key + ".driver"),
		Name:        env.Cfg.GetString(key + ".name"),
		Addr:        env.Cfg.GetString(key + ".addr"),
		Cluster:     env.Cfg.GetString(key + ".cluster"),
		Secret:      env.Cfg.GetString(key + ".secret"),
		TLSEnabled:  env.Cfg.GetBool(key + ".tls_enabled"),
		EnableTrace: env.Cfg.GetBool(key+".enable_trace") && traceOn(env, "registry"),
	}
	if cfg.Driver == "" {
		cfg.Driver = "consul"
	}
	if cfg.Name == "" {
		cfg.Name = env.ServiceName
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:8500"
	}
	reg, err := coordx.NewRegister(cfg)
	if err != nil {
		return degrade(env, "registry", ConsequenceRegistry, err)
	}
	env.Registry = reg
	env.AddStop("registry", func() error {
		reg.Close()
		return nil
	})

	// 发现句柄：与注册同源配置。**这一步不能省** —— 网关（HTTP 反代）要按
	// 服务名解析上游实例，靠的就是它；没有它，业务只能退化成配置里写死地址。
	// 建不出来只记录告警，不按 strict 裁决：注册能力仍在，发现侧退化为兜底地址。
	disc, derr := coordx.NewDiscover(cfg)
	if derr != nil {
		env.Logger.WarnContext(context.Background(),
			"launchx: discover init failed, upstream discovery falls back to static address",
			logx.Err(derr))
		return nil
	}
	env.Discover = disc
	env.AddStop("registry.discover", func() error {
		disc.Close()
		return nil
	})
	return nil
}
