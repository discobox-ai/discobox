package sandboxruntime

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
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

// An archived sandbox looks exactly like a dead one to the reaper — a directory
// with no container — and must survive anyway, indefinitely. Its retention is a
// control-plane policy the agent does not know, enforced by an explicit purge
// (ADR 0022 §4). Reaping it here would delete data the user asked to keep, on a
// schedule nobody chose.
func TestReapDeadSandboxVolumesSkipsArchived(t *testing.T) {
	root := t.TempDir()
	archived := mkSandboxDir(t, root, "sbx_archived")
	if err := writeSandboxArchiveMarker(archived, time.Now().Add(-90*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Far past any retention, and with no live container.
	reapDeadSandboxVolumes(root, map[string]struct{}{}, 24*time.Hour, time.Now(), quietLogger())

	if _, err := os.Stat(archived); err != nil {
		t.Fatalf("archived sandbox dir was reaped: %v", err)
	}
	// It must not even be tombstoned: a tombstone would start the reaper's clock
	// and delete the tree 24h after the sandbox is unarchived and next stopped.
	if _, err := os.Stat(filepath.Join(archived, sandboxVolumeTombstone)); !os.IsNotExist(err) {
		t.Fatalf("archived sandbox was tombstoned")
	}
}

// Clearing the marker is the whole of what unarchive does on disk, so once it
// is gone the tree must be ordinary again — including being reapable if its
// container never comes back.
func TestReapDeadSandboxVolumesReapsAfterUnarchive(t *testing.T) {
	root := t.TempDir()
	dir := mkSandboxDir(t, root, "sbx_unarchived")
	if err := writeSandboxArchiveMarker(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := clearSandboxArchiveMarker(dir); err != nil {
		t.Fatal(err)
	}

	retention := 24 * time.Hour
	now := time.Now()
	reapDeadSandboxVolumes(root, map[string]struct{}{}, retention, now, quietLogger())
	reapDeadSandboxVolumes(root, map[string]struct{}{}, retention, now.Add(retention+time.Minute), quietLogger())

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("unarchived dead dir not reaped: err=%v", err)
	}
}

func TestReapUnknownPoolsRetainsThenReapsData(t *testing.T) {
	dataRoot := t.TempDir()
	cacheRoot := t.TempDir()
	proxyRoot := t.TempDir()
	// A known pool and an orphan pool, each with data + cache + proxy subtrees.
	for _, root := range []string{dataRoot, cacheRoot, proxyRoot} {
		for _, pool := range []string{"pool_known", "pool_orphan"} {
			if err := os.MkdirAll(filepath.Join(root, pool, "sandboxes"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	known := map[string]struct{}{"pool_known": {}}
	retention := 24 * time.Hour
	now := time.Now()

	// First pass: orphan is tombstoned, nothing deleted.
	reapUnknownPools(dataRoot, cacheRoot, proxyRoot, known, retention, now, quietLogger())
	for _, pool := range []string{"pool_known", "pool_orphan"} {
		if _, err := os.Stat(filepath.Join(dataRoot, pool)); err != nil {
			t.Fatalf("%s data removed too early: %v", pool, err)
		}
	}

	// Past retention: only the orphan's data, cache, and proxy subtrees are reaped.
	reapUnknownPools(dataRoot, cacheRoot, proxyRoot, known, retention, now.Add(retention+time.Minute), quietLogger())
	if _, err := os.Stat(filepath.Join(dataRoot, "pool_orphan")); !os.IsNotExist(err) {
		t.Fatalf("orphan pool data not reaped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proxyRoot, "pool_orphan")); !os.IsNotExist(err) {
		t.Fatalf("orphan pool proxy not reaped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, "pool_orphan")); !os.IsNotExist(err) {
		t.Fatalf("orphan pool cache not reaped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataRoot, "pool_known")); err != nil {
		t.Fatalf("known pool must survive: %v", err)
	}
}

func TestReapUnknownPoolsReapsProxyOnlyLeftoverImmediately(t *testing.T) {
	dataRoot := t.TempDir()
	cacheRoot := t.TempDir()
	proxyRoot := t.TempDir()
	// Proxy material lingering with no data subtree (regenerable) is reaped now.
	if err := os.MkdirAll(filepath.Join(proxyRoot, "pool_gone", "sandboxes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cacheRoot, "pool_gone", "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	reapUnknownPools(dataRoot, cacheRoot, proxyRoot, map[string]struct{}{}, 24*time.Hour, time.Now(), quietLogger())
	if _, err := os.Stat(filepath.Join(proxyRoot, "pool_gone")); !os.IsNotExist(err) {
		t.Fatalf("proxy-only leftover not reaped immediately: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, "pool_gone")); !os.IsNotExist(err) {
		t.Fatalf("cache-only leftover not reaped immediately: %v", err)
	}
}

// The control plane hands each pool agent the authoritative pool set for one
// project, so the roots that agent reaps must hold only that project's pools.
// This exercises the real path helpers (relocated under the test state root)
// rather than two unrelated temp dirs: a project-global proxy pools root would
// put another project's live pool in scope and delete the proxy material out
// from under its running sandboxes, breaking egress with no log line.
func TestReapUnknownPoolsLeavesAnotherProjectsLivePoolAlone(t *testing.T) {
	withTestRoot(t)
	agentA := &DockerSandboxRuntime{projectID: "proj_a", poolID: "pool_a"}
	agentB := &DockerSandboxRuntime{projectID: "proj_b", poolID: "pool_b"}

	// Project B has a live pool with staged proxy material and a data subtree.
	liveProxyB := resolve(proxyagent.PoolSandboxMaterialRoot("proj_b", "pool_b"))
	liveDataB := agentB.sandboxesRoot()
	for _, dir := range []string{liveProxyB, liveDataB} {
		if err := os.MkdirAll(filepath.Join(dir, "sbx_live"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Project A's agent reaps, knowing only its own project's pools. It runs
	// twice: any retention window has to expire without B being touched.
	known := map[string]struct{}{"pool_a": {}}
	now := time.Now()
	for _, at := range []time.Time{now, now.Add(48 * time.Hour)} {
		reapUnknownPools(
			agentA.poolsRoot(),
			agentA.cachePoolsRoot(),
			resolve(proxyagent.PoolsRoot("proj_a")),
			known, 24*time.Hour, at, quietLogger(),
		)
	}

	if _, err := os.Stat(filepath.Join(liveProxyB, "sbx_live")); err != nil {
		t.Fatalf("another project's live proxy material was reaped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(liveDataB, "sbx_live")); err != nil {
		t.Fatalf("another project's live sandbox data was reaped: %v", err)
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
