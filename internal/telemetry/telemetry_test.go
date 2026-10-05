package telemetry

import (
	"sort"
	"testing"
)

// TestInitRejectsEmptyServiceName 断言服务名为空即退（地基参数不降级）。
func TestInitRejectsEmptyServiceName(t *testing.T) {
	if _, err := Init("", "localhost:4318", false); err == nil {
		t.Fatal("empty service name must be fatal")
	}
}

// TestInitDisabledStillRegistersPropagator 断言：即使 trace 关闭，
// 传播器也必须注册 —— 传播器与采样开关无关，缺它会让链路静默断裂。
// 本用例用 enabled=false，避免向真实 OTLP 端点导出。
func TestInitDisabledStillRegistersPropagator(t *testing.T) {
	p, err := Init("launchx-test", "", false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if p == nil {
		t.Fatal("provider must not be nil")
	}
	fields := PropagatorFields()
	sort.Strings(fields)
	want := []string{"baggage", "traceparent", "tracestate"}
	if len(fields) != len(want) {
		t.Fatalf("propagator fields = %v, want %v", fields, want)
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Fatalf("propagator fields = %v, want %v", fields, want)
		}
	}
}
