package component

import (
	"fmt"

	"github.com/gospacex/mqx"
	_ "github.com/gospacex/mqx/kafka" // 注册 kafka 驱动（blank import）
)

func init() { Register("mq", buildMQ) }

// buildMQ 构造消息的生产者与消费者（yaml 段：xmiddleware.mqx.instances.<service>）。
//
// 两侧共用同一份 cfg：broker / topic / 调用链开关必须同源，否则会出现
// "producer 按 A topic 发、consumer 按 B topic 收" 或者 "trace.on.mq=false
// 时 producer 不入链、消费侧却照录" 这类半边状态。
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

	// 消费侧独立降级：两边 NZ APPLICATION 不是一个失败模式 —— producer 挂了是
	// "事件投不出去"，consumer 挂了是"排队的业务没人干"。合成一次失败会把后者
	// 的后果文案盖掉，排障时看不出来到底哪一侧不可用。
	consumer, cerr := mqx.NewConsumer(cfg)
	if cerr != nil {
		return degrade(env, "mq", ConsequenceMQConsume, cerr)
	}
	env.MQConsumer = consumer
	env.AddStop("mq.consumer", func() error {
		consumer.Close()
		return nil
	})
	return nil
}
