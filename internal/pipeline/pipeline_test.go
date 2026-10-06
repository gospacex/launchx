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

// TestStartupOrder 断言完整启动骨架的顺序红线（验收案例 A2）。
//
// 事件名取骨架常量（config.load / telemetry.init / logger.init / mode.assemble /
// component.mount / registry.register / setup）：组件挂载在真实实现里是**一步**
// （component.mount 内部按 db/cache/mq/kv/lock/registry/search/mongodb 顺序挂），
// 不拆成 component.db 等逐组件事件；logx 全局设置为 logger.init。
func TestStartupOrder(t *testing.T) {
	ev := NewEvents()
	noop := func(*Env) error { return nil }
	err := Run(&Env{Events: ev, Logger: nopLogger{}}, []Step{
		{Name: StepConfigLoad, Fn: noop},
		{Name: StepTelemetryInit, Fn: noop},
		{Name: StepLoggerInit, Fn: noop},
		{Name: StepModeAssemble, Fn: noop},
		{Name: StepComponentMount, Fn: noop},
		{Name: StepRegistryRegister, Fn: noop},
		{Name: StepSetup, Fn: noop},
	})
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	at := func(name string) int {
		i := ev.IndexOf(name)
		if i < 0 {
			t.Fatalf("missing startup event %q in %v", name, ev.Order())
		}
		return i
	}
	// config.load 最早（配置是后面每一步的输入）。
	if got := at(StepConfigLoad); got != 0 {
		t.Fatalf("config.load must be first, got index %d: %v", got, ev.Order())
	}
	// telemetry.init 严格早于组件挂载与注册（否则组件拿到 noop tracer）。
	for _, later := range []string{StepComponentMount, StepRegistryRegister} {
		if at(StepTelemetryInit) >= at(later) {
			t.Fatalf("telemetry.init must precede %s: %v", later, ev.Order())
		}
	}
	// logger.init 在 telemetry.init 之后、mode.assemble 之前。
	if at(StepLoggerInit) <= at(StepTelemetryInit) {
		t.Fatalf("logger.init must follow telemetry.init: %v", ev.Order())
	}
	if at(StepLoggerInit) >= at(StepModeAssemble) {
		t.Fatalf("logger.init must precede mode.assemble: %v", ev.Order())
	}
	// 形态装配早于组件挂载（组件按形态取地址与发现句柄）。
	if at(StepModeAssemble) >= at(StepComponentMount) {
		t.Fatalf("mode.assemble must precede component.mount: %v", ev.Order())
	}
	// setup 最后：基础设施就绪后才交给业务。
	if at(StepSetup) != len(ev.Order())-1 {
		t.Fatalf("setup must be last: %v", ev.Order())
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
