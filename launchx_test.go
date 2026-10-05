package launchx

import (
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
