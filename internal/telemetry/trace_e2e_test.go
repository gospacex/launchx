package telemetry

import (
	"context"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// T1 与 T7 必须成对：只断言「注册后能注入」而不断言「未注册时注入为空」，
// 用例会永远绿 —— 因为不管谁把全局传播器设成了什么都算"注册过"。
//
// 分工（避免与仓根 trace_test.go 重复）：仓根那份验的是 Inject/Extract 两个函数
// 在给定传播器下的行为；本文件验的是**telemetry 步骤的注册义务** —— 装配漏掉
// 这一步，整条跨进程链路会静默断成多棵孤儿树（每个服务都能查到 trace，只是
// trace-id 互不相同，验收案例 T6 专门防这种假通过）。
//
// 这些用例刻意不连接任何中间件：Init 的 enabled=false 走 noop provider，
// localhost:4318 起不起得来都不影响结论。

// restoreGlobals 在用例结束后还原 otel 全局。
//
// 这不是洁癖：本文件断言的正是"全局状态是什么"，泄漏出去会让同包后续用例
// （以及别的包里跑过 Init 的集成用例）被运行顺序左右，红了也查不到这里。
func restoreGlobals(t *testing.T) {
	t.Helper()
	prevP, prevTP := otel.GetTextMapPropagator(), otel.GetTracerProvider()
	t.Cleanup(func() {
		otel.SetTextMapPropagator(prevP)
		otel.SetTracerProvider(prevTP)
	})
}

// recordingProvider 装一个**本地** SDK provider，用于产出真实的 span context。
//
// 为什么不用 p.TracerProvider()：enabled=false 时 otelx 交出的是 noop provider，
// 它的 span context 全零、`IsValid()` 为假，注入自然写不出 traceparent。T1 要验的
// 是**传播器**（由 Init 注册），span context 只是喂给它的输入 —— 两者分开测，
// 谁出错指向谁。
func recordingProvider(t *testing.T) *sdktrace.TracerProvider {
	t.Helper()
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp
}

// TestT7NoTelemetryStepExtractsNoUpstream 反向断言：不跑 telemetry 步骤时提取不到上游 context。
//
// 进程刚起来的状态就是空 Composite 传播器（otel 的默认值）。这里显式把它摆回
// 这个初始状态再测 —— 同一测试进程里若已有别的用例调过 Init，断言"当前是什么"
// 就会被运行顺序左右。
func TestT7NoTelemetryStepExtractsNoUpstream(t *testing.T) {
	restoreGlobals(t)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator()) // 空组合 ≡ noop
	recordingProvider(t)

	ctx, span := otel.GetTracerProvider().Tracer("t7").Start(context.Background(), "upstream")
	if !span.SpanContext().IsValid() {
		t.Fatal("用例前提不成立：本地 SDK 未产出有效的 span context")
	}
	carrier := propagation.HeaderCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	span.End()

	if got := carrier.Get("traceparent"); got != "" {
		t.Fatalf("未注册传播器时不该写出 traceparent，实际写出 %q", got)
	}
	if fields := PropagatorFields(); len(fields) != 0 {
		t.Fatalf("未注册传播器时 Fields 应为空，实际 %v", fields)
	}
	down := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	if sc := trace.SpanContextFromContext(down); sc.IsValid() {
		t.Fatal("未跑 telemetry 步骤却提取到了上游 span context：T7 反向断言失效")
	}
}

// TestT1TelemetryStepRegistersW3CPropagator 断言 telemetry 步骤后传播器覆盖 W3C 三个头，
// 且注入后 carrier 里有合法 traceparent —— 并与上述 T7 形成闭环对照。
func TestT1TelemetryStepRegistersW3CPropagator(t *testing.T) {
	restoreGlobals(t)

	// enabled=false 是刻意的：按包注释，传播器的注册与"是否采样"无关，
	// 这也是本用例能脱离 Jaeger 单跑的前提。
	p, err := Init("launchx-t1-probe", "", false)
	if err != nil {
		t.Fatalf("Init 失败：%v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	fields := PropagatorFields()
	for _, want := range []string{"traceparent", "tracestate", "baggage"} {
		if !slices.Contains(fields, want) {
			t.Fatalf("Fields %v 缺 %q：TraceContext + Baggage 必须都注册（缺 Baggage 会让 baggage 静默丢失）", fields, want)
		}
	}

	recordingProvider(t)
	ctx, span := otel.GetTracerProvider().Tracer("t1").Start(context.Background(), "upstream")
	sc := span.SpanContext()
	carrier := propagation.HeaderCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	span.End()

	got := carrier.Get("traceparent")
	if got == "" {
		t.Fatal("注入后 traceparent 为空：跨进程链路会从这一跳断开")
	}
	if !strings.Contains(got, sc.TraceID().String()) {
		t.Fatalf("traceparent %q 未携带上游 trace id %q", got, sc.TraceID())
	}

	// 闭环：只注入不提取等于白写，两者必须对得上。
	down := trace.SpanContextFromContext(otel.GetTextMapPropagator().Extract(context.Background(), carrier))
	if !down.IsValid() || down.TraceID() != sc.TraceID() {
		t.Fatalf("下游提取到的 trace id = %s，want %s", down.TraceID(), sc.TraceID())
	}
}

// TestInitRequiresServiceName 断言空服务名被拒。
//
// 守卫的意义：允许空名字注册，Jaeger 里会多出一堆 serviceName="" 的进程节点，
// 排查时既看不出是谁，也删不掉。
func TestInitRequiresServiceName(t *testing.T) {
	restoreGlobals(t)
	if _, err := Init("", DefaultEndpoint, false); err == nil {
		t.Fatal("空服务名应被拒绝")
	}
}
