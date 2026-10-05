package component

import (
	"fmt"

	"github.com/gospacex/cachex"
	_ "github.com/gospacex/cachex/redis" // 注册 redis 驱动（blank import）
)

func init() { Register("cache", buildCache) }

// cacheSection 是 xmiddleware.cachex 段的最小映射（只取 launchx 需要的字段）。
type cacheSection struct {
	Driver      string                   `mapstructure:"driver"`
	EnableTrace bool                     `mapstructure:"enable_trace"`
	Instances   map[string]cachex.Config `mapstructure:"instances"`
}

// buildCache 构造缓存组件（yaml 段：xmiddleware.cachex.instances.<service>）。
func buildCache(env *Env) error {
	if pickSection(env.Cfg, "xmiddleware.cachex") == "" {
		return nil
	}
	var sec cacheSection
	if err := env.Cfg.Unmarshal("xmiddleware.cachex", &sec); err != nil {
		return degrade(env, "cache", ConsequenceCache, err)
	}
	if len(sec.Instances) == 0 {
		return degrade(env, "cache", ConsequenceCache, fmt.Errorf("xmiddleware.cachex.instances is empty"))
	}
	// 组件级开关：入链 = 全局总闸 AND trace.on.cache AND 实例/段级 enable_trace。
	// 注意它只影响入链，不影响下面的构造。
	wantTrace := traceOn(env, "cache")
	cfgs := make([]*cachex.Config, 0, len(sec.Instances))
	for name := range sec.Instances {
		inst := sec.Instances[name] // 值拷贝，避免改到 map 里的原值
		inst.Name = name
		if inst.Driver == "" {
			inst.Driver = sec.Driver
		}
		inst.EnableTrace = (inst.EnableTrace || sec.EnableTrace) && wantTrace
		cfgs = append(cfgs, &inst)
	}
	instances, err := cachex.New(cfgs)
	if err != nil {
		return degrade(env, "cache", ConsequenceCache, err)
	}
	env.Cache = instances
	env.AddStop("cache", func() error {
		for _, c := range instances {
			if c != nil {
				_ = c.Close()
			}
		}
		return nil
	})
	return nil
}
