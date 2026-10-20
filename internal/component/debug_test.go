package component

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 探测 127.0.0.1:<port>/debug/pprof/ 是否可达，返回 (statusCode, body, err)。
func probeDebugPort(t *testing.T, port int) (int, string, error) {
	t.Helper()
	c := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get("http://127.0.0.1:" + itoa(port) + "/debug/pprof/")
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// O1-1：debug.enabled=false 时不起 server、不留关闭钩子、不告警。
func TestDebugDisabledDoesNotStartServer(t *testing.T) {
	env, w := newTestEnv(t, "service: {name: srv-order, mode: srv}\ndebug: {enabled: false}\n")
	if err := Lookup("debug")(env); err != nil {
		t.Fatalf("debug must not fail when disabled: %v", err)
	}
	if len(env.NamedStops()) != 0 {
		t.Fatalf("disabled debug must not register a stop hook, got %v", env.NamedStops())
	}
	if w.joined() != "" {
		t.Fatalf("disabled debug must not warn, got: %q", w.joined())
	}
}

// O1-2：未配置任何 debug 段、且宿主机无 APP_DEBUG=1 时同样静默跳过。
// （本用例不依赖宿主环境：newTestEnv 不设置 APP_DEBUG，故默认关。）
func TestDebugAbsentConfigSkips(t *testing.T) {
	env, w := newTestEnv(t, "service: {name: srv-order, mode: srv}\n")
	if err := Lookup("debug")(env); err != nil {
		t.Fatalf("absent debug config must skip silently: %v", err)
	}
	if len(env.NamedStops()) != 0 {
		t.Fatal("absent debug config must not register a stop hook")
	}
	if w.joined() != "" {
		t.Fatalf("absent debug config must not warn, got: %q", w.joined())
	}
}

// O1-3：debug.enabled=true 时仅回环监听 6060，并返回 pprof 索引页；
// 退出钩子必须存在（落在组件停止相，先于 logger 关闭）。
func TestDebugEnabledStartsLoopbackServer(t *testing.T) {
	env, _ := newTestEnv(t, "service: {name: srv-order, mode: srv}\ndebug: {enabled: true}\n")
	if err := Lookup("debug")(env); err != nil {
		t.Fatalf("debug must start when enabled: %v", err)
	}
	if len(env.NamedStops()) == 0 {
		t.Fatal("enabled debug must register a stop hook")
	}
	// 钩子名为 "debug"（App.Close 据此把它归入 component.stop 相）。
	found := false
	for _, s := range env.NamedStops() {
		if s.Name == "debug" {
			found = true
		}
	}
	if !found {
		t.Fatalf("debug stop hook must be named 'debug', got %v", env.NamedStops())
	}

	// 回环可达：索引页含 "pprof"。
	code, body, err := probeDebugPort(t, 6060)
	if err != nil {
		t.Fatalf("debug server must be reachable on loopback: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("debug /debug/pprof/ must return 200, got %d", code)
	}
	if !strings.Contains(body, "pprof") {
		t.Fatalf("debug index page must mention pprof, got: %q", body)
	}

	// 退出钩子必须能让 server 真正停掉（证明它接进了退出链）。
	_ = env.NamedStops()[len(env.NamedStops())-1].Fn()
	time.Sleep(100 * time.Millisecond)
	if _, _, err := probeDebugPort(t, 6060); err == nil {
		t.Fatal("debug server must be unreachable after its stop hook runs")
	}
}

// O1-4：环境变量 APP_DEBUG=1 即使无 yaml 也能开启（等价通道）。
func TestDebugEnvVarEnablesServer(t *testing.T) {
	t.Setenv("APP_DEBUG", "1")
	env, _ := newTestEnv(t, "service: {name: srv-order, mode: srv}\n")
	if err := Lookup("debug")(env); err != nil {
		t.Fatalf("APP_DEBUG=1 must enable debug: %v", err)
	}
	if len(env.NamedStops()) == 0 {
		t.Fatal("APP_DEBUG=1 must register a stop hook")
	}
	code, body, err := probeDebugPort(t, 6060)
	if err != nil || code != http.StatusOK || !strings.Contains(body, "pprof") {
		t.Fatalf("debug server must serve pprof index via APP_DEBUG=1 (code=%d err=%v)", code, err)
	}
}

// O1-5：yaml 显式 debug.enabled=false 时，即使宿主机 APP_DEBUG=1 也不应被偷偷打开。
func TestDebugYAMLOverridesEnvVar(t *testing.T) {
	t.Setenv("APP_DEBUG", "1")
	env, _ := newTestEnv(t, "service: {name: srv-order, mode: srv}\ndebug: {enabled: false}\n")
	if err := Lookup("debug")(env); err != nil {
		t.Fatalf("debug must not fail: %v", err)
	}
	if len(env.NamedStops()) != 0 {
		t.Fatal("explicit debug.enabled:false must win over APP_DEBUG=1")
	}
}

// O2：端口冲突 → 降级（Warn 含后果）+ 带病运行，不返回 error、不留钩子。
// （6060 已被本用例预占，buildDebug 必须落到降级分支。）
func TestDebugPortConflictDegrades(t *testing.T) {
	// 预占 6060，模拟端口被占。
	ln, err := net.Listen("tcp", "127.0.0.1:6060")
	if err != nil {
		t.Skipf("cannot pre-bind 6060 to exercise conflict path: %v", err)
	}
	defer ln.Close()

	env, w := newTestEnv(t, "service: {name: srv-order, mode: srv}\ndebug: {enabled: true}\n")
	if err := Lookup("debug")(env); err != nil {
		t.Fatalf("port conflict must NOT fail bootstrap: %v", err)
	}
	if len(env.NamedStops()) != 0 {
		t.Fatal("port conflict must not register a debug stop hook")
	}
	if !strings.Contains(w.joined(), "debug") || !strings.Contains(w.joined(), "pprof") {
		t.Fatalf("port conflict must warn with consequence, got: %q", w.joined())
	}
}
