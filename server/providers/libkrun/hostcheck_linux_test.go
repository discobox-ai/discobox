//go:build linux && amd64

package libkrun

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/health"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/libkrun/internal/krunvm"
)

// A first start that would install libkrun loads the libkrun its image carries
// before it installs anything, and a library that will not load is the
// runtime's failure, said as one — not a pool that fails at its first boot
// (ADR 0148 §2).
func TestCheckHostRefusesALibkrunThatWillNotLoad(t *testing.T) {
	if err := krunvm.CheckKVM(); err != nil {
		t.Skipf("this host has no KVM, so the check stops before the runtime: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	local := effectiveImageLocalDir("")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{rootArtifact, kernelArtifact, libraryArtifact, passtArtifact} {
		if err := os.WriteFile(filepath.Join(local, name), []byte("not what it says it is"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	err := CheckHost(t.Context(), dockerworker.ServerDefaults{})
	var unavailable *sandbox.ProviderUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("CheckHost() = %v, want a *sandbox.ProviderUnavailableError", err)
	}
	if unavailable.Reason != health.ReasonRuntimeUnloadable {
		t.Fatalf("reason = %q, want %q (%v)", unavailable.Reason, health.ReasonRuntimeUnloadable, err)
	}
	if !strings.Contains(err.Error(), filepath.Join(local, libraryArtifact)) {
		t.Fatalf("error %q does not name the library that would not load", err)
	}
}
