package component

import (
	"github.com/gospacex/coordx"
	_ "github.com/gospacex/coordx/consul" // 注册 consul 驱动（blank import）
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
	//
	// 与注册侧**同口径**：失败走 degrade（即受 strict.registry 裁决），不再
	// 「只告警 + 静默继续」。理由（2026-10-08 复核）：注册被判严格、发现永远
	// 只告警，等于 strict.registry=true 也拦不住「配了注册中心但发现构件器建不
	// 出来」——症状是生产里只多一行 Warn，全部出向调用退化成静态地址，
	// 没有静态兜底时表现为 502，而排查方向还会被「注册是成功的」误导。
	disc, derr := coordx.NewDiscover(cfg)
	if derr != nil {
		return degrade(env, "registry", ConsequenceDiscovery, derr)
	}
	env.Discover = disc
	env.AddStop("registry.discover", func() error {
		disc.Close()
		return nil
	})
	return nil
}
