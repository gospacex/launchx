package launchx

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

// withPropagator 临时替换全局传播器与 TracerProvider，用例结束还原。
//
// 全局状态必须成对还原：这些用例断言的正是"全局传播器是什么"，
// 泄漏出去会让后续用例在同一进程里互相污染。
func withPropagator(t *testing.T, p propagation.TextMapPropagator) {
	t.Helper()
	prevP, prevTP := otel.GetTextMapPropagator(), otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider()
	otel.SetTextMapPropagator(p)
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTextMapPropagator(prevP)
		otel.SetTracerProvider(prevTP)
		_ = tp.Shutdown(context.Background())
	})
}

// remoteSpanContext 开一个 span 并返回其 SpanContext（作为"上游"注入源）。
func remoteSpanContext(t *testing.T) (context.Context, trace.SpanContext) {
	t.Helper()
	ctx, span := otel.GetTracerProvider().Tracer("launchx-test").Start(context.Background(), "upstream")
	sc := span.SpanContext()
	span.End()
	return ctx, sc
}

// TestInjectWritesTraceParent 断言注入后 carrier 里有合法的 traceparent（T1）。
func TestInjectWritesTraceParent(t *testing.T) {
	withPropagator(t, propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	ctx, sc := remoteSpanContext(t)
	carrier := propagation.HeaderCarrier{}
	Inject(ctx, carrier)

	got := carrier.Get("traceparent")
	if got == "" {
		t.Fatal("Inject 后 traceparent 为空：跨服务链路会从这一跳断开")
	}
	if !sc.IsValid() {
		t.Fatal("上游 span context 无效，用例前提不成立")
	}
	if want := sc.TraceID().String(); !strings.Contains(got, want) {
		t.Fatalf("traceparent 未携带上游 trace id: got %q, want contains %q", got, want)
	}
}

// TestInjectAndExtractRoundTrip 断言注入—提取闭环：下游能取回同一个 trace id。
func TestInjectAndExtractRoundTrip(t *testing.T) {
	withPropagator(t, propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	ctx, sc := remoteSpanContext(t)
	carrier := propagation.HeaderCarrier{}
	Inject(ctx, carrier)

	down := Extract(context.Background(), carrier)
	got := trace.SpanContextFromContext(down)
	if !got.IsValid() {
		t.Fatal("Extract 取不到有效 span context：注入与提取没有闭环")
	}
	if got.TraceID() != sc.TraceID() {
		t.Fatalf("trace id 漂移: got %s, want %s", got.TraceID(), sc.TraceID())
	}
}

// TestDefaultPropagatorIsNoop 反向断言：不注册传播器时提取不到上游 context（T7）。
//
// 这条用例的价值在于"必须失败"——如果它在未注册传播器的情况下也能提取到
// 上游 context，说明测试环境被别的用例污染，T7 的判据就失效了。
func TestDefaultPropagatorIsNoop(t *testing.T) {
	withPropagator(t, propagation.NewCompositeTextMapPropagator()) // 空组合 = 等价 noop

	ctx, _ := remoteSpanContext(t)
	carrier := propagation.HeaderCarrier{}
	Inject(ctx, carrier)

	if got := carrier.Get("traceparent"); got != "" {
		t.Fatalf("noop 传播器不应写出 traceparent，实际写出 %q", got)
	}
	if sc := trace.SpanContextFromContext(Extract(context.Background(), carrier)); sc.IsValid() {
		t.Fatal("noop 传播器下 Extract 仍取到有效 span context：反向断言失效")
	}
	if n := len(PropagatorFields()); n != 0 {
		t.Fatalf("noop 传播器的 Fields 应为空，实际 %d 项", n)
	}
}

// TestPropagatorFieldsContainsW3C 断言注册后 Fields 覆盖 W3C 三个头（T1 的字段面）。
func TestPropagatorFieldsContainsW3C(t *testing.T) {
	withPropagator(t, propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	fields := PropagatorFields()
	for _, want := range []string{"traceparent", "tracestate", "baggage"} {
		if !containsIn(fields, want) {
			t.Fatalf("Fields %v 缺 %q（传播器未覆盖 W3C 全量）", fields, want)
		}
	}
}

// TestInjectNilCarrierSafe 断言 nil carrier 不 panic（转发场景容易传空）。
func TestInjectNilCarrierSafe(t *testing.T) {
	withPropagator(t, propagation.TraceContext{})
	ctx, _ := remoteSpanContext(t)
	Inject(ctx, nil)                          // 不应 panic
	if got := Extract(ctx, nil); got == nil { // 不应 panic，且原样返回
		t.Fatal("Extract(nil carrier) 返回 nil ctx")
	}
}

func containsIn(list []string, want string) bool {
	return slices.Contains(list, want)
}
