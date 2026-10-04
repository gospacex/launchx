package configsrc

import "testing"

func TestLocalLoaderPresenceDefault(t *testing.T) {
	c, err := Resolve("local", Options{Path: "testdata/local.yaml"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := c.GetString("service.name"); got != "test-svc" {
		t.Fatalf("service.name = %q, want %q", got, "test-svc")
	}
}

func TestLocalExpandsEnvInDSN(t *testing.T) {
	t.Setenv("HUBX_DB_PW", "secret123")
	c, _ := Resolve("local", Options{Path: "testdata/local.yaml"})
	want := "order_rw:secret123@tcp(127.0.0.1:3306)/order_db"
	if got := c.GetString("xmiddleware.sqlx.databases.test-svc.dsn"); got != want {
		t.Fatalf("dsn = %q, want %q", got, want)
	}
}

func TestUnknownLoaderErrors(t *testing.T) {
	if _, err := Resolve("etcd", Options{Path: "testdata/local.yaml"}); err == nil {
		t.Fatal("expected error for unregistered loader")
	}
}
