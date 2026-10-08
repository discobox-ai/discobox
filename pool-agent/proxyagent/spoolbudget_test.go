package proxyagent

import (
	"encoding/json"
	"testing"

	"github.com/discobox-ai/discobox/proxy"
)

func TestUnsetAuditSpoolBudgetLeavesTheProxyDefaults(t *testing.T) {
	for _, name := range []string{EnvAuditMaxSize, EnvAuditMaxPercent, EnvAuditBodyHead} {
		t.Setenv(name, "")
	}
	cfg := proxy.DefaultConfig().Recording
	if err := ConfigureAuditSpoolBudget(&cfg); err != nil {
		t.Fatalf("ConfigureAuditSpoolBudget() error = %v", err)
	}
	if cfg.MaxSpoolBytes != proxy.DefaultMaxSpoolBytes || cfg.MaxSpoolPercent != proxy.DefaultMaxSpoolPercent || cfg.BodyHeadBytes != proxy.DefaultBodyHeadBytes {
		t.Fatalf("budget = %d bytes, %v%%, head %d; want the proxy defaults", cfg.MaxSpoolBytes, cfg.MaxSpoolPercent, cfg.BodyHeadBytes)
	}
}

func TestAuditSpoolBudgetReadsTheEnvironment(t *testing.T) {
	t.Setenv(EnvAuditMaxSize, "20GiB")
	t.Setenv(EnvAuditMaxPercent, "10%")
	t.Setenv(EnvAuditBodyHead, "1MiB")
	cfg := proxy.DefaultConfig().Recording
	if err := ConfigureAuditSpoolBudget(&cfg); err != nil {
		t.Fatalf("ConfigureAuditSpoolBudget() error = %v", err)
	}
	if cfg.MaxSpoolBytes != 20<<30 || cfg.MaxSpoolPercent != 10 || cfg.BodyHeadBytes != 1<<20 {
		t.Fatalf("budget = %d bytes, %v%%, head %d", cfg.MaxSpoolBytes, cfg.MaxSpoolPercent, cfg.BodyHeadBytes)
	}
}

// An explicit zero is not unset: it drops that term from the budget.
func TestZeroDropsABudgetTerm(t *testing.T) {
	t.Setenv(EnvAuditMaxSize, "0")
	t.Setenv(EnvAuditMaxPercent, "")
	t.Setenv(EnvAuditBodyHead, "")
	cfg := proxy.DefaultConfig().Recording
	if err := ConfigureAuditSpoolBudget(&cfg); err != nil {
		t.Fatalf("ConfigureAuditSpoolBudget() error = %v", err)
	}
	if cfg.MaxSpoolBytes != 0 || cfg.MaxSpoolPercent != proxy.DefaultMaxSpoolPercent {
		t.Fatalf("budget = %d bytes, %v%%; want the ceiling dropped and the percentage kept", cfg.MaxSpoolBytes, cfg.MaxSpoolPercent)
	}
}

func TestBadAuditSpoolBudgetIsLoud(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{EnvAuditMaxSize, "lots"},
		{EnvAuditMaxSize, "-1GiB"},
		{EnvAuditMaxPercent, "101"},
		{EnvAuditMaxPercent, "-5%"},
		{EnvAuditMaxPercent, "five"},
		{EnvAuditBodyHead, "0"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			cfg := proxy.DefaultConfig().Recording
			if err := ConfigureAuditSpoolBudget(&cfg); err == nil {
				t.Fatalf("ConfigureAuditSpoolBudget() accepted %s=%q", tc.name, tc.value)
			}
		})
	}
}

// A provider instance's configuration is refused when it is written, not when
// a pool first starts with it.
func TestAuditSpoolBudgetValidatesWhenRead(t *testing.T) {
	var budget AuditSpoolBudget
	if err := json.Unmarshal([]byte(`{"proxyAuditMaxSize":"50GiB","proxyAuditMaxPercent":"5%","proxyAuditBodyHead":"64KiB"}`), &budget); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	env := map[string]string{}
	budget.SetAuditSpoolEnv(env)
	if env[EnvAuditMaxSize] != "50GiB" || env[EnvAuditMaxPercent] != "5%" || env[EnvAuditBodyHead] != "64KiB" {
		t.Fatalf("env = %v, want each value passed through as written", env)
	}
	for _, data := range []string{`{"proxyAuditMaxSize":"lots"}`, `{"proxyAuditMaxPercent":"150"}`, `{"proxyAuditBodyHead":7}`, `{"proxyAuditBodyHead":"0"}`, `{"proxyAuditBodyHead":"0KiB"}`} {
		if err := json.Unmarshal([]byte(data), &budget); err == nil {
			t.Fatalf("Unmarshal(%s) accepted it", data)
		}
	}
}
