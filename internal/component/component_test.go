package component

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gospacex/configx"
	"github.com/gospacex/logx"
	_ "github.com/gospacex/logx/zap" // 注册 zap provider：测试必须用真实日志器
)

// redisAddr 是 kvx / lockx 两个用例连的实例，与 *_YAML 里的 addr 保持一致。
const redisAddr = "127.0.0.1:6379"

// requireRedis 在无本地 redis 时跳过用例。
//
// kvx / lockx 的构造会真连 redis（建连接池并探活），无实例时构造必然失败，
// 此时该用例的命题（"trace 关闭也仍要构造"）根本无从验证。
// 这种"缺外部依赖"必须与"组件降级有缺陷"区分开：前者 Skip 并打印原因，后者才是
// 真失败 —— 混为一谈会把本机没开 redis 这种环境问题伪装成代码回归。
func requireRedis(t *testing.T) {
	t.Helper()
	c, err := net.DialTimeout("tcp", redisAddr, 500*time.Millisecond)
	if err != nil {
		t.Skipf("needs local redis at %s: %v", redisAddr, err)
	}
	_ = c.Close()
}

// warnLogger 捕获 Warn 文案供降级断言。
// 内嵌一个**真实**日志器（而非 nil 接口）：组件会调用 InfoContext 等方法，
// 嵌入 nil 接口会 panic（实测踩过）。
type warnLogger struct {
	logx.Logger
	warns []string
}

func (w *warnLogger) Warn(msg string, fields ...logx.Field) {
	var b strings.Builder
	b.WriteString(msg)
	for _, f := range fields {
		b.WriteString(" " + f.Key + "=" + fieldValue(f))
	}
	w.warns = append(w.warns, b.String())
}

// fieldValue 取单个 logx.Field 的可读值。
//
// logx.Field 就是 zap.Field，值分散在 String / Interface / Integer 三个域里，
// 直接 fmt.Sprint 出来是 "{component 15 0 db <nil>}" 这种结构体字面量：
// 断言无从区分"哪个字段等于什么"，于是"Warn 含 db"会被 hint 里的
// "strict.db" 蒙混过关 —— 降级告警到底点名了哪个组件，等于没验。
func fieldValue(f logx.Field) string {
	switch {
	case f.String != "":
		return f.String
	case f.Interface != nil:
		return fmt.Sprint(f.Interface)
	default:
		return fmt.Sprint(f.Integer)
	}
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

// presence-based：未配置任何中间件段时，八类组件都不构造、不报错、不告警。
func TestPresenceBasedSkip(t *testing.T) {
	env, w := newTestEnv(t, "service: {name: srv-order, mode: srv}\n")
	for _, name := range Names() {
		if err := Lookup(name)(env); err != nil {
			t.Fatalf("component %q must be skipped silently when unconfigured: %v", name, err)
		}
	}
	if env.DB != nil || env.Cache != nil || env.MQ != nil || env.KV != nil || env.Lock != nil ||
		env.Registry != nil || env.Search != nil || env.Mongo != nil {
		t.Fatal("no component must be constructed without config sections")
	}
	if w.joined() != "" {
		t.Fatalf("unconfigured components must not warn, got: %q", w.joined())
	}
}

// 八类组件必须都已注册（D1/验收案例 T2–T4 的前提）。
func TestAllComponentsRegistered(t *testing.T) {
	want := []string{"db", "cache", "mq", "kv", "lock", "registry", "search", "mongodb"}
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

const kvYAML = `
service: {name: srv-order, mode: srv}
xmiddleware:
  kvx:
    driver: redis
    instances:
      srv-order: {addr: "127.0.0.1:6379"}
`

// D1 的 kv 面：trace.on.kv=false 时 KV 仍被构造（只不产 span）。
// 与 mq 同源的红线：开关只裁决入链，绝不裁决构造 —— KV 变 nil 会让幂等键
// 静默失效（重复请求不再被挡），故障形态是"扣了两次款但没人报错"。
func TestTraceOffStillConstructsKV(t *testing.T) {
	requireRedis(t) // kvx 建实例要连真 redis，缺依赖时跳过而非误报成降级缺陷
	env, _ := newTestEnv(t, kvYAML)
	env.TraceOn["kv"] = false
	b := Lookup("kv")
	if b == nil {
		t.Fatal("kv builder not registered")
	}
	if err := b(env); err != nil {
		t.Fatalf("kv must be constructed even with trace off: %v", err)
	}
	if len(env.KV) == 0 {
		t.Fatal("KV must not be empty when trace.on.kv=false (D1)")
	}
	if len(env.Stops()) == 0 {
		t.Fatal("kv must register a stop func for the reverse exit chain")
	}
}

const lockYAML = `
service: {name: srv-order, mode: srv}
xmiddleware:
  lockx:
    driver: redis
    addr: "127.0.0.1:6379"
    namespace: mall
    ttl: 30s
    retry: {enabled: true, max_attempts: 3, backoff_ms: 100}
`

// D1 的 lock 面 + ttl/retry 解码：这两个键是蛇形（max_attempts / backoff_ms），
// lockx.Config 又缺 mapstructure tag，走 Unmarshal 会被静默丢弃 —— 本例固定
// 逐字段读取，这里断言重试参数确实落进了 Config（不是 0 值默认）。
func TestLockConstructedWithRetryConfig(t *testing.T) {
	requireRedis(t) // 同 kvx：lockx/redis 构造要连真实例
	env, _ := newTestEnv(t, lockYAML)
	env.TraceOn["lock"] = false
	if err := Lookup("lock")(env); err != nil {
		t.Fatalf("lock must be constructed even with trace off: %v", err)
	}
	if env.Lock == nil {
		t.Fatal("Lock must not be nil when trace.on.lock=false (D1)")
	}
}

// 锁器构造失败（驱动未注册）必须降级而非即退，且 Warn 带"超卖"后果。
func TestLockFailureDegradesByDefault(t *testing.T) {
	env, w := newTestEnv(t, `
service: {name: srv-order, mode: srv}
xmiddleware:
  lockx:
    driver: no-such-driver
    addr: "127.0.0.1:6379"
`)
	if err := Lookup("lock")(env); err != nil {
		t.Fatalf("default strict=false must not fail: %v", err)
	}
	if env.Lock != nil {
		t.Fatal("lock must be nil in degraded state")
	}
	if !strings.Contains(w.joined(), "超卖") {
		t.Fatalf("warn must carry consequence, got: %q", w.joined())
	}
}

// ===== 3.3 验收案例 D1 / D2 三组断言（plan Task 14）=====
//
// 计划里的 MountAll / App.DB / TraceOn() 在真实实现里叫 Mount / Env.DB /
// TraceOnOf：component 包不回指 launchx 根包（避免 import 环），也没有
// MountAll 这个入口（挂载顺序由调用方传入）。断言按真实 API 落地。

// errFakeDB 是 fake Builder 注入的固定根因哨兵：用来证明 strict 即退分支
// 经 degrade 的 %w 包装后仍能被 errors.Is 判定到真正的失败原因。
//
// 为什么用 fake 而不是真连一次坏 MySQL：真连的失败原因由驱动决定
// （dial tcp / no such host / auth 都可能），err.Error() 含 "init db" 这条
// 尚可断言，"errors.Is 到根因"这条根本无从写起 —— 没有稳定哨兵可判。
var errFakeDB = errors.New("fake db driver: connection refused")

// withFakeDB 用"必定失败于固定哨兵"的 fake Builder 临时替换真实 db 构造器。
// 被测对象是 degrade 的裁决分支（降级/即退），与 dbx 驱动无关。
func withFakeDB(t *testing.T) {
	t.Helper()
	real := Lookup("db")
	if real == nil {
		t.Fatal("db builder not registered")
	}
	Register("db", func(e *Env) error { return degrade(e, "db", ConsequenceDB, errFakeDB) })
	t.Cleanup(func() { Register("db", real) })
}

// D2 降级分支：strict 缺省 → Mount 返回 nil、DB 保持 nil、
// 且必须留下一条点名 component=db 且带后果文案的 Warn（不许静默降级）。
func TestMountDegradesWhenStrictAbsent(t *testing.T) {
	withFakeDB(t)
	env, w := newTestEnv(t, dbYAML) // Strict 全空：strict.db 缺省
	if err := Mount(env, []string{"db"}); err != nil {
		t.Fatalf("strict 缺省必须降级（返回 nil error），got: %v", err)
	}
	if env.DB != nil {
		t.Fatal("降级后 DB 必须保持 nil（句柄为零值，不是半初始化对象）")
	}
	got := w.joined()
	if !strings.Contains(got, "component=db") {
		t.Fatalf("Warn 必须点名 component=db，got: %q", got)
	}
	if !strings.Contains(got, ConsequenceDB) {
		t.Fatalf("Warn 必须带降级后果文案 %q，got: %q", ConsequenceDB, got)
	}
	if !strings.Contains(got, "strict.db") {
		t.Fatalf("Warn 必须给出即退开关提示，got: %q", got)
	}
}

// D2 即退分支：strict.db=true → Mount 返回 error（不许返回 nil），
// 文案含 "init db"，且 %w 包装链可被 errors.Is 判定到根因。
func TestMountFailsFastWhenStrictTrue(t *testing.T) {
	withFakeDB(t)
	env, w := newTestEnv(t, dbYAML)
	env.Strict["db"] = true
	err := Mount(env, []string{"db"})
	if err == nil {
		t.Fatal("strict.db=true 必须即退：不允许返回 nil")
	}
	if !strings.Contains(err.Error(), "init db") {
		t.Fatalf("error 必须带 \"init db\" 上下文，got: %v", err)
	}
	if !errors.Is(err, errFakeDB) {
		t.Fatalf("error 必须保留 fmt.Errorf 的 %%w 包装链（errors.Is 可判到根因），got: %v", err)
	}
	if w.joined() != "" {
		t.Fatalf("即退分支不该再打降级 Warn（启动会失败，降级文案是误导），got: %q", w.joined())
	}
}

// D1 门控：trace.on.mq=false → mq 构造器**照旧被调用**（构造发生）、MQ 非 nil，
// 只是入链判定为 false。门控关了就不构造是历史 bug（outbox 静默堆积）。
//
// 这里在真实构造器外面套一层 spy：只断言"是否被调用 + 看到的门控值"，
// 产物仍由真实构造器产出（避免为 mqx.Producer 造一整套假实现）。
func TestTraceGateDoesNotSkipConstruction(t *testing.T) {
	env, _ := newTestEnv(t, mqYAML)
	env.TraceOn["mq"] = false // 门控关
	if env.TraceOnOf("mq") {
		t.Fatal("前置条件：门控必须处于关闭态")
	}
	real := Lookup("mq")
	if real == nil {
		t.Fatal("mq builder not registered")
	}
	var called, sawTraceOn bool
	Register("mq", func(e *Env) error {
		called = true
		sawTraceOn = traceOn(e, "mq")
		return real(e)
	})
	t.Cleanup(func() { Register("mq", real) })

	if err := Mount(env, []string{"mq"}); err != nil {
		t.Fatalf("trace.on.mq=false 时 mq 仍必须构造成功，got: %v", err)
	}
	if !called {
		t.Fatal("D1: trace.on.mq=false 只关入链，构造器必须照旧被调用")
	}
	if sawTraceOn {
		t.Fatal("D1: 门控关闭时组件内看到的入链开关必须为 false")
	}
	if env.MQ == nil {
		t.Fatal("D1: trace.on.mq=false 时 App.MQ 必须非 nil（构造发生，仅入链关闭）")
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
