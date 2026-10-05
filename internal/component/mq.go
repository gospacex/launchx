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
// 历史偏差（2026-10-05 审计发现，同日修在 mqx 侧）：
// 五个驱动（kafka / nsq / pulsar / rocketmq / rabbitmq）原先一律
// `otel.Tracer(...)` 直接 Start，外面没有 EnableTrace 包裹 —— 于是
// trace.on.mq=false 只压住 EnableTrace / CallChainEnabled 两个字段
// （它们只管日志与内存调用链条目），**span 照产照报**，违反
// 「trace.on.* 只控入链」的红线。
//
// 现已改为 `traceflag.NewTracer(cfg.EnableTrace || cfg.CallChainEnabled,
// <driver>, "mqx")`：开关关时 Start 返回 noop span，且仍受 traceflag
// 全局总闸与 SetComponent("mqx", false) 约束。回归测试见
// mqx/kafka/tracer_gate_test.go（同时钉住"开关不控构造"这条）。
//
// 结论：launchx 侧仍只需管 cfg 这两个字段，不要另加拦截 —— span 在 SDK
// 内部产生，拦不住，只有在 SDK 里改对才有效。
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
