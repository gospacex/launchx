package component

import (
	"fmt"

	"github.com/gospacex/mqx"
	_ "github.com/gospacex/mqx/kafka" // 注册 kafka 驱动（blank import）
)

func init() { Register("mq", buildMQ) }

// buildMQ 构造消息生产者（yaml 段：xmiddleware.mqx.instances.<service>）。
//
// 红线：trace.on.mq=false **不阻止** producer 的构造，只是不产 span。
// 历史 bug 把该开关当构造开关，producer 变 nil，outbox 静默堆积到
// retry_count=10 才放弃（"消息没发出去但没人报错"）。
//
// 已知偏差（2026-10-05 审计，修在 mqx 侧，别在 launchx 里绕）：
// mqx 的驱动 span 是**无条件**产生的 —— kafka.go 的 Send 直接
// `p.tracer.Start(ctx, "messaging.kafka.send")`，外面没有 EnableTrace 包裹，
// callchain.go 里的 span 同理。也就是说：
//   - trace.enabled=false（全局）→ provider 是 noop，span 自然不导出；
//   - 但只把 trace.on.mq 置 false、全局仍开 → 开关只压住 EnableTrace /
//     CallChainEnabled 两个字段（它们只管日志与内存调用链条目），
//     span 照产照报。
//
// 这条违反「trace.on.* 只控入链」的红线，定性为 mqx 缺陷；launchx 侧
// 无法拦截（span 在 SDK 内部产生），只能如实记录。
func buildMQ(env *Env) error {
	if pickSection(env.Cfg, "xmiddleware.mqx") == "" {
		return nil
	}
	cfg := mqx.ReadConfigFromConfigx(env.Cfg, env.ServiceName)
	if cfg == nil || cfg.Driver == "" {
		return degrade(env, "mq", ConsequenceMQ,
			fmt.Errorf("xmiddleware.mqx.driver is empty (instance %q)", env.ServiceName))
	}
	if cfg.Logger == nil {
		cfg.Logger = env.Logger
	}
	wantTrace := traceOn(env, "mq")
	cfg.EnableTrace = cfg.EnableTrace && wantTrace
	cfg.CallChainEnabled = cfg.CallChainEnabled && wantTrace

	producer, err := mqx.NewProducer(cfg)
	if err != nil {
		return degrade(env, "mq", ConsequenceMQ, err)
	}
	env.MQ = producer
	env.AddStop("mq", func() error {
		producer.Close() // 忽略返回值：不同驱动签名不一，统一按语句调用
		return nil
	})
	return nil
}
