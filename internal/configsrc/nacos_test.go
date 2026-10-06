package configsrc

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gospacex/configx"
)

func TestNacosDataIDEmptyFallsBackToLocal(t *testing.T) {
	c, err := Resolve("nacos", Options{
		Path:   "testdata/local.yaml",
		Type:   "nacos",
		DataID: "",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := c.GetString("service.name"); got != "test-svc" {
		t.Fatalf("service.name = %q, want %q", got, "test-svc")
	}
}

func TestNacosUnreachableIsFatal(t *testing.T) {
	addr := unusedTCPAddr(t)
	t.Setenv("HUBX_NACOS_SERVER", addr)

	started := time.Now()
	_, err := Resolve("nacos", Options{
		Path:    "testdata/local.yaml",
		Type:    "nacos",
		DataID:  "hubx.test-svc",
		AddrEnv: "HUBX_NACOS_SERVER",
	})
	t.Logf("unreachable nacos returned in %s: %v", time.Since(started), err)
	if err == nil {
		t.Fatal("expected unreachable nacos to return error")
	}
	if !strings.Contains(err.Error(), "hubx.test-svc") {
		t.Fatalf("error %q should mention dataId %q", err, "hubx.test-svc")
	}
	if !strings.Contains(err.Error(), addr) {
		t.Fatalf("error %q should mention env address %q", err, addr)
	}
}

func TestNacosParseServerAddrs(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want []configx.NacosServerConfig
	}{
		{
			name: "单地址",
			addr: "127.0.0.1:8848",
			want: []configx.NacosServerConfig{{IP: "127.0.0.1", Port: 8848}},
		},
		{
			name: "逗号分隔多地址",
			addr: "127.0.0.1:8848, nacos.example:8849",
			want: []configx.NacosServerConfig{
				{IP: "127.0.0.1", Port: 8848},
				{IP: "nacos.example", Port: 8849},
			},
		},
		{
			name: "忽略空项和非法项",
			addr: "bad,127.0.0.1:not-a-port,:8848,127.0.0.1:70000,,127.0.0.1:8848",
			want: []configx.NacosServerConfig{{IP: "127.0.0.1", Port: 8848}},
		},
		{
			name: "全为空或非法",
			addr: " ,bad,127.0.0.1:0",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseServerAddrs(tt.addr); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseServerAddrs(%q) = %#v, want %#v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestNacosRemoteOverridesLocal(t *testing.T) {
	addr := strings.TrimSpace(os.Getenv("HUBX_NACOS_SERVER"))
	if addr == "" {
		t.Skip("HUBX_NACOS_SERVER 未设置，跳过真机 Nacos 测试")
	}
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		t.Skipf("Nacos 不可达，跳过真机测试: %v", err)
	}
	conn.Close()

	c, err := Resolve("nacos", Options{
		Path:    "testdata/local.yaml",
		Type:    "nacos",
		DataID:  "hubx.test-svc",
		AddrEnv: "HUBX_NACOS_SERVER",
	})
	if err != nil {
		t.Skipf("Nacos 服务不可用，跳过真机测试: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if got := c.GetString("service.name"); got == "" || got == "test-svc" {
		t.Fatalf("service.name = %q, want remote value overriding local %q", got, "test-svc")
	}
	if got := c.GetInt("service.port"); got != 8080 {
		t.Fatalf("service.port = %d, want local fallback %d", got, 8080)
	}
}

// TestNacosEmptyDataIDPureLocal dataId 为空（含纯空白）→ 纯本地，不触达远端。
//
// 本包没有 nacosClient/Fetcher 注入点可数调用次数，改用"不可达地址 + 地基失败即退"
// 反证：把 env 地址指向一个无人监听的端口——同一地址在 dataId 非空时必然报错
// （见 TestNacosUnreachableIsFatal），故 dataId 空时无错返回即证明未触达远端。
func TestNacosEmptyDataIDPureLocal(t *testing.T) {
	t.Setenv("HUBX_NACOS_SERVER", unusedTCPAddr(t))

	for _, dataID := range []string{"", "   "} {
		c, err := Resolve("nacos", Options{
			Path:    "testdata/local.yaml",
			Type:    "nacos",
			DataID:  dataID,
			AddrEnv: "HUBX_NACOS_SERVER",
		})
		if err != nil {
			t.Fatalf("dataId=%q 必须纯本地无错返回（无错即未触达不可达远端）: %v", dataID, err)
		}
		if got := c.GetString("service.name"); got != "test-svc" {
			t.Fatalf("dataId=%q service.name = %q, want 本地兜底 %q", dataID, got, "test-svc")
		}
	}
}

// TestNacosServerEnvOverride HUBX_NACOS_SERVER env 覆盖 server 地址（地址级最高优先）。
//
// 真实落点说明：地址解析内联在 nacosLoader（nacos.go:24-34），既无 Fetcher 抽象、
// 也无 resolveNacosServer 函数可直接断言，故在可观测落点断言——远端不可达时
// nacosLoader 用 `dataId=%q addr=%q` 回吐**实际使用的地址**，据此判定优先级。
func TestNacosServerEnvOverride(t *testing.T) {
	// 两个地址都是"已预留但无人监听"的端口，不可达且秒级失败；断言取 err 中
	// addr="..." 的实际值做**全等**比较，避免两个 127.0.0.1:<port> 互为前缀误判。
	yamlAddr := unusedTCPAddr(t)
	path := writeNacosLocal(t, yamlAddr)

	t.Run("env 覆盖 yaml", func(t *testing.T) {
		envAddr := unusedTCPAddr(t)
		t.Setenv("HUBX_NACOS_SERVER", envAddr)

		started := time.Now()
		_, err := Resolve("nacos", Options{
			Path:    path,
			Type:    "nacos",
			DataID:  "hubx.test-svc",
			AddrEnv: "HUBX_NACOS_SERVER",
		})
		t.Logf("env addr returned in %s: %v", time.Since(started), err)
		if err == nil {
			t.Fatal("expected unreachable nacos to return error")
		}
		if got := resolvedNacosAddr(t, err); got != envAddr {
			t.Fatalf("实际使用地址 = %q, want env 地址 %q（env 优先级最高）", got, envAddr)
		}
	})

	t.Run("env 未设置回落 yaml", func(t *testing.T) {
		t.Setenv("HUBX_NACOS_SERVER", "")

		started := time.Now()
		_, err := Resolve("nacos", Options{
			Path:    path,
			Type:    "nacos",
			DataID:  "hubx.test-svc",
			AddrEnv: "HUBX_NACOS_SERVER",
		})
		t.Logf("yaml addr returned in %s: %v", time.Since(started), err)
		if err == nil {
			t.Fatal("expected unreachable nacos to return error")
		}
		if got := resolvedNacosAddr(t, err); got != yamlAddr {
			t.Fatalf("实际使用地址 = %q, want yaml 地址 %q（env 空则回落 yaml）", got, yamlAddr)
		}
	})
}

// resolvedNacosAddr 从 nacosLoader 的错误上下文里取回实际使用的 server 地址
// （nacos.go 的 `nacos dataId=%q addr=%q`）。取不到即视为落点漂移，直接失败。
func resolvedNacosAddr(t *testing.T, err error) string {
	t.Helper()
	const marker = `addr="`
	i := strings.Index(err.Error(), marker)
	if i < 0 {
		t.Fatalf("error %q 未携带 addr 上下文，地址解析落点可能已漂移", err)
	}
	rest := err.Error()[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("error %q 的 addr 上下文格式异常", err)
	}
	return rest[:j]
}

// TestNacosAddrUnconfiguredPureLocal dataId 非空但地址解析为空（env 未指定 AddrEnv、
// yaml 无 configcenter.nacos.server_addr）→ 纯本地，同样不触达远端。
func TestNacosAddrUnconfiguredPureLocal(t *testing.T) {
	// env 有值但 AddrEnv 指向另一个未设置的 env，故地址解析结果为空；
	// 若加载器仍拿 HUBX_NACOS_SERVER 去拉远端，不可达地址会让它报错。
	t.Setenv("HUBX_NACOS_SERVER", unusedTCPAddr(t))

	path := writeNacosLocal(t, "")

	c, err := Resolve("nacos", Options{
		Path:    path,
		Type:    "nacos",
		DataID:  "hubx.test-svc",
		AddrEnv: "HUBX_NACOS_OTHER",
	})
	if err != nil {
		t.Fatalf("地址未配置必须纯本地无错返回（无错即未触达不可达远端）: %v", err)
	}
	if got := c.GetString("service.name"); got != "test-svc" {
		t.Fatalf("service.name = %q, want 本地兜底 %q", got, "test-svc")
	}
}

// TestNacosRealServer B1 真机分支：key 级裁决（远端值覆盖本地、未出现键保留本地）
// 需真机 nacos 底座，本轮无底座故以 Skip 形态保留，底座就绪后解除。
func TestNacosRealServer(t *testing.T) {
	t.Skip("无 nacos 底座；真机分支待回补")
}

// writeNacosLocal 写一份带 configcenter.nacos.server_addr 的临时本地 yaml。
func writeNacosLocal(t *testing.T, serverAddr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "local.yaml")
	body := "service:\n  name: test-svc\n  port: 8080\n" +
		"configcenter:\n  nacos:\n    server_addr: \"" + serverAddr + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func unusedTCPAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve unused TCP address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release unused TCP address: %v", err)
	}
	return addr
}
