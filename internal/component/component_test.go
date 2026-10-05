package component

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gospacex/configx"
	"github.com/gospacex/logx"
	_ "github.com/gospacex/logx/zap" // 注册 zap provider：测试必须用真实日志器
)

// warnLogger 捕获 Warn 文案供降级断言。
// 内嵌一个**真实**日志器（而非 nil 接口）：组件会调用 InfoContext 等方法，
// 嵌入 nil 接口会 panic（实测踩过）。
type warnLogger struct {
	logx.Logger
	warns []string
}

func (w *warnLogger) Warn(msg string, fields ...logx.Field) {
	w.warns = append(w.warns, msg+" "+fmt.Sprint(fields))
}
func (w *warnLogger) Error(msg string, fields ...logx.Field) {}
func (w *warnLogger) joined() string                         { return strings.Join(w.warns, "\n") }

// newTestLogger 构造一个只输出 error 级别的真实日志器（保持测试输出干净）。
func newTestLogger(t *testing.T) logx.Logger {
	t.Helper()
	l, err := logx.New(logx.Config{
		Level: "error", Format: "console", Output: "stdout", ServiceName: "component-test",
	})
	if err != nil {
		t.Fatalf("build test logger: %v", err)
	}
	return l
}

// newTestEnv 从内联 yaml 构造 Env（不依赖外部底座）。
func newTestEnv(t *testing.T, yaml string) (*Env, *warnLogger) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := configx.ReadConfig(configx.ConfigOptions{ConfigFile: path, ConfigType: "yaml"})
	if err != nil {
		t.Fatal(err)
	}
	w := &warnLogger{Logger: newTestLogger(t)}
	return &Env{
		Cfg: c, ServiceName: "srv-order", Logger: w,
		TraceOn: map[string]bool{}, Strict: map[string]bool{},
	}, w
}

const mqYAML = `
service: {name: srv-order, mode: srv}
xmiddleware:
  mqx:
    driver: kafka
    instances:
      srv-order:
        brokers: ["127.0.0.1:9092"]
`

// D1：trace.on.mq=false 时 producer 仍被构造（只不产 span）。
// 这是历史 bug 的回归网：曾把该开关当构造开关，producer 变 nil，
// outbox 静默堆积到 retry_count=10 才放弃。
func TestTraceOffStillConstructsMQ(t *testing.T) {
	env, _ := newTestEnv(t, mqYAML)
	env.TraceOn["mq"] = false
	b := Lookup("mq")
	if b == nil {
		t.Fatal("mq builder not registered")
	}
	if err := b(env); err != nil {
		t.Fatalf("mq must be constructed even with trace off: %v", err)
	}
	if env.MQ == nil {
		t.Fatal("MQ must not be nil when trace.on.mq=false (D1)")
	}
	if len(env.Stops()) == 0 {
		t.Fatal("mq must register a stop func for the reverse exit chain")
	}
}

const cacheYAML = `
service: {name: srv-order, mode: srv}
xmiddleware:
  cachex:
    driver: redis
    instances:
      srv-order: {addr: "127.0.0.1:1"}
`

// D2：cache 失败默认降级（字段 nil + Warn 含后果文案）
func TestCacheFailureDegradesByDefault(t *testing.T) {
	env, w := newTestEnv(t, cacheYAML)
	if err := Lookup("cache")(env); err != nil {
		t.Fatalf("default strict=false must not fail: %v", err)
	}
	if env.Cache != nil {
		t.Fatal("cache must be nil in degraded state")
	}
	if !strings.Contains(w.joined(), "缓存穿透") {
		t.Fatalf("warn must carry consequence, got: %q", w.joined())
	}
}

const dbYAML = `
service: {name: srv-order, mode: srv}
xmiddleware:
  sqlx:
    driver: gorm
    databases:
      srv-order: {dsn: "bad@tcp(127.0.0.1:1)/nodb"}
`

// strict.db=true 时失败即退（错误带 "init db" 上下文）。
func TestStrictTrueFailsFast(t *testing.T) {
	env, _ := newTestEnv(t, dbYAML)
	env.Strict["db"] = true
	err := Lookup("db")(env)
	if err == nil || !strings.Contains(err.Error(), "init db") {
		t.Fatalf("strict.db=true must return 'init db' error, got: %v", err)
	}
}

// D2 的对称面：strict.db 缺省时降级（db 字段 nil + Warn 含后果），启动继续。
func TestDBFailureDegradesByDefault(t *testing.T) {
	env, w := newTestEnv(t, dbYAML)
	if err := Lookup("db")(env); err != nil {
		t.Fatalf("default strict=false must not fail: %v", err)
	}
	if env.DB != nil {
		t.Fatal("db must be nil in degraded state")
	}
	if !strings.Contains(w.joined(), "5xx") {
		t.Fatalf("warn must carry consequence, got: %q", w.joined())
	}
}

// presence-based：未配置任何中间件段时，六类组件都不构造、不报错、不告警。
func TestPresenceBasedSkip(t *testing.T) {
	env, w := newTestEnv(t, "service: {name: srv-order, mode: srv}\n")
	for _, name := range Names() {
		if err := Lookup(name)(env); err != nil {
			t.Fatalf("component %q must be skipped silently when unconfigured: %v", name, err)
		}
	}
	if env.DB != nil || env.Cache != nil || env.MQ != nil || env.Registry != nil || env.Search != nil || env.Mongo != nil {
		t.Fatal("no component must be constructed without config sections")
	}
	if w.joined() != "" {
		t.Fatalf("unconfigured components must not warn, got: %q", w.joined())
	}
}

// 六类组件必须都已注册（D1/验收案例 T2–T4 的前提）。
func TestAllSixComponentsRegistered(t *testing.T) {
	want := []string{"db", "cache", "mq", "registry", "search", "mongodb"}
	got := Names()
	for _, w := range want {
		if Lookup(w) == nil {
			t.Fatalf("component %q not registered, registered=%v", w, got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("registered = %v, want exactly %v", got, want)
	}
}

// Mount 遇到未注册组件名必须报错并列出已注册项（拼错名字不允许静默通过）。
func TestMountUnknownComponentFails(t *testing.T) {
	env, _ := newTestEnv(t, "service: {name: srv-order, mode: srv}\n")
	err := Mount(env, []string{"db", "sqlx"})
	if err == nil {
		t.Fatal("unknown component must be rejected")
	}
	if !strings.Contains(err.Error(), "sqlx") || !strings.Contains(err.Error(), "db") {
		t.Fatalf("error must name the unknown component and list registered ones, got: %v", err)
	}
}

// traceflag 全局总闸关闭时，traceOn 必须为 false（组件级 true 也不能绕过总闸）。
func TestTraceOnRespectsGlobalSwitch(t *testing.T) {
	env, _ := newTestEnv(t, "service: {name: srv-order, mode: srv}\n")
	env.TraceOn["cache"] = true
	if !traceOn(env, "cache") {
		t.Fatal("component-level trace on must yield true when the global switch is on")
	}
	env.TraceOn["cache"] = false
	if traceOn(env, "cache") {
		t.Fatal("component-level trace off must yield false")
	}
}
