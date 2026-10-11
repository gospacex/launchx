// 单号裁决的单测：钉住「调用方给的幂等键必须优先」。
package service

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestResolveOrderNoUsesCallerKey 调用方带了号就必须用它的 —— 订单域自己另起
// 一个号，等于把幂等键作废：重放一次 = 一张新单，而库存/积分侧按 bff 那个号
// 幂等掉了，对账出现「下了两单、只送一次积分」。
//
// 反向探针：把 resolveOrderNo 改成无条件 `return newOrderNo()`，本用例立刻红。
func TestResolveOrderNoUsesCallerKey(t *testing.T) {
	s := &OrderService{}
	if got := resolveOrderNo(s, context.Background(), "O-CALLER-1"); got != "O-CALLER-1" {
		t.Fatalf("resolveOrderNo = %q, want 原样用调用方的 O-CALLER-1", got)
	}
}

// TestResolveOrderNoFallsBackAndLeavesTrace 调用方没带号时回落生成，但必须留痕。
//
// 留痕这一半是重点：静默回落的代价是「这段调用没有幂等保护」，而它只在**重试
// 真的发生**那天才暴露。没有 span 属性，排障时没人能回答"当时到底带没带键"。
func TestResolveOrderNoFallsBackAndLeavesTrace(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))

	s := &OrderService{}
	ctx, span := tp.Tracer("test").Start(context.Background(), "CreateOrder")
	got := resolveOrderNo(s, ctx, "")
	span.End()

	if got == "" {
		t.Fatal("没带键时也必须产出一个可用单号（不能下出空单号的单）")
	}
	var marked bool
	for _, sp := range rec.Ended() {
		for _, kv := range sp.Attributes() {
			if string(kv.Key) == "order.idempotent_key_missing" && kv.Value.AsBool() {
				marked = true
			}
		}
	}
	if !marked {
		t.Fatal("回落必须打 order.idempotent_key_missing=true —— 否则'幂等没生效'会静默到对账那天")
	}
}
