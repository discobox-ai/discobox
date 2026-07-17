package origin

import (
	"testing"

	"github.com/obot-platform/discobox/internal/hostid"
	"github.com/obot-platform/discobox/internal/originkey"
)

func TestKeyMatchesSharedDerivation(t *testing.T) {
	t.Setenv(hostid.EnvVar, "host_0123456789abcdef")

	resolved, err := Resolve(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := originkey.Of(resolved.HostId, resolved.ProjectPath)
	if got := Key(resolved); got != want {
		t.Fatalf("Key = %q, want %q", got, want)
	}
	if Key(resolved) == "" {
		t.Fatal("Key is empty; ls would silently list every sandbox")
	}
}
