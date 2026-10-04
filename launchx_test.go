package launchx

import "testing"

func TestBootstrapReturnsApp(t *testing.T) {
	app, err := Bootstrap("testdata/empty.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if app == nil {
		t.Fatal("app is nil")
	}
	if err := app.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Close 幂等
	if err := app.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestOnSetupRegistered(t *testing.T) {
	app, _ := Bootstrap("testdata/empty.yaml")
	called := false
	app.OnSetup(func(a *App) error { called = true; return nil })
	if len(app.setups) != 1 {
		t.Fatalf("setups = %d, want 1", len(app.setups))
	}
	_ = called
}
