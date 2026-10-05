package pipeline

import (
	"errors"
	"testing"

	"github.com/gospacex/logx"
)

// errBoom 步骤失败用哨兵错误。
var errBoom = errors.New("boom")

// nopLogger 测试用空日志器：嵌入 logx.Logger 接口以获得全部方法，
// 只显式覆盖本用例会触达的 Warn/Error；其余方法不会被调用。
type nopLogger struct{ logx.Logger }

func (nopLogger) Warn(msg string, fields ...logx.Field)  {}
func (nopLogger) Error(msg string, fields ...logx.Field) {}

// TestStartupOrderTelemetryBeforeComponents 断言顺序红线：
// telemetry.init 必须早于 component.mount（否则 webx/组件拿到 noop tracer）。
func TestStartupOrderTelemetryBeforeComponents(t *testing.T) {
	ev := NewEvents()
	noop := func(*Env) error { return nil }
	err := Run(&Env{Events: ev, Logger: nopLogger{}}, []Step{
		{Name: StepConfigLoad, Fn: noop},
		{Name: StepTelemetryInit, Fn: noop},
		{Name: StepLoggerInit, Fn: noop},
		{Name: StepModeAssemble, Fn: noop},
		{Name: StepComponentMount, Fn: noop},
		{Name: StepSetup, Fn: noop},
	})
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	tele := ev.IndexOf(StepTelemetryInit)
	comp := ev.IndexOf(StepComponentMount)
	if tele < 0 || comp < 0 {
		t.Fatalf("missing events: %v", ev.Order())
	}
	if tele >= comp {
		t.Fatalf("telemetry.init(%d) must precede component.mount(%d): %v", tele, comp, ev.Order())
	}
	if got := len(ev.Order()); got != 6 {
		t.Fatalf("expected 6 recorded steps, got %d: %v", got, ev.Order())
	}
}

// TestStepFailureAborts 断言任一步失败即中止，后续步骤不执行。
func TestStepFailureAborts(t *testing.T) {
	ev := NewEvents()
	err := Run(&Env{Events: ev, Logger: nopLogger{}}, []Step{
		{Name: StepConfigLoad, Fn: func(*Env) error { return errBoom }},
		{Name: StepTelemetryInit, Fn: func(*Env) error { return nil }},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("error must wrap sentinel, got: %v", err)
	}
	if ev.IndexOf(StepTelemetryInit) >= 0 {
		t.Fatal("later steps must not run after failure")
	}
}

// TestRunRejectsNilFunc 断言步骤未装 Fn 时明确报错，而不是 panic。
func TestRunRejectsNilFunc(t *testing.T) {
	err := Run(&Env{}, []Step{{Name: StepSetup}})
	if err == nil {
		t.Fatal("nil func must be rejected")
	}
}

// TestEventsFirstOccurrenceWins 断言同名事件只记首次，避免重试/幂等调用污染顺序。
func TestEventsFirstOccurrenceWins(t *testing.T) {
	ev := NewEvents()
	ev.Record("a")
	ev.Record("b")
	ev.Record("a")
	if got := ev.IndexOf("a"); got != 0 {
		t.Fatalf("first occurrence must win, got %d", got)
	}
	if got := len(ev.Order()); got != 2 {
		t.Fatalf("duplicate must not append, got %v", ev.Order())
	}
	if ev.IndexOf("missing") != -1 {
		t.Fatal("absent event must return -1")
	}
}
