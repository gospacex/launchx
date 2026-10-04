package configsrc

import (
	"net"
	"os"
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
