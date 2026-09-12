package origin

import (
	"testing"

	"github.com/discobox-ai/discobox/internal/hostid"
)

// The origin names the client and nothing about a directory (ADR 0111): the
// host identity is what every host-based decision reads, and where a discobox
// belongs on that host is its origin key.
func TestResolveCarriesTheHost(t *testing.T) {
	t.Setenv(hostid.EnvVar, "host_0123456789abcdef")

	resolved, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.HostId != "host_0123456789abcdef" {
		t.Fatalf("HostId = %q, want the configured host identity", resolved.HostId)
	}
}
