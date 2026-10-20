package launchx_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gospacex/launchx"
)

// 验收案例 O2 / 3.12：APP_DEBUG=1 启动 srv 档，debug server 仅在回环监听 6061，
// `/debug/pprof/` 返回 pprof 索引页；yaml `debug.enabled` 置位等效。
//
// 用最小化 srv 配置（service.mode=srv，不配任何中间件段）启动，做到不依赖外部
// 底座（无 db/cache/mq/consul）—— debug 是观测侧伪组件，与业务分层解耦。
func TestDebugServerBootsWithSrvMode(t *testing.T) {
	const debugPort = 6061
	cfg := "service:\n" +
		"  name: debug-srv\n" +
		"  mode: srv\n" +
		"  addr: \"127.0.0.1:0\"\n" +
		"trace:\n" +
		"  enabled: false\n" +
		"  endpoint: \"127.0.0.1:4318\"\n" +
		"debug:\n" +
		"  enabled: true\n" +
		"  port: " + itoa(debugPort) + "\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// APP_DEBUG=1 作为等价通道一并验证（yaml 已显式 enabled，环境变量不应反向关掉）。
	t.Setenv("APP_DEBUG", "1")

	a, err := launchx.Bootstrap(path)
	if err != nil {
		t.Fatalf("bootstrap srv mode with debug must succeed: %v", err)
	}
	// 不调 Run：debug server 在组件挂载阶段（Bootstrap 内）即已起，业务是否起服不影响验证。
	defer func() {
		if cerr := a.Close(); cerr != nil {
			t.Logf("close: %v", cerr)
		}
	}()

	code, body, perr := probe(t, debugPort)
	if perr != nil {
		t.Fatalf("debug server must be reachable on loopback after bootstrap: %v", perr)
	}
	if code != http.StatusOK {
		t.Fatalf("debug /debug/pprof/ must return 200, got %d", code)
	}
	if !strings.Contains(body, "pprof") {
		t.Fatalf("debug index page must mention pprof, got: %q", body)
	}

	// 退出链必须关停 debug server：Close 之后端口不可达（钩子在 component.stop 相，先于 logger）。
	if cerr := a.Close(); cerr != nil {
		t.Logf("close: %v", cerr)
	}
	// 复用 a 的 Close 是幂等的，但本例已在 defer 中 Close；此处仅再做一次探测确认端口已释放。
	time.Sleep(150 * time.Millisecond)
	if _, _, err2 := probe(t, debugPort); err2 == nil {
		t.Fatal("debug server must be unreachable after App.Close (exit-chain stop hook)")
	}
}

// probe 探测 127.0.0.1:<port>/debug/pprof/。
func probe(t *testing.T, port int) (int, string, error) {
	t.Helper()
	c := &http.Client{Timeout: 800 * time.Millisecond}
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
