package sandboxruntime

import (
	"os"
	"testing"
)

// Tier 2 of the attach wait (ADR 0039) waits for a container to come back only
// for a sandbox this pool is actually holding. The tree is what says so: it is
// the durable half of a sandbox and outlives the container a rebuild replaces,
// so a sandbox mid-rebuild is worth waiting for while an id this pool has never
// seen is not — waiting on that one would stall every sandbox-directed route
// for the full budget before failing.
func TestHostsSandboxTellsARebuildFromAnUnknownID(t *testing.T) {
	withTestRoot(t)
	runtime := &DockerSandboxRuntime{projectID: "proj_a", poolID: "pool_a"}

	if runtime.hostsSandbox("sbx_elsewhere") {
		t.Fatal("a sandbox with no tree on this pool reads as hosted here")
	}

	if err := os.MkdirAll(runtime.sandboxRoot("sbx_rebuilding"), 0o755); err != nil {
		t.Fatalf("create sandbox tree: %v", err)
	}
	if !runtime.hostsSandbox("sbx_rebuilding") {
		t.Fatal("a sandbox whose tree is retained here does not read as hosted")
	}
}
