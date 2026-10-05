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
	return nil
}
