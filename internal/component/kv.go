package component

import (
	"fmt"

	"github.com/gospacex/kvx"
	_ "github.com/gospacex/kvx/redis" // 注册 redis 驱动（blank import）
)

func init() { Register("kv", buildKV) }

// kvSection 是 xmiddleware.kvx 段的最小映射（只取 launchx 需要的字段）。
type kvSection struct {
	Driver      string                `mapstructure:"driver"`
	EnableTrace bool                  `mapstructure:"enable_trace"`
	Instances   map[string]kvx.Config `mapstructure:"instances"`
}

// buildKV 构造 KV 存储组件（yaml 段：xmiddleware.kvx.instances.<service>）。
//
// 与 cachex 的分工：cachex 面向"可失效的缓存语义"（带 TTL 的对象缓存），
// kvx 面向"可直接读写、可自增的键值状态"（幂等键、限流计数、跨请求的状态位）。
// 两者都走 redis 驱动，但语义不同，故句柄分开暴露 —— 拿缓存句柄去存必须持久
// 的状态，被逐出时会表现为「能力悄悄失效」；反过来用 KV 存纯派生缓存，等于
// 放弃 TTL 与失效语义。需要原子自增的状态一律走 kvx（cachex 无 Incr）。
//
// kvx 的调用链在实例级：kvx.New 内部对 EnableTrace=true 的实例套 traceStore
// 包装器（产 db.kvx.<OP> span），故这里只需按 trace.on.kv 裁决开关，
// 不碰全局 provider（宿主已注册全局 provider，SDK 不得再抢）。
func buildKV(env *Env) error {
	key := pickSection(env.Cfg, "xmiddleware.kvx")
	if key == "" {
		return nil
	}
	var sec kvSection
	if err := env.Cfg.Unmarshal(key, &sec); err != nil {
		return degrade(env, "kv", ConsequenceKV, err)
	}
	if len(sec.Instances) == 0 {
		return degrade(env, "kv", ConsequenceKV, fmt.Errorf("%s.instances is empty", key))
	}
	wantTrace := traceOn(env, "kv")
	cfgs := make([]*kvx.Config, 0, len(sec.Instances))
	for name := range sec.Instances {
		inst := sec.Instances[name] // 值拷贝，避免改到 map 里的原值
		inst.Name = name
		if inst.Driver == "" {
			inst.Driver = sec.Driver
		}
		inst.EnableTrace = (inst.EnableTrace || sec.EnableTrace) && wantTrace
		cfgs = append(cfgs, &inst)
	}
	stores, err := kvx.New(cfgs)
	if err != nil {
		return degrade(env, "kv", ConsequenceKV, err)
	}
	env.KV = stores
	env.AddStop("kv", func() error {
		for _, s := range stores {
			if s != nil {
				_ = s.Close()
			}
		}
		return nil
	})
	return nil
}
