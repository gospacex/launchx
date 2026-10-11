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
// 采样缺省为 `always_on`（历史行为，本地与验收适用）。生产要控制成本请走
// `InitWithSampler` 或由 App 从 `trace.sampler*` 读配置。
func Init(serviceName, endpoint string, enabled bool) (otelx.Provider, error) {
	return InitWithSampler(serviceName, endpoint, enabled, otelx.SamplerConfig{Type: "always_on"})
}

// InitWithSampler 同 Init，但显式给定采样配置。
//
// 为什么必须留这个出口：采样率写死在库里，下游所有业务就都被迫继承同一个
// 策略 —— 生产高 QPS 下 100% 采样是实打实的成本与 exporter 背压。没有出口，
// 想改的业务只能绕过本包自己调 `otel.SetTracerProvider`，反而更危险。
//
// Type 为空时 otelx 会按 `always_on` 处理（见 otelx.buildSampler），
// 故零值 Config 是安全的。
func InitWithSampler(serviceName, endpoint string, enabled bool, sampler otelx.SamplerConfig) (otelx.Provider, error) {
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
		Sampler: sampler,
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
