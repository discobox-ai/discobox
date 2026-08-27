package proxyagent

import (
	"testing"
	"time"
)

func TestConfiguredAuditRetentionIsZeroWhenUnset(t *testing.T) {
	t.Setenv(EnvAuditRetention, "")
	retention, err := ConfiguredAuditRetention()
	if err != nil {
		t.Fatalf("ConfiguredAuditRetention() error = %v", err)
	}
	if retention != 0 {
		t.Fatalf("retention = %s, want zero so the proxy default applies", retention)
	}
}

func TestConfiguredAuditRetentionReadsTheWindow(t *testing.T) {
	t.Setenv(EnvAuditRetention, "72h")
	retention, err := ConfiguredAuditRetention()
	if err != nil {
		t.Fatalf("ConfiguredAuditRetention() error = %v", err)
	}
	if retention != 72*time.Hour {
		t.Fatalf("retention = %s, want 72h", retention)
	}
}

// Both ways to get this wrong — discarding an audit trail someone needs, and
// never reclaiming anything — are worse than refusing to start.
func TestBadAuditRetentionIsLoud(t *testing.T) {
	for _, value := range []string{"two days", "0", "-1h"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(EnvAuditRetention, value)
			if _, err := ConfiguredAuditRetention(); err == nil {
				t.Fatalf("ConfiguredAuditRetention() accepted %q", value)
			}
		})
	}
}
