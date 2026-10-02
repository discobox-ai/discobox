package sandboxruntime

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
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

// heldSet answers the reaper as a control plane holding exactly ids would.
func heldSet(ids ...string) HeldSandboxes {
	return func(context.Context) ([]string, error) { return ids, nil }
}

// The incident this reaper was rebuilt for: a sandbox whose rebuild failed has
// a tree and no container, and as a settled failure it waits for its user's
// repair however long that takes. The control plane holds it the whole time, so
// its tree is kept the whole time — no clock is even started.
func TestReapUnheldSandboxVolumesKeepsAHeldTreeWithNoContainer(t *testing.T) {
	root := t.TempDir()
	failed := mkSandboxDir(t, root, "sbx_failed")
	now := time.Now()

	for _, at := range []time.Time{now, now.Add(48 * time.Hour), now.Add(90 * 24 * time.Hour)} {
		reapUnheldSandboxVolumes(context.Background(), root, heldSet("sbx_failed"), 24*time.Hour, at, quietLogger())
	}

	if _, err := os.Stat(filepath.Join(failed, "data")); err != nil {
		t.Fatalf("a held sandbox's tree was reaped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(failed, sandboxVolumeUnheldMarker)); !os.IsNotExist(err) {
		t.Fatalf("a held sandbox's tree was marked unheld")
	}
}

func TestReapUnheldSandboxVolumesReapsAfterRetention(t *testing.T) {
	root := t.TempDir()
	unheld := mkSandboxDir(t, root, "sbx_unheld")
	retention := 24 * time.Hour
	now := time.Now()

	// First pass outside the set: the clock starts, nothing is removed.
	reapUnheldSandboxVolumes(context.Background(), root, heldSet(), retention, now, quietLogger())
	if _, err := os.Stat(unheld); err != nil {
		t.Fatalf("unheld tree removed on first sight: %v", err)
	}
	if _, ok := readSandboxTombstone(filepath.Join(unheld, sandboxVolumeUnheldMarker)); !ok {
		t.Fatalf("unheld marker not written on first pass")
	}

	reapUnheldSandboxVolumes(context.Background(), root, heldSet(), retention, now.Add(retention-time.Minute), quietLogger())
	if _, err := os.Stat(unheld); err != nil {
		t.Fatalf("unheld tree removed within retention: %v", err)
	}

	reapUnheldSandboxVolumes(context.Background(), root, heldSet(), retention, now.Add(retention+time.Minute), quietLogger())
	if _, err := os.Stat(unheld); !os.IsNotExist(err) {
		t.Fatalf("unheld tree not reaped past retention: err=%v", err)
	}
}

// No answer is never an empty set. An agent that cannot reach the control plane
// — or one that has never been told anything — reaps nothing, and starts no
// clock that a later answer would inherit.
func TestReapUnheldSandboxVolumesReapsNothingWithoutAnAnswer(t *testing.T) {
	root := t.TempDir()
	dir := mkSandboxDir(t, root, "sbx_unknown")
	noAnswer := func(context.Context) ([]string, error) { return nil, errors.New("control plane unreachable") }
	now := time.Now()

	for _, at := range []time.Time{now, now.Add(48 * time.Hour), now.Add(90 * 24 * time.Hour)} {
		reapUnheldSandboxVolumes(context.Background(), root, noAnswer, 24*time.Hour, at, quietLogger())
	}

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a tree was reaped with no answer from the control plane: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, sandboxVolumeUnheldMarker)); !os.IsNotExist(err) {
		t.Fatalf("a tree was marked unheld with no answer from the control plane")
	}
}

// A sandbox back in the set — an import whose row has now been written, an
// answer that was wrong — has its clock cleared, so a later absence starts a
// full window rather than inheriting the old one.
func TestReapUnheldSandboxVolumesClearsTheClockWhenHeldAgain(t *testing.T) {
	root := t.TempDir()
	dir := mkSandboxDir(t, root, "sbx_back")
	writeSandboxTombstone(filepath.Join(dir, sandboxVolumeUnheldMarker), time.Now().Add(-48*time.Hour), quietLogger())

	reapUnheldSandboxVolumes(context.Background(), root, heldSet("sbx_back"), 24*time.Hour, time.Now(), quietLogger())

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a held tree with a stale clock was reaped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, sandboxVolumeUnheldMarker)); !os.IsNotExist(err) {
		t.Fatalf("a held tree kept its unheld marker")
	}
}

// A tombstone written when the reaper judged trees by their containers counted
// container absence, which says nothing about whether the sandbox is held. It
// is removed and never read, so an unheld tree carrying one still gets its full
// window from the first time it is seen outside the set.
func TestReapUnheldSandboxVolumesIgnoresTheContainerAbsenceTombstone(t *testing.T) {
	root := t.TempDir()
	dir := mkSandboxDir(t, root, "sbx_legacy")
	writeSandboxTombstone(filepath.Join(dir, legacySandboxVolumeTombstone), time.Now().Add(-48*time.Hour), quietLogger())

	reapUnheldSandboxVolumes(context.Background(), root, heldSet(), 24*time.Hour, time.Now(), quietLogger())

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a tree was reaped on a container-absence clock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, legacySandboxVolumeTombstone)); !os.IsNotExist(err) {
		t.Fatalf("the container-absence tombstone was not removed")
	}
}

// The trees are listed before the control plane is asked, so a tree that
// appears while the answer is in flight — a create whose row the answer may
// predate — is not judged by that answer at all.
func TestReapUnheldSandboxVolumesJudgesOnlyTreesListedBeforeTheAnswer(t *testing.T) {
	root := t.TempDir()
	mkSandboxDir(t, root, "sbx_held")
	var late string
	held := func(context.Context) ([]string, error) {
		late = mkSandboxDir(t, root, "sbx_late")
		return []string{"sbx_held"}, nil
	}

	reapUnheldSandboxVolumes(context.Background(), root, held, 24*time.Hour, time.Now(), quietLogger())

	if _, err := os.Stat(filepath.Join(late, sandboxVolumeUnheldMarker)); !os.IsNotExist(err) {
		t.Fatalf("a tree created after the trees were listed was judged by the answer")
	}
}

// The reaper only ever scans the root it is given (this pool's own sandboxes
// dir), against this pool's own held set, so a sibling pool's tree is
// untouched.
func TestReapUnheldSandboxVolumesIsScopedToItsRoot(t *testing.T) {
	base := t.TempDir()
	poolA := filepath.Join(base, "pools", "pool_a", "sandboxes")
	poolB := filepath.Join(base, "pools", "pool_b", "sandboxes")
	unheldA := mkSandboxDir(t, poolA, "sbx_a")
	treeB := mkSandboxDir(t, poolB, "sbx_b")

	now := time.Now()
	reapUnheldSandboxVolumes(context.Background(), poolA, heldSet(), time.Hour, now, quietLogger())
	reapUnheldSandboxVolumes(context.Background(), poolA, heldSet(), time.Hour, now.Add(48*time.Hour), quietLogger())

	if _, err := os.Stat(unheldA); !os.IsNotExist(err) {
		t.Fatalf("pool A did not reap its own unheld tree: err=%v", err)
	}
	if _, err := os.Stat(treeB); err != nil {
		t.Fatalf("pool A reaped pool B's tree: %v", err)
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
	state := withTestRoot(t)
	agentA := &DockerSandboxRuntime{root: state, projectID: "proj_a", poolID: "pool_a"}
	agentB := &DockerSandboxRuntime{root: state, projectID: "proj_b", poolID: "pool_b"}

	// Project B has a live pool with staged proxy material and a data subtree.
	liveProxyB := proxyagent.PoolSandboxMaterialRoot(state, "proj_b", "pool_b")
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
			proxyagent.PoolsRoot(state, "proj_a"),
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

// Storage accounting enumerates trees rather than containers, because the two
// halves of a sandbox do not end together: an archived one is a tree with no
// container and is still occupying every byte it occupied while running.
func TestStoredSandboxIDsFindsTreesWithoutContainers(t *testing.T) {
	root := t.TempDir()
	mkSandboxDir(t, root, "sbx_live")
	archived := mkSandboxDir(t, root, "sbx_archived")
	if err := writeSandboxArchiveMarker(archived, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A stray file beside the directories is not a sandbox.
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	ids, err := storedSandboxIDs(root)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "sbx_archived" || ids[1] != "sbx_live" {
		t.Errorf("ids = %v, want both trees and not the file", ids)
	}
}

// A pool that has never created a sandbox has no root yet. That is no
// sandboxes, not a failure that would cost the whole resource report.
func TestStoredSandboxIDsTreatsAMissingRootAsEmpty(t *testing.T) {
	ids, err := storedSandboxIDs(filepath.Join(t.TempDir(), "never-created"))
	if err != nil {
		t.Fatalf("a missing root errored: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("ids = %v, want none", ids)
	}
}
