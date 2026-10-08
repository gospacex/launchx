// Package messaging 消息投递适配层：把 mqx 的发送包成本层的 `messaging.*` span。
//
// 跨进程续链**不由本层做**：mqx 的四个驱动在 Send / SendBatch 发出前都会调
// mqx.InjectTrace(ctx, msg) 写 traceparent（覆盖语义：ctx 站在哪，谁就是下游的
// 父节点）。业务侧再写一遍不只是冗余 —— 它把"续链发生在哪里"在业务层又声明了
// 一次，mqx 改口径时这里会静默不同步，表现为链路偶发断在消费侧。
//
// nsq 协议不承载 header，走 nsq 的消息无法续链（已在 mqx/propagation.go 注明）。
package messaging

import (
	"context"
	"fmt"

	"github.com/gospacex/mqx"
	"go.opentelemetry.io/otel/attribute"

	"github.com/gospacex/otelx/layer"
)

// spanPrefix 本层前缀。
const spanPrefix = "messaging."

// Order 订单域投递器。
type Order struct {
	producer mqx.Producer
	topic    string
	traceOn  bool
}

// NewOrder 构造投递器。producer 由装配层从 App.MQ() 取；topic 来自配置。
func NewOrder(producer mqx.Producer, topic string, traceOn bool) *Order {
	return &Order{producer: producer, topic: topic, traceOn: traceOn}
}

// Publish 投递一条订单事件。
//
// 降级语义由调用方保证 producer 非 nil —— 本方法不吞 nil：producer 为空时
// 返回 error 并打进 span，让"事件没发出去"在链路上可见，而不是静默丢。
// begin 开本层 span（三段式 messaging.order.<Op>），返回「收 error 再收尾」的闭包。
func (o *Order) begin(ctx context.Context, op string) (context.Context, layer.End) {
	return layer.Begin(ctx, "messaging", "order", op, o.traceOn, nil)
}

func (o *Order) Publish(ctx context.Context, key string, payload []byte) (err error) {
	ctx, end := o.begin(ctx, "PublishOrder")
	defer func() { end(err) }()
	layer.Attrs(ctx,
		attribute.String("messaging.system", "kafka"),
		attribute.String("messaging.destination", o.topic),
		attribute.String("messaging.message_key", key),
	)

	// Headers 刻意留空：traceparent 由 mqx 自己注入 —— 四个驱动的 Send / SendBatch
	// 在发出前都会调 mqx.InjectTrace(ctx, msg)（kafka.go / pulsar.go / rabbitmq.go /
	// rocketmq.go），业务侧再写一遍是重复的。
	//
	// 曾经这里手写 launchx.Inject 填 msg.Headers，那不只是冗余：它把"跨进程续链
	// 发生在哪里"这件事在业务层又声明了一次，于是 mqx 改注入口径时这里会静默
	// 不同步 —— 表现为链路偶发断在消费侧，而代码看着完全正确。
	//
	// Topic 同样留空：producer 由 xmiddleware.mqx 配的目标 topic 构造（writer 自带
	// topic），kafka-go 不接受 writer 与 message 同时指定 topic
	// ——2026-10-05 实测报 "Topic must not be specified for both Writer and Message"。
	// 目标 topic 进 span 属性即可检索，不必写进消息体。
	msg := &mqx.Message{
		Key:   key,
		Value: payload,
	}
	if err = o.producer.Send(ctx, msg); err != nil {
		err = fmt.Errorf("messaging: publish order event: %w", err)
	}
	return err
}
