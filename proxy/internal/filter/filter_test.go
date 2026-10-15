package filter

import "testing"

func TestFilterRestrictsOnlyTheClientsItNames(t *testing.T) {
	filter := New(Config{Clients: map[string][]string{
		"sandbox-1": {"api.example.com", "10.0.0.0/24"},
		"sandbox-2": nil,
	}})

	for _, tc := range []struct {
		host, client string
		want         bool
	}{
		{"api.example.com", "sandbox-1", true},
		{"10.0.0.10:443", "sandbox-1", true},
		{"other.example.com", "sandbox-1", false},
		{"api.example.com", "sandbox-2", false},
		{"api.example.com", "sandbox-3", true},
		{"anything.example", "", true},
	} {
		if got := filter.AllowHostForClient(tc.host, tc.client); got != tc.want {
			t.Errorf("AllowHostForClient(%q, %q) = %v, want %v", tc.host, tc.client, got, tc.want)
		}
	}
}

func TestNilFilterAllowsEverything(t *testing.T) {
	var filter *Filter
	if !filter.AllowHostForClient("example.com", "sandbox-1") {
		t.Fatal("a nil filter denied a host")
	}
}
