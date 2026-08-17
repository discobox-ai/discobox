package reconcile

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testEngine(t *testing.T) (*Engine, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// Registered after t.TempDir's own cleanup and before start's engine stop,
	// so cleanup runs in the only order that works: stop the workers, release
	// the handle, then remove the directory. Linux unlinks a file that is still
	// open, so a leaked handle is invisible there; Windows refuses, and it
	// surfaces as a cleanup failure rather than as the leak it is.
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	e, err := New(db, Options{
		SingleNode:   true,
		PollInterval: 20 * time.Millisecond,
		Lease:        2 * time.Second,
		BackoffBase:  50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, db
}

func start(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.Stop(ctx)
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func rowCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&dirtyRow{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// errBox lets a possibly-nil error go through atomic.Value, which panics on a
// nil interface.
type errBox struct{ err error }

type fakeReconciler struct {
	runs    atomic.Int32
	fail    atomic.Int32  // fail this many runs before succeeding
	block   chan struct{} // if non-nil, each run waits until it can receive
	lastIDs sync.Map
	// onRun, when set, supplies the Result (and any extra behavior) for a run.
	onRun func(ctx context.Context, id string) Result
}

func (f *fakeReconciler) Reconcile(ctx context.Context, id string) (Result, error) {
	f.lastIDs.Store(id, true)
	if f.block != nil {
		<-f.block
	}
	n := f.runs.Add(1)
	if n <= f.fail.Load() {
		return Result{}, errors.New("boom")
	}
	if f.onRun != nil {
		return f.onRun(ctx, id), nil
	}
	return Result{}, nil
}

func TestMarkReconcileAndSettle(t *testing.T) {
	e, db := testEngine(t)
	r := &fakeReconciler{}
	if err := e.Register("sandbox", r); err != nil {
		t.Fatal(err)
	}
	start(t, e)

	if err := e.MarkDirty(context.Background(), "sandbox", "sb-1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reconcile to run", func() bool { return r.runs.Load() == 1 })
	waitFor(t, "row to settle", func() bool { return rowCount(t, db) == 0 })
	if _, ok := r.lastIDs.Load("sb-1"); !ok {
		t.Fatal("reconciler did not receive sb-1")
	}
}

func TestMarksCoalesce(t *testing.T) {
	e, db := testEngine(t)
	r := &fakeReconciler{}
	if err := e.Register("sandbox", r); err != nil {
		t.Fatal(err)
	}
	// Mark many times BEFORE starting: must collapse into one row / one run.
	for range 25 {
		if err := e.MarkDirty(context.Background(), "sandbox", "sb-1"); err != nil {
			t.Fatal(err)
		}
	}
	if n := rowCount(t, db); n != 1 {
		t.Fatalf("expected 1 coalesced row, got %d", n)
	}
	start(t, e)
	waitFor(t, "row to settle", func() bool { return rowCount(t, db) == 0 })
	if n := r.runs.Load(); n != 1 {
		t.Fatalf("expected exactly 1 run for 25 marks, got %d", n)
	}
}

func TestRemarkDuringRunTriggersRerun(t *testing.T) {
	e, db := testEngine(t)
	r := &fakeReconciler{block: make(chan struct{})}
	if err := e.Register("sandbox", r); err != nil {
		t.Fatal(err)
	}
	start(t, e)

	if err := e.MarkDirty(context.Background(), "sandbox", "sb-1"); err != nil {
		t.Fatal(err)
	}
	// Wait until the first run is in flight (blocked), then re-mark: newer
	// intent arrived mid-run.
	waitFor(t, "first run in flight", func() bool {
		if _, ok := r.lastIDs.Load("sb-1"); !ok {
			return false
		}
		return true
	})
	if err := e.MarkDirty(context.Background(), "sandbox", "sb-1"); err != nil {
		t.Fatal(err)
	}
	r.block <- struct{}{} // finish first run
	// seq moved, so the row must survive completion and run again.
	waitFor(t, "second run", func() bool {
		select {
		case r.block <- struct{}{}:
			return true
		default:
			return false
		}
	})
	waitFor(t, "row to settle after rerun", func() bool { return rowCount(t, db) == 0 })
	if n := r.runs.Load(); n != 2 {
		t.Fatalf("expected 2 runs (initial + rerun), got %d", n)
	}
}

func TestFailureBacksOffThenConverges(t *testing.T) {
	e, db := testEngine(t)
	r := &fakeReconciler{}
	r.fail.Store(2) // fail twice, then succeed
	if err := e.Register("sandbox", r); err != nil {
		t.Fatal(err)
	}
	start(t, e)

	if err := e.MarkDirty(context.Background(), "sandbox", "sb-1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first failure recorded", func() bool {
		var row dirtyRow
		if err := db.First(&row, "resource_id = ?", "sb-1").Error; err != nil {
			return false
		}
		return row.Attempts >= 1 && row.ClaimedBy == nil && row.NotBefore.After(time.Now().Add(-time.Millisecond))
	})
	// The failure is on the row, not only in the log: ListDirty reports why.
	dirty, err := e.ListDirty(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 1 || dirty[0].LastError == nil || *dirty[0].LastError != "boom" {
		t.Fatalf("expected one dirty row with last error %q, got %+v", "boom", dirty)
	}
	// Stays dirty with backoff until it finally converges.
	waitFor(t, "eventual convergence", func() bool { return rowCount(t, db) == 0 })
	if n := r.runs.Load(); n != 3 {
		t.Fatalf("expected 3 runs (2 failures + success), got %d", n)
	}
}

func TestSuccessAfterFailureClearsLastError(t *testing.T) {
	e, db := testEngine(t)
	r := &fakeReconciler{}
	r.fail.Store(1)
	// After the one failure, succeed with a timer so the row survives and its
	// settled shape can be inspected.
	r.onRun = func(context.Context, string) Result { return RequeueAfter(time.Hour) }
	if err := e.Register("sandbox", r); err != nil {
		t.Fatal(err)
	}
	start(t, e)

	if err := e.MarkDirty(context.Background(), "sandbox", "sb-1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "armed after recovery", func() bool {
		var row dirtyRow
		if err := db.First(&row, "resource_id = ?", "sb-1").Error; err != nil {
			return false
		}
		return row.ClaimedBy == nil && row.NotBefore.After(time.Now().Add(30*time.Minute))
	})
	dirty, err := e.ListDirty(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 1 || dirty[0].Attempts != 0 || dirty[0].LastError != nil {
		t.Fatalf("expected a clean armed row (attempts 0, no error), got %+v", dirty)
	}
	if n := r.runs.Load(); n != 2 {
		t.Fatalf("expected 2 runs (failure + success), got %d", n)
	}
}

func TestMarkDirtyAtIsATimer(t *testing.T) {
	e, db := testEngine(t)
	r := &fakeReconciler{}
	if err := e.Register("provider", r); err != nil {
		t.Fatal(err)
	}
	start(t, e)

	at := time.Now().Add(300 * time.Millisecond)
	if err := e.MarkDirtyAt(context.Background(), "provider", "p-1", at); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if n := r.runs.Load(); n != 0 {
		t.Fatalf("ran %d times before not_before", n)
	}
	waitFor(t, "timer fire", func() bool { return r.runs.Load() == 1 })
	waitFor(t, "row to settle", func() bool { return rowCount(t, db) == 0 })

	// An earlier mark pulls the row forward.
	far := time.Now().Add(time.Hour)
	if err := e.MarkDirtyAt(context.Background(), "provider", "p-2", far); err != nil {
		t.Fatal(err)
	}
	if err := e.MarkDirty(context.Background(), "provider", "p-2"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pulled-forward run", func() bool { return r.runs.Load() == 2 })
}

type scanningReconciler struct {
	fakeReconciler
	scans atomic.Int32
}

func (s *scanningReconciler) ScanDirty(context.Context) ([]string, error) {
	if s.scans.Add(1) == 1 {
		return []string{"drifted-1"}, nil
	}
	return nil, nil
}

func TestScannerBackstop(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	e, err := New(db, Options{
		SingleNode:   true,
		PollInterval: 20 * time.Millisecond,
		ScanInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &scanningReconciler{}
	if err := e.Register("sandbox", r); err != nil {
		t.Fatal(err)
	}
	start(t, e)

	// No explicit mark: the scan must find and heal the drifted resource.
	waitFor(t, "scan-driven reconcile", func() bool {
		_, ok := r.lastIDs.Load("drifted-1")
		return ok
	})
}

// TestSelfMarkIsRejected pins the invariant the Result contract exists to
// enforce: a reconciler cannot mark the resource it is reconciling. Allowing it
// is unbounded CPU, not a slow retry — the seq bump makes complete's guarded
// delete miss forever, so the row is re-run with no delay and no backoff.
func TestSelfMarkIsRejected(t *testing.T) {
	e, db := testEngine(t)
	var markErr atomic.Value
	r := &fakeReconciler{onRun: func(ctx context.Context, id string) Result {
		markErr.Store(errBox{e.MarkDirty(ctx, "sandbox", id)})
		return Result{}
	}}
	if err := e.Register("sandbox", r); err != nil {
		t.Fatal(err)
	}
	start(t, e)

	if err := e.MarkDirty(context.Background(), "sandbox", "sb-1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reconcile to run", func() bool { return r.runs.Load() == 1 })
	waitFor(t, "row to settle", func() bool { return rowCount(t, db) == 0 })

	got, _ := markErr.Load().(errBox)
	if !errors.Is(got.err, ErrSelfMark) {
		t.Fatalf("self-mark error = %v, want ErrSelfMark", got.err)
	}
	// The rejection is what lets the row settle at all: one run, then done.
	if runs := r.runs.Load(); runs != 1 {
		t.Fatalf("runs = %d, want 1 (a tolerated self-mark loops)", runs)
	}
}

// TestMarkingAnotherResourceIsAllowed guards the other half: only marking
// YOURSELF is the error. Cross-resource marks are how work propagates.
func TestMarkingAnotherResourceIsAllowed(t *testing.T) {
	e, db := testEngine(t)
	var markErr atomic.Value
	r := &fakeReconciler{onRun: func(ctx context.Context, id string) Result {
		if id == "sb-1" {
			markErr.Store(errBox{e.MarkDirty(ctx, "sandbox", "sb-2")})
		}
		return Result{}
	}}
	if err := e.Register("sandbox", r); err != nil {
		t.Fatal(err)
	}
	start(t, e)

	if err := e.MarkDirty(context.Background(), "sandbox", "sb-1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both resources to run", func() bool { return r.runs.Load() == 2 })
	waitFor(t, "rows to settle", func() bool { return rowCount(t, db) == 0 })
	if got, _ := markErr.Load().(errBox); got.err != nil {
		t.Fatalf("cross-resource mark failed: %v", got.err)
	}
	if _, ok := r.lastIDs.Load("sb-2"); !ok {
		t.Fatal("sb-2 was never reconciled")
	}
}

// TestRequeueAtKeepsRowAndArmsIt is the replacement for self-marking: the row
// survives as the reconciler's timer, future-dated, claimable again at the
// deadline rather than immediately.
func TestRequeueAtKeepsRowAndArmsIt(t *testing.T) {
	e, db := testEngine(t)
	requeueAt := time.Now().Add(time.Hour)
	r := &fakeReconciler{onRun: func(context.Context, string) Result {
		return RequeueAt(requeueAt)
	}}
	if err := e.Register("sandbox", r); err != nil {
		t.Fatal(err)
	}
	start(t, e)

	if err := e.MarkDirty(context.Background(), "sandbox", "sb-1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reconcile to run", func() bool { return r.runs.Load() == 1 })

	// Long enough for several poll ticks: a row armed for an hour must not be
	// re-run, which is exactly what the old self-mark got wrong.
	time.Sleep(200 * time.Millisecond)
	if runs := r.runs.Load(); runs != 1 {
		t.Fatalf("runs = %d, want 1 (armed row must not re-run)", runs)
	}
	if n := rowCount(t, db); n != 1 {
		t.Fatalf("row count = %d, want 1 (the row IS the timer)", n)
	}
	var row dirtyRow
	if err := db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.ClaimedBy != nil {
		t.Fatalf("claimed_by = %v, want released", *row.ClaimedBy)
	}
	if !row.NotBefore.Truncate(time.Second).Equal(requeueAt.Truncate(time.Second)) {
		t.Fatalf("not_before = %v, want %v", row.NotBefore, requeueAt)
	}
}

// TestRequeueAtAssignsNotBeforeBackwards is the property MarkDirtyAt could not
// provide, and the direct cause of the runaway loop: pull-forward-only marking
// leaves a stale past not_before pinned forever, so the row is permanently
// claimable. A reconciler that just read the resource may move the deadline in
// either direction.
func TestRequeueAtAssignsNotBeforeBackwards(t *testing.T) {
	e, db := testEngine(t)
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	r := &fakeReconciler{onRun: func(context.Context, string) Result {
		return RequeueAt(future)
	}}
	if err := e.Register("sandbox", r); err != nil {
		t.Fatal(err)
	}
	start(t, e)

	// Arm the row in the past first — the exact state the looping sandboxes were in.
	if err := e.MarkDirtyAt(context.Background(), "sandbox", "sb-1", past); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reconcile to run", func() bool { return r.runs.Load() == 1 })
	time.Sleep(200 * time.Millisecond)

	if runs := r.runs.Load(); runs != 1 {
		t.Fatalf("runs = %d, want 1 (a past not_before must not survive the requeue)", runs)
	}
	var row dirtyRow
	if err := db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.NotBefore.Before(time.Now()) {
		t.Fatalf("not_before = %v, still in the past", row.NotBefore)
	}
}

// TestNewIntentBeatsRequeue: a mark that lands mid-run is newer intent and must
// win over the timer the reconciler was about to arm, or an armed deadline
// could swallow a user's request for up to its whole duration.
func TestNewIntentBeatsRequeue(t *testing.T) {
	e, db := testEngine(t)
	release := make(chan struct{})
	var once sync.Once
	r := &fakeReconciler{onRun: func(context.Context, string) Result {
		once.Do(func() {
			if err := e.MarkDirty(context.Background(), "sandbox", "sb-1"); err != nil {
				t.Error(err)
			}
			close(release)
		})
		return RequeueAt(time.Now().Add(time.Hour))
	}}
	if err := e.Register("sandbox", r); err != nil {
		t.Fatal(err)
	}
	start(t, e)

	if err := e.MarkDirty(context.Background(), "sandbox", "sb-1"); err != nil {
		t.Fatal(err)
	}
	<-release
	// The mid-run mark must re-run the reconcile now, not an hour from now.
	waitFor(t, "re-run against newer intent", func() bool { return r.runs.Load() >= 2 })
	waitFor(t, "row to arm after the re-run", func() bool { return rowCount(t, db) == 1 })
}
