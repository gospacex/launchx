// Package pipeline 定义 launchx 的固定启动骨架与顺序断言基础设施。
//
// 骨架顺序是框架契约，业务只能挂 Setup 回调，不能插队：
//
//	config.load → telemetry.init → logger.init → mode.assemble → component.mount → registry.register → setup
//
// 顺序不是风格问题：telemetry 必须最早，否则 webx/组件会拿到 noop tracer
// （详见 internal/telemetry 包注释）。
package pipeline

import (
	"context"
	"fmt"

	"github.com/gospacex/logx"
	"github.com/gospacex/otelx"
)

// 启动骨架步骤名（顺序断言与日志都用这里的常量，避免散落字符串字面量）。
const (
	StepConfigLoad       = "config.load"
	StepTelemetryInit    = "telemetry.init"
	StepLoggerInit       = "logger.init"
	StepModeAssemble     = "mode.assemble"
	StepComponentMount   = "component.mount"
	StepRegistryRegister = "registry.register"
	StepSetup            = "setup"
)

// 反序退出链步骤名（显式序列，不依赖 defer）。
//
// StepSetupStop 是「业务 OnStop 组」的事件名。它与下面几个**不同层**：其余六个
// 是基础设施的固定步骤，业务回调是 N 条（每条各自 migrating runStop 有界执行），
// 这里只为**整组**记一个事件，故单独给一个常量，不要在 App.Close 里写字面量。
const (
	StepSetupStop          = "setup.stop"
	StepServerStop         = "server.stop"
	StepMetricsListener    = "metrics.listener"
	StepComponentStop      = "component.stop"
	StepRegistryDeregister = "registry.deregister"
	StepTracingShutdown    = "tracing.shutdown"
	StepMetricsShutdown    = "metrics.shutdown"
	StepLoggerClose        = "logger.close"
)

// Step 骨架步骤。Fn 返回 error 即中止后续步骤（地基失败不降级）。
type Step struct {
	Name string
	Fn   func(*Env) error
}

// Env 骨架执行环境。
//
// 它只承载「跨步骤传递的句柄」，不回指根包 App，避免 import 环
// （组件包与 mode 包都面向 Env 编程）。
type Env struct {
	Events *Events
	Logger logx.Logger
	Tracer otelx.Provider
	Ctx    context.Context

	// shutdownDone 退出链已走过一轮（Shutdown 幂等的判据，见 shutdown.go）。
	shutdownDone bool
}

// Run 按序执行骨架步骤；任一步失败立即返回，后续步骤不再执行。
func Run(e *Env, steps []Step) error {
	if e == nil {
		return fmt.Errorf("pipeline: env is nil")
	}
	if e.Events == nil {
		e.Events = NewEvents()
	}
	if e.Ctx == nil {
		e.Ctx = context.Background()
	}
	for _, s := range steps {
		e.Events.Record(s.Name)
		if s.Fn == nil {
			return fmt.Errorf("pipeline step %q: nil func", s.Name)
		}
		if err := s.Fn(e); err != nil {
			return fmt.Errorf("pipeline step %q: %w", s.Name, err)
		}
	}
	return nil
}
