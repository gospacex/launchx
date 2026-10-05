// Package telemetry 封装 otelx 的装配与 OTel 全局注册。
//
// 本包是「启动顺序红线」的落点：它必须早于任何组件的构造、早于 mode 装配。
// 原因（2026-10-04 实测，非形式主义）：
//
//  1. otelx.New **会**调用 otel.SetTracerProvider(tp)（otelx/interface.go:207，
//     仅 Tracer.EnableTrace 为真时），但**从不**调用 otel.SetTextMapPropagator。
//     OTel 全局默认传播器是**空 Composite**（noop）：注入与提取皆为无操作，
//     于是 traceparent 既不写出也不读取 —— 跨服务链路断成多段孤儿 trace，
//     且伪装成成功（每个服务都能查到自己的 trace，只是 trace-id 各不相同）。
//     故本包 MUST 显式注册 Composite{TraceContext, Baggage}。
//  2. webx.Config.FillDefaults 的 Tracer 默认取
//     otel.GetTracerProvider().Tracer("webx")。若 telemetry 晚于 mode 装配，
//     webx 拿到的是 noop tracer，症状是「配了 enable_trace 却不产 span」——
//     静默故障，极难排查。
package telemetry

import (
	"fmt"

	"github.com/gospacex/otelx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// DefaultEndpoint 是 OTLP/HTTP 的默认端点，与本机 Jaeger 容器对齐
// （docker-compose: jaeger 暴露 4318 的 OTLP/HTTP）。
const DefaultEndpoint = "localhost:4318"

// Init 装配 otelx 并完成两项 OTel 全局注册，返回 provider 供退出链关闭。
//
// enabled=false 时 otelx 返回 noop provider 且不触碰全局 TracerProvider；
// 本函数仍会把它显式设为全局，以保证「关闭即全局无 span」的语义不被上游
// 遗留的 provider 破坏，并仍注册传播器（传播器与是否采样无关）。
func Init(serviceName, endpoint string, enabled bool) (otelx.Provider, error) {
	if serviceName == "" {
		return nil, fmt.Errorf("telemetry: service name is empty")
	}
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	p, err := otelx.New(&otelx.Config{
		ServiceName: serviceName,
		Tracer: otelx.TracerConfig{
			EnableTrace: enabled,
			Exported:    "otlp",
			Endpoint:    endpoint,
		},
		Sampler: otelx.SamplerConfig{Type: "always_on"},
	})
	if err != nil {
		return nil, fmt.Errorf("telemetry: init %q: %w", serviceName, err)
	}
	// 顺序不可交换，且必须在任何组件构造之前完成。
	otel.SetTracerProvider(p.TracerProvider())
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return p, nil
}

// PropagatorFields 返回当前全局传播器写出的头名，用于验收断言（T1）。
func PropagatorFields() []string {
	return otel.GetTextMapPropagator().Fields()
}
