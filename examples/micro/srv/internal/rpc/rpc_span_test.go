package rpc

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestGuardSpanEnds：guard 产出的分层 span 必须真实导出（End 后可见），
// 回归「ctx, _ = tracex.Start」span 泄漏缺陷（span 永不 End → 不导出，
// 且子 span parent 悬空 → Jaeger missing-parent）。
//
// 注入方式说明：tracex.Start 取全局 TracerProvider，这里用
// otel.SetTracerProvider 注册内存 exporter 的 provider —— 全局 provider 是
// 委托实现，先于注册取到的 tracer 也会转发到后注册的 provider。
func TestGuardSpanEnds(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(tp)

	s := &Server{} // Limiter 为 nil 时 guard 直接放行
	ctx, span, ok := s.guard(context.Background(), "CreateOrder", 1)
	if !ok {
		t.Fatal("guard should allow when limiter is nil")
	}
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 exported span, got %d", len(spans))
	}
	if name := spans[0].Name; name != "service.order.CreateOrder" {
		t.Fatalf("unexpected span name %q", name)
	}
	_ = ctx
}
