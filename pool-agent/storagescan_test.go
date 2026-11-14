package poolagent

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

func testScanner(poolID string) *storageScanner {
	return &storageScanner{
		poolID: poolID,
		diskUsage: func(context.Context) (sandboxruntime.DiskUsage, error) {
			return sandboxruntime.DiskUsage{}, nil
		},
		dutyCycle:   storageScanDutyCycle,
		minInterval: storageScanMinInterval,
		maxInterval: storageScanMaxInterval,
	}
}

// The whole schedule is one property: the agent spends a fixed fraction of wall
// time walking. At a 2% duty cycle a sweep costing d is followed by 50d of
// waiting, so an expensive pool is walked rarely and a cheap one often with
// nothing tuned per pool.
func TestNextIntervalSpendsAFixedFractionOfTimeWalking(t *testing.T) {
	scanner := testScanner("")
	for _, cost := range []time.Duration{5 * time.Second, 20 * time.Second, 45 * time.Second} {
		interval := scanner.nextInterval(cost)
		duty := float64(cost) / float64(interval)
		if math.Abs(duty-storageScanDutyCycle) > storageScanDutyCycle*0.5 {
			t.Errorf("a %s sweep every %s is a %.3f duty cycle, want about %.3f",
				cost, interval, duty, storageScanDutyCycle)
		}
	}
}

// The floor stops a trivially small pool being walked continuously, and the cap
// stops an enormous one being abandoned entirely.
func TestNextIntervalIsClamped(t *testing.T) {
	scanner := testScanner("")
	if got := scanner.nextInterval(time.Millisecond); got < storageScanMinInterval {
		t.Errorf("a 1ms sweep scheduled %s out, want at least %s", got, storageScanMinInterval)
	}
	if got := scanner.nextInterval(24 * time.Hour); got < storageScanMaxInterval {
		t.Errorf("a 24h sweep scheduled %s out, want at least the %s cap", got, storageScanMaxInterval)
	}
	// The stagger is added on top of the cap, so it may exceed it slightly;
	// what must not happen is an unbounded interval.
	if got := scanner.nextInterval(24 * time.Hour); got > 2*storageScanMaxInterval {
		t.Errorf("a 24h sweep scheduled %s out, far beyond the %s cap", got, storageScanMaxInterval)
	}
}

// An expensive pool must genuinely back off relative to a cheap one; that is
// the behavior the whole mechanism exists for.
func TestNextIntervalBacksOffWithCost(t *testing.T) {
	scanner := testScanner("")
	cheap := scanner.nextInterval(2 * time.Second)
	expensive := scanner.nextInterval(40 * time.Second)
	if expensive <= cheap {
		t.Errorf("a 40s sweep (%s) did not back off past a 2s sweep (%s)", expensive, cheap)
	}
}

// Pools sharing a host must not sweep in lockstep — one Docker daemon hosts
// every local pool, and after a reboot they would otherwise all start together.
func TestStaggerSpreadsPoolsButIsStablePerPool(t *testing.T) {
	const interval = 10 * time.Minute
	first := testScanner("pool-aaaa").nextInterval(12 * time.Second)
	second := testScanner("pool-bbbb").nextInterval(12 * time.Second)
	if first == second {
		t.Error("two pools with the same sweep cost landed on the identical interval")
	}
	// Stable for one pool: the same agent must not drift run to run.
	repeat := testScanner("pool-aaaa").nextInterval(12 * time.Second)
	if repeat != first {
		t.Errorf("the same pool got %s then %s; the stagger must be deterministic", first, repeat)
	}
	// And bounded, so it shifts the schedule rather than replacing it.
	if offset := testScanner("pool-aaaa").stagger(interval); offset < 0 || offset > time.Duration(float64(interval)*storageScanStagger) {
		t.Errorf("stagger %s is outside 0..%.0f%% of %s", offset, storageScanStagger*100, interval)
	}
}

// A canceled sweep must yield nothing rather than a partial total: a walk that
// stopped half way reports every unvisited tree as empty, which is a wrong
// answer rather than a missing one.
func TestWalkPoolTreesReportsNothingWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if walk, ok := walkPoolTrees(ctx, layout.ContainerAt(t.TempDir()), "prj_1", "pool_1", []string{"sbx_a"}, nil); ok || walk != nil {
		t.Errorf("a canceled sweep returned a result: %+v", walk)
	}
}

func TestSweepRecordsScheduleAndSnapshotIsReadable(t *testing.T) {
	scanner := testScanner("pool-aaaa")
	scanner.logger = slog.New(slog.DiscardHandler)
	scanner.sandboxIDs = func(context.Context) []string { return []string{"sbx_a"} }

	if scanner.Snapshot() != nil {
		t.Fatal("a scanner that has not swept must report no attribution, not zeroes")
	}
	interval, ok := scanner.sweep(context.Background())
	if !ok {
		t.Fatal("sweep reported the context ended")
	}
	walk := scanner.Snapshot()
	if walk == nil {
		t.Fatal("the completed sweep was not recorded")
	}
	if walk.ObservedAt.IsZero() {
		t.Error("the sweep carries no observation time, so its age cannot be read")
	}
	if walk.NextScanAt.Before(walk.ObservedAt) {
		t.Errorf("nextScanAt %v is before observedAt %v", walk.NextScanAt, walk.ObservedAt)
	}
	if math.Abs(walk.IntervalSeconds-interval.Seconds()) > 0.001 {
		t.Errorf("reported interval %.3fs does not match the scheduled %.3fs", walk.IntervalSeconds, interval.Seconds())
	}
	if len(walk.Sandboxes) != 1 || walk.Sandboxes[0].SandboxID != "sbx_a" {
		t.Errorf("sandboxes = %+v, want the one this pool hosts", walk.Sandboxes)
	}
}

// The walk must stop promptly when the agent is shutting down, even part way
// through a large tree.
func TestTreeBytesStopsOnCancellation(t *testing.T) {
	root := t.TempDir()
	for i := range 2000 {
		if err := os.WriteFile(filepath.Join(root, "f"+utoaTest(i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	full := treeBytes(context.Background(), root)
	stopped := treeBytes(ctx, root)
	if full <= 0 {
		t.Fatalf("uncanceled walk returned %d", full)
	}
	if stopped >= full {
		t.Errorf("canceled walk counted %d of %d; it did not stop early", stopped, full)
	}
}

func utoaTest(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// The walk attributes the disk the pool's proxy holds — its response cache, its
// recorded bodies and its audit database — not only the sandbox, cache and
// build trees, so a full disk is not mostly unexplained.
func TestWalkPoolTreesAttributesTheProxyStore(t *testing.T) {
	root := layout.ContainerAt(t.TempDir())
	const project, pool = "prj_1", "pool_1"
	payload := make([]byte, 1<<20)
	for _, path := range []string{
		filepath.Join(root.ProxyCache(project, pool), "blob"),
		filepath.Join(root.ProxyBodies(project, pool), "bodies", "sbx_a", "response-1.bin"),
		root.ProxyAuditDB(project, pool),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	walk, ok := walkPoolTrees(context.Background(), root, project, pool, []string{"sbx_a"}, nil)
	if !ok {
		t.Fatal("walk reported the context ended")
	}
	attributed := walk.DataBytes + walk.CacheBytes + walk.BuildBytes +
		walk.ProxyCacheBytes + walk.ProxyAuditBytes + walk.ProxyRecordingBytes
	if want := int64(3 << 20); attributed < want {
		t.Errorf("walk attributed %d bytes, want at least the %d the proxy store holds", attributed, want)
	}
}

// A box's row is everything that is its own: its trees, its container's
// writable layer and its recordings, under both spool trees. A spool no box
// of the sweep owns, a purged one's, is the pool's; shared images and other
// containers' layers are the pool's too, never split across the boxes
// (ADR 26-11-14-896 §§1-3).
func TestWalkPoolTreesChargesABoxItsOwnAndThePoolTheShared(t *testing.T) {
	root := layout.ContainerAt(t.TempDir())
	const project, pool = "prj_1", "pool_1"
	write := func(path string, size int) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root.SandboxData(project, pool, "sbx_a"), "file"), 1<<20)
	write(filepath.Join(root.ProxyBodies(project, pool), "bodies", "sbx_a", "response-1.bin"), 1<<20)
	write(filepath.Join(root.ProxyStreams(project, pool), "streams", "sbx_a", "stream-1.bin"), 1<<20)
	write(filepath.Join(root.ProxyBodies(project, pool), "bodies", "sbx_purged", "response-2.bin"), 1<<20)

	disk := &sandboxruntime.DiskUsage{
		Measured:        true,
		Layers:          map[string]int64{"sbx_a": 5 << 20, "sbx_unlisted": 2 << 20},
		ImageBytes:      40 << 20,
		OtherLayerBytes: 3 << 20,
		DaemonShared:    true,
	}
	walk, ok := walkPoolTrees(context.Background(), root, project, pool, []string{"sbx_a", "sbx_b"}, disk)
	if !ok {
		t.Fatal("walk reported the context ended")
	}
	a, b := walk.Sandboxes[0], walk.Sandboxes[1]
	if a.RecordingBytes < 2<<20 {
		t.Errorf("sbx_a's recordings = %d, want both its spools, at least %d", a.RecordingBytes, 2<<20)
	}
	if a.LayerBytes == nil || *a.LayerBytes != 5<<20 {
		t.Errorf("sbx_a's layer = %v, want %d", a.LayerBytes, 5<<20)
	}
	if want := a.DataBytes + a.RecordingBytes + 5<<20; a.TotalBytes != want {
		t.Errorf("sbx_a's total = %d, want its trees, recordings and layer, %d", a.TotalBytes, want)
	}
	// A box with no container has a layer of zero, not an unknown one.
	if b.LayerBytes == nil || *b.LayerBytes != 0 {
		t.Errorf("sbx_b's layer = %v, want a measured zero", b.LayerBytes)
	}
	if walk.ProxyRecordingBytes < 1<<20 || walk.ProxyRecordingBytes >= 2<<20 {
		t.Errorf("the pool's recordings = %d, want only the purged box's spool", walk.ProxyRecordingBytes)
	}
	if walk.DataBytes != a.TotalBytes+b.TotalBytes {
		t.Errorf("dataBytes = %d, want the rows' totals, %d", walk.DataBytes, a.TotalBytes+b.TotalBytes)
	}
	// A layer of a box the sweep did not list is on no row, so it is one of
	// the pool's other layers rather than counted nowhere.
	if walk.ImageBytes == nil || *walk.ImageBytes != 40<<20 || walk.OtherLayerBytes == nil || *walk.OtherLayerBytes != 5<<20 || !walk.DaemonShared {
		t.Errorf("pool figures = images %v, other layers %v, shared %v", walk.ImageBytes, walk.OtherLayerBytes, walk.DaemonShared)
	}

	// A runtime that could not say leaves its figures absent rather than zero.
	unmeasured, _ := walkPoolTrees(context.Background(), root, project, pool, []string{"sbx_a"}, nil)
	if unmeasured.ImageBytes != nil || unmeasured.OtherLayerBytes != nil || unmeasured.Sandboxes[0].LayerBytes != nil {
		t.Errorf("an unmeasured sweep reported image and layer figures: %+v", unmeasured)
	}
}

// A daemon that does not answer costs the sweep its image and layer figures,
// which are absent rather than zero, and nothing else: the trees are still
// walked and the sweep still recorded.
func TestSweepWithoutTheDaemonReportsItsFiguresAbsent(t *testing.T) {
	scanner := testScanner("pool-aaaa")
	scanner.logger = slog.New(slog.DiscardHandler)
	scanner.sandboxIDs = func(context.Context) []string { return []string{"sbx_a"} }
	scanner.diskUsage = func(context.Context) (sandboxruntime.DiskUsage, error) {
		return sandboxruntime.DiskUsage{}, errors.New("daemon unreachable")
	}
	if _, ok := scanner.sweep(context.Background()); !ok {
		t.Fatal("sweep reported the context ended")
	}
	walk := scanner.Snapshot()
	if walk == nil || len(walk.Sandboxes) != 1 {
		t.Fatalf("the sweep was not recorded whole: %+v", walk)
	}
	if walk.ImageBytes != nil || walk.OtherLayerBytes != nil || walk.Sandboxes[0].LayerBytes != nil {
		t.Errorf("a sweep the daemon did not answer reported its figures: %+v", walk)
	}
}

// A tree on another disk than the root's is not in the filesystem figure: a
// VM pool keeps its cache on a disk of its own. The sweep says how much of
// what it walked is elsewhere, so a reader does not subtract it from a used
// figure it is not part of.
func TestWalkPoolTreesSaysWhatIsOnAnotherFilesystem(t *testing.T) {
	root := layout.ContainerAt(t.TempDir())
	const project, pool = "prj_1", "pool_1"
	other, err := os.MkdirTemp("/dev/shm", "discobox-walk-")
	if err != nil {
		t.Skipf("no /dev/shm to stand in for another disk: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(other) })
	if err := os.MkdirAll(root.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	rootDevice, ok := statDevice(root.Dir())
	otherDevice, _ := statDevice(other)
	if !ok || rootDevice == otherDevice {
		t.Skip("/dev/shm is on the same filesystem as the test's temporary directory")
	}
	// The proxy's subtree lives on the other disk; the sandboxes' do not.
	if err := os.MkdirAll(filepath.Dir(root.ProxyPool(project, pool)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, root.ProxyPool(project, pool)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root.ProxyCache(project, pool), "blob"),
		filepath.Join(root.SandboxData(project, pool, "sbx_a"), "file"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, 1<<20), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	walk, ok := walkPoolTrees(context.Background(), root, project, pool, []string{"sbx_a"}, nil)
	if !ok {
		t.Fatal("walk reported the context ended")
	}
	if walk.ProxyCacheBytes == 0 || walk.OtherFilesystemBytes != walk.ProxyCacheBytes {
		t.Errorf("elsewhere = %d, want the proxy cache's %d and none of the sandbox's", walk.OtherFilesystemBytes, walk.ProxyCacheBytes)
	}
}

// The daemon call is part of what a sweep costs, so the duty cycle spaces
// sweeps by it too; and it has a deadline, so a daemon that hangs costs only
// its own figures.
func TestSweepCountsTheDaemonCallAndBoundsIt(t *testing.T) {
	scanner := testScanner("pool-aaaa")
	scanner.logger = slog.New(slog.DiscardHandler)
	scanner.sandboxIDs = func(context.Context) []string { return nil }
	scanner.diskUsage = func(ctx context.Context) (sandboxruntime.DiskUsage, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the daemon call has no deadline")
		}
		time.Sleep(50 * time.Millisecond)
		return sandboxruntime.DiskUsage{}, nil
	}
	if _, ok := scanner.sweep(context.Background()); !ok {
		t.Fatal("sweep reported the context ended")
	}
	if walk := scanner.Snapshot(); walk == nil || walk.DurationMillis < 50 {
		t.Errorf("the sweep's cost = %+v, want the daemon call's 50ms in it", walk)
	}
}
