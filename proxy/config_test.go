package proxy

import "testing"

func TestConfigValidate(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	cfg.ListenAddress = "not-an-address"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid listen address error")
	}

	cfg = DefaultConfig()
	cfg.Cache.Enabled = true
	cfg.Cache.MaxSizeBytes = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid cache config error")
	}
}
