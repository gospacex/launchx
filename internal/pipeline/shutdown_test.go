package pipeline

import (
	"errors"
	"reflect"
	"testing"
)

// errStopBoom 退出链某步失败的哨兵错误。
var errStopBoom = errors.New("stop boom")

// exitSteps 反序退出链的规范步骤序列（A3）：
// server.stop → component.stop → registry.deregister → tracing.shutdown → logger.close。
func exitSteps(record *[]string) []Step {
	mark := func(name string) func(*Env) error {
		return func(*Env) error {
			*record = append(*record, name)
			return nil
		}
	}
	return []Step{
		{Name: StepServerStop, Fn: mark(StepServerStop)},
		{Name: StepComponentStop, Fn: mark(StepComponentStop)},
		{Name: StepRegistryDeregister, Fn: mark(StepRegistryDeregister)},
		{Name: StepTracingShutdown, Fn: mark(StepTracingShutdown)},
		{Name: StepLoggerClose, Fn: mark(StepLoggerClose)},
	}
}

// TestShutdown 断言退出链顺序红线（验收案例 A3）：
// registry.deregister 早于 tracing.shutdown；tracing.shutdown 早于 logger.close
// 且是最后一条冲刷类事件；会标 span 的资源（component.stop）早于 tracing.shutdown。
//
// 事件名取实现真实值：冲刷步是 tracing.shutdown（不是 tracing.flush）。
func TestShutdown(t *testing.T) {
	ev := NewEvents()
	var ran []string
	if err := Shutdown(&Env{Events: ev, Logger: nopLogger{}}, exitSteps(&ran)); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	at := func(name string) int {
		i := ev.IndexOf(name)
		if i < 0 {
			t.Fatalf("missing exit event %q in %v", name, ev.Order())
		}
		return i
	}
	// 全链严格递增：server → 组件 → 注销 → 冲刷 → 关日志。
	prev := -1
	for _, name := range []string{
		StepServerStop, StepComponentStop, StepRegistryDeregister,
		StepTracingShutdown, StepLoggerClose,
	} {
		if i := at(name); i <= prev {
			t.Fatalf("exit order broken at %q: %v", name, ev.Order())
		} else {
			prev = i
		}
	}
	// deregister 早于 flush：注销本身会标 span，冲刷之后再注销会丢最后一批 span。
	if at(StepRegistryDeregister) >= at(StepTracingShutdown) {
		t.Fatalf("registry.deregister must precede tracing.shutdown: %v", ev.Order())
	}
	// 会标 span 的资源停止均早于 flush。
	if at(StepComponentStop) >= at(StepTracingShutdown) {
		t.Fatalf("component.stop must precede tracing.shutdown: %v", ev.Order())
	}
	// flush 之后只剩 logger.close：它是最后一条冲刷类事件。
	order := ev.Order()
	if tail := order[at(StepTracingShutdown)+1:]; !reflect.DeepEqual(tail, []string{StepLoggerClose}) {
		t.Fatalf("nothing but logger.close may follow tracing.shutdown, got %v", tail)
	}
	if !reflect.DeepEqual(ran, order) {
		t.Fatalf("steps must execute in recorded order: ran %v, recorded %v", ran, order)
	}
}

// TestSetupFailure 断言 Setup 失败仍走完整退出链，且首错仍是 setup 错误
// （退出链自己的错误不吞，附在后面，不覆盖 setup 首错）。
func TestSetupFailure(t *testing.T) {
	ev := NewEvents()
	env := &Env{Events: ev, Logger: nopLogger{}}
	noop := func(*Env) error { return nil }

	setupErr := Run(env, []Step{
		{Name: StepConfigLoad, Fn: noop},
		{Name: StepTelemetryInit, Fn: noop},
		{Name: StepLoggerInit, Fn: noop},
		{Name: StepModeAssemble, Fn: noop},
		{Name: StepComponentMount, Fn: noop},
		{Name: StepSetup, Fn: func(*Env) error { return errBoom }},
	})
	if !errors.Is(setupErr, errBoom) {
		t.Fatalf("setup error must surface, got: %v", setupErr)
	}
	if ev.IndexOf(StepSetup) < 0 {
		t.Fatalf("setup must be recorded before it fails: %v", ev.Order())
	}

	// Setup 失败后调用方照走退出链（app.go Run 的做法），且其中一步失败。
	var ran []string
	steps := exitSteps(&ran)
	stopAt := 2 // component.stop
	steps[stopAt].Fn = func(*Env) error {
		ran = append(ran, StepComponentStop)
		return errStopBoom
	}
	exitErr := Shutdown(env, steps)
	if !errors.Is(exitErr, errStopBoom) {
		t.Fatalf("exit chain error must be reported, got: %v", exitErr)
	}
	// 关键：某步失败不得中断后续步骤，否则漏掉 flush 与 logger.close。
	for _, name := range []string{
		StepServerStop, StepComponentStop, StepRegistryDeregister,
		StepTracingShutdown, StepLoggerClose,
	} {
		if ev.IndexOf(name) < 0 {
			t.Fatalf("exit step %q skipped after a failing step: %v", name, ev.Order())
		}
	}
	if len(ran) != len(steps) {
		t.Fatalf("all %d exit steps must run, ran %d: %v", len(steps), len(ran), ran)
	}
	// 首错仍是 setup：退出链错误聚合在它之后。
	joined := errors.Join(setupErr, exitErr)
	if !errors.Is(joined, errBoom) || !errors.Is(joined, errStopBoom) {
		t.Fatalf("first error must stay the setup error: %v", joined)
	}
}

// TestCloseIdempotent 断言退出链幂等：第二次调用直接返回 nil，
// 退出事件序列不重复记录（与 app.go Close 的 closed 幂等语义一致）。
func TestCloseIdempotent(t *testing.T) {
	ev := NewEvents()
	env := &Env{Events: ev, Logger: nopLogger{}}
	var ran []string
	steps := exitSteps(&ran)

	if err := Shutdown(env, steps); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	before := ev.Order()
	if err := Shutdown(env, exitSteps(&ran)); err != nil {
		t.Fatalf("second shutdown must be a no-op, got: %v", err)
	}
	if after := ev.Order(); !reflect.DeepEqual(before, after) {
		t.Fatalf("exit events must not repeat: before %v, after %v", before, after)
	}
	if len(ran) != len(steps) {
		t.Fatalf("second shutdown must not re-run steps, ran %v", ran)
	}
	for _, name := range before {
		if n := countOf(before, name); n != 1 {
			t.Fatalf("exit event %q recorded %d times: %v", name, n, before)
		}
	}
}

// countOf 统计 name 在 seq 中出现次数。
func countOf(seq []string, name string) int {
	n := 0
	for _, s := range seq {
		if s == name {
			n++
		}
	}
	return n
}
