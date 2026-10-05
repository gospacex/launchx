package launchx

import (
	"errors"
	"strings"
	"testing"

	"github.com/gospacex/launchx/internal/pipeline"
)

func TestBootstrapReturnsApp(t *testing.T) {
	app, err := Bootstrap("testdata/monolith.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if app == nil {
		t.Fatal("app is nil")
	}
	if app.Mode() != ModeMonolith {
		t.Fatalf("mode = %q, want %q", app.Mode(), ModeMonolith)
	}
	if app.Config() == nil || app.Config().GetString("service.name") != "mono" {
		t.Fatal("config handle not wired")
	}
	if err := app.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Close 幂等
	if err := app.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// TestBootstrapIncompleteConfigRejected 断言地基缺失即退：
// 缺 service.name（telemetry 资源属性依赖）或缺 service.mode（四档装配无从选择），
// 都不允许带病启动。注意 testdata/empty.yaml 虽名为 empty，实为一份最小合法配置。
func TestBootstrapIncompleteConfigRejected(t *testing.T) {
	if app, err := Bootstrap("testdata/noservice.yaml"); err == nil {
		_ = app.Close()
		t.Fatal("missing service.name must be rejected (地基失败不降级)")
	}
	if app, err := Bootstrap("testdata/nomode.yaml"); err == nil {
		_ = app.Close()
		t.Fatal("missing service.mode must be rejected (地基失败不降级)")
	}
}

// TestBootstrapMissingPathRejected 断言路径不存在即退（configx 会静默返回空配置，
// 故由 localLoader 做 os.Stat 预检）。
func TestBootstrapMissingPathRejected(t *testing.T) {
	if _, err := Bootstrap("testdata/not-exist.yaml"); err == nil {
		t.Fatal("missing config path must be rejected")
	}
	if _, err := Bootstrap(""); err == nil {
		t.Fatal("empty config path must be rejected")
	}
}

// TestBootstrapStartupOrder 断言固定骨架顺序：telemetry 必须早于组件挂载。
func TestBootstrapStartupOrder(t *testing.T) {
	app, err := Bootstrap("testdata/monolith.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	defer func() { _ = app.Close() }()

	order := app.Events().Order()
	want := []string{
		pipeline.StepConfigLoad,
		pipeline.StepTelemetryInit,
		pipeline.StepLoggerInit,
		pipeline.StepModeAssemble,
		pipeline.StepComponentMount,
		pipeline.StepRegistryRegister,
	}
	for _, w := range want {
		if app.Events().IndexOf(w) < 0 {
			t.Fatalf("missing event %q in %v", w, order)
		}
	}
	if app.Events().IndexOf(pipeline.StepTelemetryInit) >= app.Events().IndexOf(pipeline.StepComponentMount) {
		t.Fatalf("telemetry must precede components: %v", order)
	}
	// 顺序必须严格等于定义序（骨架不可打乱）
	if len(order) != len(want) {
		t.Fatalf("recorded %d steps, want %d: %v", len(order), len(want), order)
	}
	for i, w := range want {
		if order[i] != w {
			t.Fatalf("step %d = %q, want %q: %v", i, order[i], w, order)
		}
	}
}

// TestBootstrapBadModeRejected 断言非法形态即退，且错误列出全部合法取值。
func TestBootstrapBadModeRejected(t *testing.T) {
	_, err := Bootstrap("testdata/badmode.yaml")
	if err == nil {
		t.Fatal("bad mode must be rejected")
	}
	for _, w := range Modes() {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("error must list %q, got: %v", w, err)
		}
	}
}

// TestParseMode 断言四档合法 + 网格不入档（T8：mesh 不是 mode）。
func TestParseMode(t *testing.T) {
	for _, m := range Modes() {
		got, err := ParseMode(m)
		if err != nil {
			t.Fatalf("ParseMode(%q): %v", m, err)
		}
		if string(got) != m {
			t.Fatalf("ParseMode(%q) = %q", m, got)
		}
	}
	if _, err := ParseMode("mesh"); err == nil {
		t.Fatal("mesh must NOT be a valid mode (网格与微服共用代码路径)")
	}
	if _, err := ParseMode(""); err == nil {
		t.Fatal("empty mode must be rejected")
	}
}

func TestOnSetupRegistered(t *testing.T) {
	app, err := Bootstrap("testdata/monolith.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	defer func() { _ = app.Close() }()
	app.OnSetup(func(a *App) error { return nil })
	if len(app.setups) != 1 {
		t.Fatalf("setups = %d, want 1", len(app.setups))
	}
}

// TestExitOrderReverseChain 断言反序退出链的显式顺序（A3）：
// 业务回调 → server.stop → component.stop → registry.deregister
// → tracing.shutdown（冲刷）→ logger.close。
// 关键点：tracing.shutdown 必须晚于所有组件停止（否则最后的 span 丢失），
// logger.close 必须最后（前面的失败还需要它打日志）。
func TestExitOrderReverseChain(t *testing.T) {
	app, err := Bootstrap("testdata/monolith.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	app.OnStop(func() error { return nil })
	if err := app.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	want := []string{
		"setup.stop",
		pipeline.StepServerStop,
		pipeline.StepComponentStop,
		pipeline.StepRegistryDeregister,
		pipeline.StepTracingShutdown,
		pipeline.StepLoggerClose,
	}
	prev := -1
	for _, name := range want {
		i := app.Events().IndexOf(name)
		if i < 0 {
			t.Fatalf("missing exit event %q in %v", name, app.Events().Order())
		}
		if i <= prev {
			t.Fatalf("exit order broken at %q: %v", name, app.Events().Order())
		}
		prev = i
	}
}

// TestBootstrapModeAssembly 断言形态装配产物已挂到 App（monolith 档）：
// 有 webx 引擎、无 RPC 服务端选项、无下游 RPC 客户端工厂。
func TestBootstrapModeAssembly(t *testing.T) {
	app, err := Bootstrap("testdata/monolith.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	defer func() { _ = app.Close() }()
	if app.Engine() == nil {
		t.Fatal("monolith must assemble a webx engine")
	}
	if app.RPCServerOptions() != nil {
		t.Fatal("monolith must not build rpc server options")
	}
	if app.RPCServer() != nil {
		t.Fatal("monolith must not hold an rpc server")
	}
	if _, err := app.RPCClientOptions("downstream"); err == nil {
		t.Fatal("monolith has no rpc client factory")
	}
}

// TestRunSetupFailureRunsExitChain 断言 Setup 失败走完整反序退出链
// （spec: bootstrap-pipeline → "Setup 失败走完整退出"；tasks.md 1.7）：
// Run 返回该 error **之前**必须先把已初始化的资源按退出链收掉——否则
// tracer 不冲刷（丢最后一批 span）、logger 不关（丢缓冲日志），
// 而调用方拿到 error 时完全看不出资源还悬着。
func TestRunSetupFailureRunsExitChain(t *testing.T) {
	app, err := Bootstrap("testdata/monolith.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	boom := errors.New("setup boom")
	app.OnSetup(func(a *App) error { return boom })

	err = app.Run()
	if err == nil {
		t.Fatal("Run must return the setup error")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("Run must return the setup error, got: %v", err)
	}
	for _, name := range []string{
		pipeline.StepServerStop,
		pipeline.StepComponentStop,
		pipeline.StepRegistryDeregister,
		pipeline.StepTracingShutdown,
		pipeline.StepLoggerClose,
	} {
		if app.Events().IndexOf(name) < 0 {
			t.Fatalf("exit step %q not recorded, order = %v", name, app.Events().Order())
		}
	}
}

// TestBootstrapStrictComponentFailsFast 断言 strict.<name>=true 时组件失败
// 会冒泡为 Bootstrap 错误（错误带 "init db" 上下文），而非静默降级。
func TestBootstrapStrictComponentFailsFast(t *testing.T) {
	_, err := Bootstrap("testdata/strict-db.yaml")
	if err == nil {
		t.Fatal("strict.db=true with a bad DSN must fail bootstrap")
	}
	if !strings.Contains(err.Error(), "init db") {
		t.Fatalf("error must carry 'init db' context, got: %v", err)
	}
}

// TestBootstrapDegradeKeepsRunning 断言默认（strict 缺省 false）时组件失败只降级：
// Bootstrap 成功、句柄为空、进程可继续。
func TestBootstrapDegradeKeepsRunning(t *testing.T) {
	app, err := Bootstrap("testdata/degrade-db.yaml")
	if err != nil {
		t.Fatalf("default must degrade, not fail: %v", err)
	}
	defer func() { _ = app.Close() }()
	if app.DB() != nil {
		t.Fatal("db must be nil in degraded state")
	}
}
