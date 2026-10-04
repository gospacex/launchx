package configsrc

import (
	"os"
	"testing"
)

// TestEnvConfigGetExpandsString 校验 Get 走的是包装层，字符串同样被展开。
func TestEnvConfigGetExpandsString(t *testing.T) {
	t.Setenv("HUBX_DB_PW", "from-get")
	c, err := Resolve("local", Options{Path: "testdata/local.yaml"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := "order_rw:from-get@tcp(127.0.0.1:3306)/order_db"
	if got, _ := c.Get("xmiddleware.sqlx.databases.test-svc.dsn").(string); got != want {
		t.Fatalf("Get(dsn) = %q, want %q", got, want)
	}
}

// TestEnvConfigUnsetVarExpandsToEmpty 未设置的变量展开为空串，而不是保留 ${} 字面量。
func TestEnvConfigUnsetVarExpandsToEmpty(t *testing.T) {
	old, had := os.LookupEnv("HUBX_DB_PW")
	os.Unsetenv("HUBX_DB_PW")
	t.Cleanup(func() {
		if had {
			os.Setenv("HUBX_DB_PW", old)
		}
	})

	c, err := Resolve("local", Options{Path: "testdata/local.yaml"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := "order_rw:@tcp(127.0.0.1:3306)/order_db"
	if got := c.GetString("xmiddleware.sqlx.databases.test-svc.dsn"); got != want {
		t.Fatalf("dsn = %q, want %q", got, want)
	}
}

// TestEnvConfigNonStringPassThrough 非字符串值原样返回，既不被 ExpandEnv 触碰，
// 也不被转成字符串。
func TestEnvConfigNonStringPassThrough(t *testing.T) {
	c, err := Resolve("local", Options{Path: "testdata/local.yaml"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	v := c.Get("service.port")
	if v == nil {
		t.Fatal("service.port = nil, want 8080")
	}
	if _, isStr := v.(string); isStr {
		t.Fatalf("service.port = %T(%v), want non-string", v, v)
	}
	if got := c.GetInt("service.port"); got != 8080 {
		t.Fatalf("GetInt(service.port) = %d, want 8080", got)
	}
}

// TestEnvConfigNestedMapNotExpanded 记录当前边界：展开只发生在标量字符串读取
// （GetString / Get），GetMap 返回的子树仍是未展开的原始值。
func TestEnvConfigNestedMapNotExpanded(t *testing.T) {
	t.Setenv("HUBX_DB_PW", "nested")
	c, err := Resolve("local", Options{Path: "testdata/local.yaml"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	m := c.GetMap("xmiddleware.sqlx.databases.test-svc")
	if m == nil {
		t.Fatal("expected map for xmiddleware.sqlx.databases.test-svc")
	}
	raw := m["dsn"]
	want := "order_rw:${HUBX_DB_PW}@tcp(127.0.0.1:3306)/order_db"
	if raw != want {
		t.Fatalf("GetMap(dsn) = %v, want unexpanded %q", raw, want)
	}
}
