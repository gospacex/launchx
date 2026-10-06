package component

import (
	"github.com/gospacex/lockx"
	_ "github.com/gospacex/lockx/redis" // 注册 redis 驱动（blank import）
)

func init() { Register("lock", buildLock) }

// buildLock 构造分布式锁组件（yaml 段：xmiddleware.lockx）。
//
// 为什么逐字段读而不 Unmarshal：lockx.Config 的三套 tag 里没有 mapstructure
// （与 mongodbx 同一个坑），`enable_trace / max_attempts` 这类蛇形键经 viper
// 通道解码会被**静默丢弃** —— 比字段名拼错更难发现（没有报错，只是永远关着）。
//
// lockx.New 是单实例工厂（不像 kvx/cachex 有 instances 表），故配置段是平铺的：
// 一个进程一把锁器，锁的隔离靠 Config.Namespace 与业务侧的 key 前缀。
//
// 调用链：lockx.New 内部经 traceflag.Apply("lockx", ...) 收敛开关，这里只按
// trace.on.lock 给出组件级裁决（全局总闸仍可一键熔断）。
func buildLock(env *Env) error {
	key := pickSection(env.Cfg, "xmiddleware.lockx")
	if key == "" {
		return nil
	}
	cfg := &lockx.Config{
		Driver:      env.Cfg.GetString(key + ".driver"),
		Name:        env.Cfg.GetString(key + ".name"),
		Addr:        env.Cfg.GetString(key + ".addr"),
		Password:    env.Cfg.GetString(key + ".password"),
		Namespace:   env.Cfg.GetString(key + ".namespace"),
		TTL:         env.Cfg.GetDuration(key + ".ttl"),
		TLSEnabled:  env.Cfg.GetBool(key + ".tls_enabled"),
		EnableTrace: traceOn(env, "lock"),
		Retry: lockx.RetryConfig{
			Enabled:     env.Cfg.GetBool(key + ".retry.enabled"),
			MaxAttempts: env.Cfg.GetInt(key + ".retry.max_attempts"),
			BackoffMs:   env.Cfg.GetInt(key + ".retry.backoff_ms"),
		},
	}
	if cfg.Driver == "" {
		cfg.Driver = "redis"
	}
	if cfg.Name == "" {
		cfg.Name = env.ServiceName
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:6379"
	}
	// FillDefaults 在 New 内部调用：TTL/Retry 的零值在那里补默认，
	// 此处不重复填一遍，避免两处默认值将来各自漂移。
	locker, err := lockx.New(cfg)
	if err != nil {
		return degrade(env, "lock", ConsequenceLock, err)
	}
	env.Lock = locker
	env.AddStop("lock", func() error {
		_ = locker.Close()
		return nil
	})
	return nil
}
