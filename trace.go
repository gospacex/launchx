package launchx

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// Inject 把 ctx 里的 span context 注入出向请求的 carrier，完成跨进程续链。
//
// 存在原因：webx 的 otel 中间件只做 Extract（入站提取），全 hubx 侧没有
// 出向注入入口。网关/代理类服务在转发前若不注入 traceparent，下游 Extract
// 拿不到上游 context，链路会从这一跳断成两棵独立的树——且表面上看两个
// 服务"都有 span"，极易被误判为链路正常。
//
// 本函数是 otel 全局传播器的薄封装，**不含任何转发逻辑**：它只写 carrier，
// 不碰请求也不建连接。能注入出什么取决于 telemetry 步骤注册的传播器
// （Composite{TraceContext, Baggage}）；未注册时全局是 noop，注入为空，
// 这本身就是一条可断言的反向判据（验收案例 T7）。
//
// 入参取 TextMapCarrier 而不是 HeaderCarrier：HTTP 头是一类 carrier，
// MQ 消息头（MapCarrier / 自定义单值 map）是另一类。只认 HTTP 头会逼着
// 消息侧在业务代码里裸调 otel 全局，等于把唯一的续链入口堵死一半。
// HeaderCarrier 本身实现 TextMapCarrier，故这次放宽对既有调用方是兼容的。
func Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	if carrier == nil {
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
}

// Extract 从入向 carrier 提取上游 span context，返回携带该 context 的 ctx。
//
// 与 Inject 成对提供：反向代理既要从入站请求取 context（Extract），
// 也要把它写进出站请求（Inject）。只做一半等于断链。
func Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	if carrier == nil {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

// PropagatorFields 返回当前全局传播器会读写的 header 名集合。
//
// 用于验收断言：telemetry 步骤跑完后必须含 traceparent / tracestate / baggage
// （验收案例 T1）；为空即说明传播器没注册，跨服务链路必断。
func PropagatorFields() []string {
	return otel.GetTextMapPropagator().Fields()
}

// Propagator 返回当前全局传播器，供需要自定义 carrier 形态的调用方使用。
func Propagator() propagation.TextMapPropagator {
	return otel.GetTextMapPropagator()
}
