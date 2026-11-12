package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// flaky is a step that fails until fixed exists, as a download does through
// a network blip, and appends to ran each time it runs.
func flaky(fixed, ran string) string {
	return "#!/bin/sh\necho run >> " + ran + "\n[ -e " + fixed + " ] || { echo no network >&2; exit 1; }\n"
}

func lines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// A reprovision runs the features that failed, and the ones that failed with
// them, on the same box: a ready feature runs nothing, readiness stays as it
// was when only background features failed, and the box's workspace is not
// touched (ADR 26-11-11-560 §§1–2).
func TestReprovisionRunsOnlyWhatFailed(t *testing.T) {
	b := newBox(t)
	dir := t.TempDir()
	fixed, browserRan, goRan := filepath.Join(dir, "fixed"), filepath.Join(dir, "browser"), filepath.Join(dir, "go")
	b.feature(provisionspec.Core, map[string]string{provisionspec.FileName: head + "steps: []\n"})
	b.feature("go", map[string]string{provisionspec.FileName: asyncHead + "steps:\n  - name: install\n    run: install.sh\n", "install.sh": "#!/bin/sh\necho run >> " + goRan + "\n"})
	b.feature("browser", map[string]string{
		provisionspec.FileName: asyncHead + "commands: [chromium]\nsteps:\n  - name: install\n    run: install.sh\n",
		"install.sh":           flaky(fixed, browserRan),
	})
	b.feature("desktop", map[string]string{provisionspec.FileName: asyncHead + "requires: [browser]\nsteps: []\n"})
	workspace := filepath.Join(b.root, "workspace", "notes")
	if err := os.MkdirAll(filepath.Dir(workspace), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workspace, []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracker := b.tracked()
	if _, err := b.apply(); err == nil {
		t.Fatal("the first run did not fail")
	}
	status := b.status()
	if status.Features["browser"].State != provisionspec.StateFailed || status.Features["desktop"].State != provisionspec.StateFailed || !status.Ready {
		t.Fatalf("after the boot's run: %+v", status)
	}
	if !b.exists(filepath.Join(PlaceholderDir, "chromium")) {
		t.Fatal("a failed feature's placeholder was removed")
	}
	version := tracker.Changes().Version()

	if err := os.WriteFile(fixed, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rerun, started := tracker.Rerun()
	if !started {
		t.Fatal("nothing started")
	}
	for _, name := range []string{"browser", "desktop"} {
		if feature := rerun.Features[name]; feature.State != provisionspec.StatePending || !feature.Async || feature.Error != "" {
			t.Errorf("%s once rerun: %+v", name, feature)
		}
	}
	if rerun.Features["go"].State != provisionspec.StateReady || !rerun.Ready || rerun.State != provisionspec.StateProvisioning {
		t.Errorf("rerun = %+v", rerun)
	}
	if tracker.Changes().Version() == version {
		t.Error("the status wait's version did not move")
	}
	if _, again := tracker.Rerun(); again {
		t.Error("a second reprovision started beside the first")
	}
	// Readiness was not reopened: what was waiting on it still is not.
	if err := tracker.AwaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}

	b.opts.Retry = true
	outcomes := b.mustApply()
	if got := results(outcomes); got["go/install"] != "" || got["browser/install"] != Applied {
		t.Errorf("outcomes = %v", got)
	}
	status = b.status()
	for _, name := range []string{"go", "browser", "desktop"} {
		if status.Features[name].State != provisionspec.StateReady {
			t.Errorf("%s = %+v", name, status.Features[name])
		}
	}
	if status.State != provisionspec.StateReady || !status.Ready {
		t.Errorf("status = %+v", status)
	}
	if n := lines(t, goRan); n != 1 {
		t.Errorf("the ready feature ran %d times", n)
	}
	if n := lines(t, browserRan); n != 2 {
		t.Errorf("the failed feature ran %d times", n)
	}
	if b.exists(filepath.Join(PlaceholderDir, "chromium")) {
		t.Error("the placeholder outlived its feature")
	}
	if b.read("workspace/notes") != "uncommitted\n" {
		t.Error("the workspace was touched")
	}
	if _, started := tracker.Rerun(); started {
		t.Error("a reprovision with nothing failed started a run")
	}
}

// A blocking feature that failed reopens readiness: a launch arriving while
// it runs joins the run and is answered by its outcome, not the old failure
// (ADR 26-11-11-560 §4). One that fails again fails readiness again, with the
// new error.
func TestReprovisionReopensReadinessForABlockingFeature(t *testing.T) {
	b := newBox(t)
	dir := t.TempDir()
	fixed, ran, release := filepath.Join(dir, "fixed"), filepath.Join(dir, "ran"), filepath.Join(dir, "release")
	b.feature(provisionspec.Core, map[string]string{
		provisionspec.FileName: head + "steps:\n  - name: s\n    run: s.sh\n  - name: gate\n    run: gate.sh\n",
		"s.sh":                 flaky(fixed, ran),
		"gate.sh":              gate(release),
	})
	tracker := b.tracked()
	b.apply()
	if err := tracker.AwaitReady(context.Background()); err == nil {
		t.Fatal("readiness did not fail")
	}

	if err := os.WriteFile(fixed, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rerun, started := tracker.Rerun()
	if !started || rerun.Ready || rerun.Error != "" || rerun.Features[provisionspec.Core].State != provisionspec.StatePending {
		t.Fatalf("rerun = %+v, %v", rerun, started)
	}
	ready := make(chan error, 1)
	go func() { ready <- tracker.AwaitReady(context.Background()) }()
	done := make(chan error, 1)
	b.opts.Retry = true
	go func() {
		_, err := b.apply()
		done <- err
	}()
	waitFor(t, func() bool { return b.status().Features[provisionspec.Core].Step == "gate" })
	select {
	case err := <-ready:
		t.Fatalf("a launch was answered before the run decided readiness: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-ready; err != nil {
		t.Fatalf("readiness: %v", err)
	}
	if status := b.status(); status.State != provisionspec.StateReady || !status.Ready || status.Error != "" {
		t.Errorf("status = %+v", status)
	}

	// Failing again is the new failure, with readiness failed by it.
	b.feature(provisionspec.Core, map[string]string{"s.sh": "#!/bin/sh\necho disk full >&2\nexit 3\n"})
	if _, started := tracker.Rerun(); started {
		t.Fatal("started with nothing failed")
	}
	b.opts.Retry = false
	b.apply()
	if _, started := tracker.Rerun(); !started {
		t.Fatal("not started")
	}
	b.opts.Retry = true
	b.apply()
	status := b.status()
	if core := status.Features[provisionspec.Core]; core.State != provisionspec.StateFailed || !strings.Contains(core.Error, "exit status 3") {
		t.Errorf("core = %+v", core)
	}
	if err := tracker.AwaitReady(context.Background()); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("readiness: %v", err)
	}
}

// A run in progress is joined, never doubled; a run that failed before it
// had a plan reopens readiness with nothing pending, which is the caller's
// cue to run the boot's path whole (ADR 26-11-11-560 §§2–3).
func TestReprovisionJoinsARunAndReopensOneThatHadNoPlan(t *testing.T) {
	tracker := NewTracker("")
	if _, started := tracker.Rerun(); started {
		t.Fatal("started before the boot's run did")
	}
	tracker.Fail(errors.New("fetch the box's bundles: unreachable"))
	rerun, started := tracker.Rerun()
	if !started || rerun.Ready || len(rerun.Features) != 0 || rerun.State != provisionspec.StateProvisioning {
		t.Fatalf("rerun = %+v, %v", rerun, started)
	}
	if _, again := tracker.Rerun(); again {
		t.Fatal("a second run started beside the first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := tracker.AwaitReady(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("readiness while reopened: %v", err)
	}
	tracker.start(nil)
	tracker.ready(nil)
	tracker.finished()
	if err := tracker.AwaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A reprovision that cannot start its run fails the features it had made
// pending again, so nothing is left waiting forever.
func TestReprovisionThatCannotStartFailsWhatItReopened(t *testing.T) {
	tracker := NewTracker("")
	tracker.update(func(s *provisionspec.Status) {
		s.Features["go"] = provisionspec.FeatureStatus{State: provisionspec.StateFailed, Async: true, Error: "old"}
	})
	tracker.ready(nil)
	if _, started := tracker.Rerun(); !started {
		t.Fatal("not started")
	}
	tracker.Fail(errors.New("load bundle trees: gone"))
	status := tracker.Status()
	if feature := status.Features["go"]; feature.State != provisionspec.StateFailed || feature.Error != "load bundle trees: gone" {
		t.Errorf("go = %+v", feature)
	}
	if status.State != provisionspec.StateFailed {
		t.Errorf("status = %+v", status)
	}
}

// The status wait moves on a feature's state, never on a step within it
// (ADR 26-11-11-560 §6).
func TestTrackerChangesMoveOnStateNotSteps(t *testing.T) {
	tracker := NewTracker("")
	plan := &featurePlan{feature: provisionspec.Feature{Name: "go"}, done: make(chan struct{})}
	tracker.start([]*featurePlan{plan})
	version := tracker.Changes().Version()
	tracker.step("go", "install")
	if moved := tracker.Changes().Version(); moved == version {
		t.Fatal("pending to provisioning did not move it")
	} else {
		version = moved
	}
	tracker.step("go", "check")
	tracker.logPath("go", "/var/log/go.log")
	if tracker.Changes().Version() != version {
		t.Error("a step moved it")
	}
	tracker.finish(plan)
	if tracker.Changes().Version() == version {
		t.Error("finishing did not move it")
	}
	var none *Tracker
	if none.Changes() != nil {
		t.Error("a nil tracker has a signal")
	}
}

// A reprovision asked from inside the box is a file the agent takes once it
// has started or joined the run; the requester returns only then
// (ADR 26-11-11-560 §5).
func TestARetryRequestIsTakenAfterTheRunStarts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reprovision")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 4)
	watching := make(chan error, 1)
	go func() {
		watching <- WatchRetries(ctx, dir, func() { started <- struct{}{} })
	}()
	waitFor(t, func() bool {
		info, err := os.Stat(dir)
		return err == nil && info.Mode()&os.ModeSticky != 0 && info.Mode().Perm() == 0o777
	})
	if err := RequestRetry(ctx, dir); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	default:
		t.Fatal("the request was taken before the run was started")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("requests left behind: %v", entries)
	}
	cancel()
	if err := <-watching; err != nil {
		t.Fatal(err)
	}
}

// A request nothing takes is withdrawn, and says so.
func TestARetryRequestNothingTakesIsWithdrawn(t *testing.T) {
	defer func(poll, taken time.Duration) { retryPoll, retryTaken = poll, taken }(retryPoll, retryTaken)
	retryPoll, retryTaken = 10*time.Millisecond, 50*time.Millisecond
	dir := t.TempDir()
	if err := RequestRetry(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "did not take") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the request was left behind: %v", entries)
	}
}

// Reprovisioned is closed once a reprovision's run ends, and not by the
// boot's run.
func TestReprovisionedClosesWhenAReprovisionEnds(t *testing.T) {
	tracker := NewTracker("")
	boot := tracker.Reprovisioned()
	tracker.update(func(s *provisionspec.Status) {
		s.Features["go"] = provisionspec.FeatureStatus{State: provisionspec.StateFailed, Async: true}
	})
	tracker.ready(nil)
	tracker.finished()
	select {
	case <-boot:
		t.Fatal("the boot's run closed it")
	default:
	}
	if _, started := tracker.Rerun(); !started {
		t.Fatal("not started")
	}
	tracker.update(func(s *provisionspec.Status) {
		s.Features["go"] = provisionspec.FeatureStatus{State: provisionspec.StateReady, Async: true}
	})
	tracker.finished()
	select {
	case <-boot:
	default:
		t.Fatal("the reprovision's end did not close it")
	}
	if next := tracker.Reprovisioned(); next == boot {
		t.Fatal("it was not replaced")
	}
}

// A reprovision whose run cannot start ends it in one change: the signal
// fires, and the run is over, together.
func TestAReprovisionThatFailsToStartEndsOnce(t *testing.T) {
	tracker := NewTracker("")
	tracker.update(func(s *provisionspec.Status) {
		s.Features["go"] = provisionspec.FeatureStatus{State: provisionspec.StateFailed, Async: true}
	})
	tracker.ready(nil)
	tracker.finished()
	ended := tracker.Reprovisioned()
	if _, started := tracker.Rerun(); !started {
		t.Fatal("not started")
	}
	tracker.Fail(errors.New("load bundle trees: gone"))
	select {
	case <-ended:
	default:
		t.Fatal("the reprovision's end did not fire")
	}
	if _, started := tracker.Rerun(); !started {
		t.Fatal("the failed run still reads as in progress")
	}
}
