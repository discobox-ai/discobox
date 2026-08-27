package poolruntime

import (
	"encoding/json"
	"testing"
	"time"
)

// The policy is embedded anonymously so its fields flatten into each provider's
// own JSON. A provider that nested it instead would silently stop reading a
// setting the catalog says it accepts.
func TestPoolPolicyFlattensIntoAProviderConfig(t *testing.T) {
	type providerConfig struct {
		PoolPolicy
		Image string `json:"image,omitempty"`
	}
	var cfg providerConfig
	if err := json.Unmarshal([]byte(`{"image":"pool:test","proxyAuditRetention":"72h"}`), &cfg); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if cfg.Image != "pool:test" {
		t.Fatalf("Image = %q", cfg.Image)
	}
	if got := cfg.ProxyAuditRetention.Value(); got != 72*time.Hour {
		t.Fatalf("ProxyAuditRetention = %s, want 72h", got)
	}
}

func TestUnsetRetentionLeavesThePoolProxyOnItsDefault(t *testing.T) {
	var cfg PoolPolicy
	if err := json.Unmarshal([]byte(`{}`), &cfg); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got := cfg.ProxyAuditRetention.Value(); got != 0 {
		t.Fatalf("ProxyAuditRetention = %s, want zero so the proxy default applies", got)
	}
}

func TestBadDurationsAreRejectedRatherThanIgnored(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"unparsable", `{"proxyAuditRetention":"two days"}`},
		{"negative", `{"proxyAuditRetention":"-1h"}`},
		{"a bare number is not a duration", `{"proxyAuditRetention":172800}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg PoolPolicy
			if err := json.Unmarshal([]byte(tc.body), &cfg); err == nil {
				t.Fatalf("Unmarshal(%s) was accepted", tc.body)
			}
		})
	}
}

func TestRetentionRoundTripsAsADurationString(t *testing.T) {
	cfg := PoolPolicy{ProxyAuditRetention: Duration(90 * time.Minute)}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var back PoolPolicy
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", data, err)
	}
	if back.ProxyAuditRetention != cfg.ProxyAuditRetention {
		t.Fatalf("round trip = %s, want %s (%s)", back.ProxyAuditRetention.Value(), cfg.ProxyAuditRetention.Value(), data)
	}
}

func TestPolicyFieldsAreOfferedByProviderCatalogs(t *testing.T) {
	fields := PoolPolicyConfigFields()
	if len(fields) != 1 || fields[0].Key != "proxyAuditRetention" {
		t.Fatalf("PoolPolicyConfigFields() = %+v", fields)
	}
	if fields[0].Placeholder != defaultRetentionHint {
		t.Fatalf("placeholder = %q, want %q", fields[0].Placeholder, defaultRetentionHint)
	}
}
