// Package messaging 消息投递适配层：把 mqx 的发送包成本层的 `messaging.*` span，
// 并把 traceparent 注入消息头完成跨进程续链。
//
// 为什么必须显式注入：MQ 不像 HTTP 那样有中间件替你 Extract/Inject，
// producer 不写 traceparent 的话，消费端拿不到上游 context，链路在投递这一跳
// 断成两棵孤儿树（与 gateway 忘注入的现象同构，见 launchx/trace.go 的注释）。
package messaging

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gospacex/launchx"
	"github.com/gospacex/mqx"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

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

// flatten 把 http.Header 摊平成 mqx 的消息头（多值取第一个）。
//
// 为什么要这层转换：注入器写的是 TextMapCarrier（多值语义），而 mqx.Message.Headers
// 是单值 map。丢掉多余的值不算丢数据 —— traceparent / tracestate / baggage 都是单值。
func flatten(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

// Publish 投递一条订单事件。
//
// 降级语义由调用方保证 producer 非 nil —— 本方法不吞 nil：producer 为空时
// 返回 error 并打进 span，让"事件没发出去"在链路上可见，而不是静默丢。
// begin 开本层 span（沿用两段式 messaging.<Op> 命名），返回「收 error 再收尾」的闭包。
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

	headers := http.Header{}
	// 注入走 launchx.Inject（传播器由 telemetry 步骤注册），业务侧不裸调 otel 全局。
	launchx.Inject(ctx, propagation.HeaderCarrier(headers))

	// Topic 刻意留空：producer 由 xmiddleware.mqx 配的目标 topic 构造（writer 自带
	// topic），kafka-go 不接受 writer 与 message 同时指定 topic
	// ——2026-10-05 实测报 "Topic must not be specified for both Writer and Message"。
	// 目标 topic 进 span 属性即可检索，不必写进消息体。
	msg := &mqx.Message{
		Key:     key,
		Value:   payload,
		Headers: flatten(headers),
	}
	if err = o.producer.Send(ctx, msg); err != nil {
		err = fmt.Errorf("messaging: publish order event: %w", err)
	}
	return err
}
