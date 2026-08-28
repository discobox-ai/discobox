package poolagent

import (
	"context"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testScanner(poolID string) *storageScanner {
	return &storageScanner{
		poolID:      poolID,
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
	if walk, ok := walkPoolTrees(ctx, "prj_1", "pool_1", []string{"sbx_a"}); ok || walk != nil {
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
