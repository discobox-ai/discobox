package filter

import "testing"

func TestFilterClientScopedRules(t *testing.T) {
	filter := New(Config{
		Enabled: true,
		Rules: []Rule{{
			ClientIDs: []string{"sandbox-1"},
			Domains:   []string{"api.example.com"},
			IPs:       []string{"10.0.0.0/24"},
		}},
	})

	if !filter.AllowHostForClient("api.example.com", "sandbox-1") {
		t.Fatal("expected sandbox-1 to reach scoped domain")
	}
	if filter.AllowHostForClient("api.example.com", "sandbox-2") {
		t.Fatal("expected sandbox-2 to be denied scoped domain")
	}
	if !filter.AllowHostForClient("10.0.0.10:443", "sandbox-1") {
		t.Fatal("expected sandbox-1 to reach scoped CIDR")
	}
	if filter.AllowHostForClient("10.0.0.10:443", "sandbox-2") {
		t.Fatal("expected sandbox-2 to be denied scoped CIDR")
	}
}
