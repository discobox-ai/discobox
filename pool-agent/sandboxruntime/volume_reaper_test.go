package sandboxruntime

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func mkSandboxDir(t *testing.T, root, sandboxID string) string {
	t.Helper()
	dir := filepath.Join(root, sandboxID)
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReapDeadSandboxVolumesTombstoneLifecycle(t *testing.T) {
	root := t.TempDir()
	dead := mkSandboxDir(t, root, "sbx_dead")
	retention := 24 * time.Hour
	now := time.Now()

	// First pass: no container, no tombstone -> stamp, keep.
	reapDeadSandboxVolumes(root, map[string]struct{}{}, retention, now, quietLogger())
	if _, err := os.Stat(dead); err != nil {
		t.Fatalf("dead dir removed too early: %v", err)
	}
	if _, ok := readSandboxTombstone(filepath.Join(dead, sandboxVolumeTombstone)); !ok {
		t.Fatalf("tombstone not written on first pass")
	}

	// Within retention: still kept.
	reapDeadSandboxVolumes(root, map[string]struct{}{}, retention, now.Add(retention-time.Minute), quietLogger())
	if _, err := os.Stat(dead); err != nil {
		t.Fatalf("dead dir removed within retention: %v", err)
	}

	// Past retention: reaped.
	reapDeadSandboxVolumes(root, map[string]struct{}{}, retention, now.Add(retention+time.Minute), quietLogger())
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatalf("dead dir not reaped past retention: err=%v", err)
	}
}

func TestReapDeadSandboxVolumesKeepsLiveAndClearsTombstone(t *testing.T) {
	root := t.TempDir()
	live := mkSandboxDir(t, root, "sbx_live")
	// Pre-existing tombstone from a period the container was down.
	writeSandboxTombstone(filepath.Join(live, sandboxVolumeTombstone), time.Now().Add(-48*time.Hour), quietLogger())

	// The sandbox is live again: even well past retention, it must survive and
	// its stale tombstone must be cleared.
	reapDeadSandboxVolumes(root, map[string]struct{}{"sbx_live": {}}, 24*time.Hour, time.Now(), quietLogger())

	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live sandbox dir was reaped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(live, sandboxVolumeTombstone)); !os.IsNotExist(err) {
		t.Fatalf("stale tombstone not cleared for live sandbox")
	}
}

// The reaper only ever scans the root it is given (this pool's own
// sandboxes dir), so a sibling pool's tree is untouched.
func TestReapDeadSandboxVolumesIsScopedToItsRoot(t *testing.T) {
	base := t.TempDir()
	poolA := filepath.Join(base, "pools", "pool_a", "sandboxes")
	poolB := filepath.Join(base, "pools", "pool_b", "sandboxes")
	deadA := mkSandboxDir(t, poolA, "sbx_a")
	liveB := mkSandboxDir(t, poolB, "sbx_b")

	// Pool A reaps its own dead sandbox (empty live set), well past retention.
	past := time.Now().Add(48 * time.Hour)
	reapDeadSandboxVolumes(poolA, map[string]struct{}{}, time.Hour, time.Now(), quietLogger())
	reapDeadSandboxVolumes(poolA, map[string]struct{}{}, time.Hour, past, quietLogger())

	if _, err := os.Stat(deadA); !os.IsNotExist(err) {
		t.Fatalf("pool A did not reap its own dead sandbox: err=%v", err)
	}
	if _, err := os.Stat(liveB); err != nil {
		t.Fatalf("pool A reaped pool B's sandbox: %v", err)
	}
}
