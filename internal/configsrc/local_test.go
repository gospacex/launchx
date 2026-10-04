package configsrc

import (
	"strings"
	"testing"
)

func TestLocalLoaderPresenceDefault(t *testing.T) {
	c, err := Resolve("local", Options{Path: "testdata/local.yaml"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := c.GetString("service.name"); got != "test-svc" {
		t.Fatalf("service.name = %q, want %q", got, "test-svc")
	}
}

// TestResolveEmptyNameDefaultsToLocal 空名走 local 默认分支（registry.go 的
// name == "" → "local"），该分支此前无人覆盖。
func TestResolveEmptyNameDefaultsToLocal(t *testing.T) {
	c, err := Resolve("", Options{Path: "testdata/local.yaml"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := c.GetString("service.name"); got != "test-svc" {
		t.Fatalf("service.name = %q, want %q", got, "test-svc")
	}
}

// TestLocalMissingPathErrors 路径不存在必须硬失败。
// configx.ReadConfig 对任意不存在的路径都不报错（viper.go:58-63 吞掉
// ConfigFileNotFoundError，回退空配置），错拼路径会在本层被拦下，
// 避免调用方拿到空配置却以为加载成功。
func TestLocalMissingPathErrors(t *testing.T) {
	const missing = "testdata/does-not-exist.yaml"
	_, err := Resolve("local", Options{Path: missing})
	if err == nil {
		t.Fatalf("expected error for missing config path %q", missing)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("error %q should mention path %q", err, missing)
	}
}

func TestLocalExpandsEnvInDSN(t *testing.T) {
	t.Setenv("HUBX_DB_PW", "secret123")
	c, err := Resolve("local", Options{Path: "testdata/local.yaml"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := "order_rw:secret123@tcp(127.0.0.1:3306)/order_db"
	if got := c.GetString("xmiddleware.sqlx.databases.test-svc.dsn"); got != want {
		t.Fatalf("dsn = %q, want %q", got, want)
	}
}

func TestUnknownLoaderErrors(t *testing.T) {
	_, err := Resolve("etcd", Options{Path: "testdata/local.yaml"})
	if err == nil {
		t.Fatal("expected error for unregistered loader")
	}
	// 错误信息须带可用加载器列表，否则改坏了测试还是绿的。
	if !strings.Contains(err.Error(), "local") {
		t.Fatalf("error %q should list available loader %q", err, "local")
	}
}
