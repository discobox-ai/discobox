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
	cfg.Headers = []HeaderRule{{ID: "empty", Pattern: "example.com"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid header rule error")
	}

	cfg = DefaultConfig()
	cfg.Headers = []HeaderRule{{ID: "bad-path", Pattern: "example.com", PathRegexes: []string{"["}, Set: map[string]string{"X-Test": "value"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid path regex error")
	}

	cfg = DefaultConfig()
	cfg.Headers = []HeaderRule{{ID: "bad-client", Pattern: "example.com", ClientIDs: []string{" "}, Set: map[string]string{"X-Test": "value"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid client ID error")
	}

	cfg = DefaultConfig()
	cfg.Cache.Enabled = true
	cfg.Cache.MaxSizeBytes = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid cache config error")
	}
}
