package sandbox

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// A phase entered once and held for minutes reads, to anyone judging by the
// record's age, as a phase that was abandoned. That is what made a booting VM
// look stopped: the narration blanked back to "waiting for a pool to take it",
// and placement gave up with "no sandbox capacity" while the boot was going
// fine.
func TestHoldKeepsRestatingThePhase(t *testing.T) {
	defer shortHeartbeat(t)()

	var mu sync.Mutex
	var reports []PoolProvisionPhase
	reporter := PoolProgressReporter(func(_ context.Context, _ string, progress PoolProvisionProgress) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, progress.Phase)
	})

	_, release := reporter.Hold(t.Context(), "pool_1", PoolPhaseStartingVM)
	// Wait for the restatements rather than for a number of heartbeat periods.
	// A timer does not fire at the resolution it is asked for — Windows steps in
	// about 15ms, so three 10ms heartbeats do not fit in 30ms — and the property
	// is that a held phase keeps being restated, not that it is restated at a
	// particular rate.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		enough := len(reports) >= 3
		mu.Unlock()
		if enough {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	release()

	mu.Lock()
	got := append([]PoolProvisionPhase(nil), reports...)
	mu.Unlock()
	if len(got) < 3 {
		t.Fatalf("got %d reports, want the phase restated while it was held", len(got))
	}
	for _, phase := range got {
		if phase != PoolPhaseStartingVM {
			t.Fatalf("reported %q, want every restatement to be the held phase", phase)
		}
	}
}

// Releasing stops it, or a finished phase would go on claiming to be current.
func TestHoldStopsOnRelease(t *testing.T) {
	defer shortHeartbeat(t)()

	var mu sync.Mutex
	count := 0
	reporter := PoolProgressReporter(func(context.Context, string, PoolProvisionProgress) {
		mu.Lock()
		defer mu.Unlock()
		count++
	})

	_, release := reporter.Hold(t.Context(), "pool_1", PoolPhaseWaitingForDocker)
	release()
	mu.Lock()
	afterRelease := count
	mu.Unlock()

	time.Sleep(2 * poolPhaseHeartbeat)
	mu.Lock()
	defer mu.Unlock()
	if count != afterRelease {
		t.Fatalf("kept reporting after release: %d then %d", afterRelease, count)
	}
	// Releasing twice is safe: the callers defer it and also call it early.
	release()
}

// A driver with no reporter says nothing, and holding nothing is still safe.
func TestHoldToleratesNoReporter(t *testing.T) {
	var reporter PoolProgressReporter
	_, release := reporter.Hold(t.Context(), "pool_1", PoolPhaseStartingVM)
	release()
}

// A driver refines a held phase from inside it: the engine holds starting_vm
// around EnsureVM, and a VM driver reports fetching_vm_image with its bytes
// from inside that call. The hold's heartbeat must not restate the bare phase
// over that report, or a status line sampling the pool sees the fetch, then a
// bare starting_vm with no bytes, then the fetch again.
func TestHoldDoesNotOverwriteAReportMadeInsideIt(t *testing.T) {
	defer shortHeartbeat(t)()

	var mu sync.Mutex
	var reports []PoolProvisionProgress
	reporter := PoolProgressReporter(func(_ context.Context, _ string, progress PoolProvisionProgress) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, progress)
	})

	ctx, release := reporter.Hold(t.Context(), "pool_1", PoolPhaseStartingVM)
	// The fetch moves bytes for many heartbeats, reporting as it goes.
	for current := int64(1); current <= 20; current++ {
		reporter.ReportProgress(ctx, "pool_1", PoolProvisionProgress{
			Phase: PoolPhaseFetchingVMImage,
			Pull:  &PoolPullProgress{Image: "guest", Current: current * 1024, Total: 20 * 1024},
		})
		time.Sleep(3 * poolPhaseHeartbeat)
	}
	reporter.ReportProgress(ctx, "pool_1", PoolProvisionProgress{
		Phase: PoolPhaseFetchingVMImage,
		Pull:  &PoolPullProgress{Image: "guest", Current: 20 * 1024, Total: 20 * 1024, Done: true},
	})
	release()

	mu.Lock()
	defer mu.Unlock()
	first, last := -1, -1
	for i, report := range reports {
		if report.Phase == PoolPhaseFetchingVMImage {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	for i := first; i <= last; i++ {
		if reports[i].Phase != PoolPhaseFetchingVMImage {
			t.Fatalf("report %d of %d is a bare %q between two %q reports of the same fetch, want the fetch kept until it is done",
				i, len(reports), reports[i].Phase, PoolPhaseFetchingVMImage)
		}
	}
}

// A fetch has quiet stretches — a registry that has not answered, the unpack
// between libkrun's guest and kernel images — longer than a heartbeat. The
// hold keeps the record fresh through them by restating what the work last
// reported, its byte counts included, never the bare held phase.
func TestHoldRestatesTheLastReportMadeInsideIt(t *testing.T) {
	defer shortHeartbeat(t)()

	var mu sync.Mutex
	var reports []PoolProvisionProgress
	reporter := PoolProgressReporter(func(_ context.Context, _ string, progress PoolProvisionProgress) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, progress)
	})
	// waitForRestatements waits until the hold has restated n more times.
	waitForRestatements := func(n int) {
		t.Helper()
		mu.Lock()
		want := len(reports) + n
		mu.Unlock()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			got := len(reports)
			mu.Unlock()
			if got >= want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("hold did not restate %d times", n)
	}

	ctx, release := reporter.Hold(t.Context(), "pool_1", PoolPhaseStartingVM)
	defer release()
	// A registry that has not answered yet: only the bare fetch phase.
	reporter.Report(ctx, "pool_1", PoolPhaseFetchingVMImage)
	waitForRestatements(3)
	guest := &PoolPullProgress{Image: "guest", Current: 10, Total: 10, Done: true}
	reporter.ReportProgress(ctx, "pool_1", PoolProvisionProgress{Phase: PoolPhaseFetchingVMImage, Pull: guest})
	// The unpack before the next image is fetched.
	waitForRestatements(3)
	reporter.ReportProgress(ctx, "pool_1", PoolProvisionProgress{Phase: PoolPhaseFetchingVMImage, Pull: &PoolPullProgress{Image: "kernel", Current: 1, Total: 5}})
	// Released before reading, so no restatement lands while the reports are
	// checked.
	release()

	mu.Lock()
	defer mu.Unlock()
	// From the first fetch report on: the heartbeat may restate the held phase
	// before the work's first report reaches it.
	first := slices.IndexFunc(reports, func(report PoolProvisionProgress) bool { return report.Phase == PoolPhaseFetchingVMImage })
	for i := first; i < len(reports); i++ {
		if reports[i].Phase != PoolPhaseFetchingVMImage {
			t.Fatalf("report %d is %q, want the fetch restated through its quiet stretches", i, reports[i].Phase)
		}
	}
	kernel := slices.IndexFunc(reports, func(report PoolProvisionProgress) bool { return report.Pull != nil && report.Pull.Image == "kernel" })
	if kernel < 1 {
		t.Fatalf("no kernel fetch report after a restatement: %+v", reports)
	}
	last := reports[kernel-1]
	if last.Pull == nil || *last.Pull != *guest {
		t.Fatalf("restated %+v before the kernel fetch, want the guest image's finished pull %+v", last.Pull, guest)
	}
}

// The work hands the line back by reporting the held phase once its
// refinement is over: the image is here, and the VM is starting.
func TestHoldResumesWhenTheWorkReportsTheHeldPhase(t *testing.T) {
	defer shortHeartbeat(t)()

	var mu sync.Mutex
	var reports []PoolProvisionPhase
	reporter := PoolProgressReporter(func(_ context.Context, _ string, progress PoolProvisionProgress) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, progress.Phase)
	})

	ctx, release := reporter.Hold(t.Context(), "pool_1", PoolPhaseStartingVM)
	defer release()
	reporter.ReportProgress(ctx, "pool_1", PoolProvisionProgress{Phase: PoolPhaseFetchingVMImage, Pull: &PoolPullProgress{Image: "guest", Current: 1, Total: 1, Done: true}})
	reporter.Report(ctx, "pool_1", PoolPhaseStartingVM)
	mu.Lock()
	handedBack := len(reports)
	mu.Unlock()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := append([]PoolProvisionPhase(nil), reports...)
		mu.Unlock()
		if len(got) >= handedBack+3 {
			for i, phase := range got[handedBack:] {
				if phase != PoolPhaseStartingVM {
					t.Fatalf("restatement %d after the hand-back is %q, want %q", i, phase, PoolPhaseStartingVM)
				}
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("hold stopped restating after the work handed the line back")
}

// A report about another pool, or made outside the held work, says nothing
// about what the held work is doing, so the heartbeat carries on.
func TestHoldIgnoresReportsFromOutsideIt(t *testing.T) {
	defer shortHeartbeat(t)()

	var mu sync.Mutex
	held := 0
	reporter := PoolProgressReporter(func(_ context.Context, poolID string, progress PoolProvisionProgress) {
		mu.Lock()
		defer mu.Unlock()
		if poolID == "pool_1" && progress.Phase == PoolPhaseStartingVM {
			held++
		}
	})

	ctx, release := reporter.Hold(t.Context(), "pool_1", PoolPhaseStartingVM)
	defer release()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		reporter.ReportProgress(ctx, "pool_2", PoolProvisionProgress{Phase: PoolPhaseFetchingVMImage})
		reporter.ReportProgress(t.Context(), "pool_1", PoolProvisionProgress{Phase: PoolPhaseFetchingVMImage})
		mu.Lock()
		enough := held >= 3
		mu.Unlock()
		if enough {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("held phase restated %d times, want the heartbeat to carry on", held)
}

// shortHeartbeat makes a held phase restate itself fast enough to assert on.
func shortHeartbeat(t *testing.T) func() {
	t.Helper()
	previous := poolPhaseHeartbeat
	poolPhaseHeartbeat = 10 * time.Millisecond
	return func() { poolPhaseHeartbeat = previous }
}

func TestHoldReleaseWaitsForAnActiveHeartbeat(t *testing.T) {
	defer shortHeartbeat(t)()

	started := make(chan struct{})
	finish := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(finish) }) }
	defer unblock()
	reports := 0
	reporter := PoolProgressReporter(func(context.Context, string, PoolProvisionProgress) {
		reports++
		if reports == 2 {
			close(started)
			<-finish
		}
	})

	_, release := reporter.Hold(t.Context(), "pool_1", PoolPhasePullingPoolImage)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("heartbeat did not enter the reporter")
	}
	releasing := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		close(releasing)
		release()
		close(stopped)
	}()
	<-releasing
	select {
	case <-stopped:
		t.Error("release returned while the heartbeat was still writing progress")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("release did not return after the heartbeat finished")
	}
}
