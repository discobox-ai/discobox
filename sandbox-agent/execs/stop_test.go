package execs

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// Stop is not Delete. Delete tears the record down; Stop ends the run and keeps
// it, which is what lets a long-lived exec — a service, a terminal — be started
// again under the same identity (ADR 0038, ADR 0070 §6).
func TestStopEndsTheRunAndKeepsTheRecord(t *testing.T) {
	units := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       units,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	stopped, err := manager.Stop(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.Status != StatusExited {
		t.Errorf("status = %q, want exited", stopped.Status)
	}
	if !stopped.Stopped {
		t.Error("a stopped exec must record that it was stopped, not left to look like a crash")
	}
	if stopped.ExitedAt == nil {
		t.Error("a stopped exec must record when it ended")
	}
	// The record survives, which is the whole difference from Delete.
	current, ok := manager.Get(created.ID)
	if !ok {
		t.Fatal("the exec record was removed; Stop must keep it")
	}
	if !current.Stopped || current.Status != StatusExited {
		t.Fatalf("record = %+v, want a stopped, exited exec", current)
	}
	// The shim's socket is gone, so an attach reports the session as over
	// rather than dialing something dead.
	if _, err := os.Stat(created.SocketPath); !os.IsNotExist(err) {
		t.Errorf("socket still present: %v", err)
	}
}

// A stopped exec is not lost. A sweep finds the unit gone and calls a live exec
// lost — true of a unit that vanished underneath one, and wrong for one that
// was asked to stop.
func TestStopSurvivesSweep(t *testing.T) {
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       &fakeUnitManager{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := manager.Stop(context.Background(), created.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	manager.Sweep(context.Background())

	current, ok := manager.Get(created.ID)
	if !ok {
		t.Fatal("exec not found after the sweep")
	}
	if current.Status != StatusExited || !current.Stopped {
		t.Fatalf("status = %q stopped = %t, want exited and stopped", current.Status, current.Stopped)
	}
	if current.Error != "" {
		t.Errorf("error = %q, want none: being stopped is not a failure", current.Error)
	}
}

// A relaunch is a new run, so it clears the previous one's stop.
func TestRelaunchClearsStopped(t *testing.T) {
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       &fakeUnitManager{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := manager.Stop(context.Background(), created.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	relaunched, err := manager.Relaunch(context.Background(), RelaunchRequest{ID: created.ID})
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if relaunched.Stopped {
		t.Error("a relaunched exec must not still report the previous run's stop")
	}
	if relaunched.Status != StatusStarting {
		t.Errorf("status = %q, want starting", relaunched.Status)
	}
	if relaunched.ID != created.ID {
		t.Errorf("relaunch changed the exec id: %q then %q", created.ID, relaunched.ID)
	}
}

func TestStopUnknownExec(t *testing.T) {
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       &fakeUnitManager{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if _, err := manager.Stop(context.Background(), "ex_nope"); err == nil {
		t.Fatal("stop succeeded, want ErrNotFound")
	}
}

// A stop must survive the observation the stop itself provokes. Stopping the
// unit makes systemd report a change, so the watcher reads the exec's record —
// still running — and then spends a D-Bus round trip asking what became of the
// unit. By the time that answer comes back Stop has already recorded the stop,
// and writing the conclusion drawn from the pre-stop record puts the exec back
// to `lost` with "exec unit is no longer loaded": a service left reading
// `failed`, permanently, because nothing reconciles it again.
func TestStopSurvivesAnInFlightUnitObservation(t *testing.T) {
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       &fakeUnitManager{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The run the stop interrupts: a service that has been up for a while.
	running := created
	startedAt := time.Now().UTC().Add(-time.Minute)
	running.Status = StatusRunning
	running.StartedAt = &startedAt
	running.PID = 4321
	if err := writeRuntime(running.RuntimePath, running); err != nil {
		t.Fatalf("write runtime: %v", err)
	}
	// What the watcher read before the stop, and is still holding.
	observed := running

	if _, err := manager.Stop(context.Background(), created.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// The round trip lands now, against a record that has already moved on.
	manager.refreshExec(context.Background(), observed, true)

	current, ok := manager.Get(created.ID)
	if !ok {
		t.Fatal("exec not found after the observation")
	}
	if current.Status != StatusExited || !current.Stopped {
		t.Fatalf("status = %q stopped = %t error = %q, want exited and stopped", current.Status, current.Stopped, current.Error)
	}
	if current.Error != "" {
		t.Errorf("error = %q, want none: being stopped is not a failure", current.Error)
	}
}

// Delete stops the unit too, and so provokes the same observation. Its answer
// lands after the runtime file is gone, and writing it recreates the file: a
// deleted exec back in the listing as a lost one, its transcript already
// discarded, and a primary terminal the terminal layer may try to revive.
func TestDeleteSurvivesAnInFlightUnitObservation(t *testing.T) {
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       &fakeUnitManager{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	running := created
	startedAt := time.Now().UTC().Add(-time.Minute)
	running.Status = StatusRunning
	running.StartedAt = &startedAt
	running.PID = 4321
	if err := writeRuntime(running.RuntimePath, running); err != nil {
		t.Fatalf("write runtime: %v", err)
	}
	observed := running

	if err := manager.Delete(context.Background(), created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	manager.refreshExec(context.Background(), observed, true)

	if _, err := os.Stat(created.RuntimePath); !os.IsNotExist(err) {
		t.Fatalf("runtime file recreated after delete: %v", err)
	}
	if _, ok := manager.Get(created.ID); ok {
		t.Fatal("a deleted exec is back in the listing")
	}
}

// Delete is teardown, so nothing of the exec may be read back: not its runtime
// file and not its durable record. Leaving the record behind surfaces the
// deleted exec again from every listing, as a lost one that never goes away,
// because the durable record is exactly what a reboot-stranded exec looks like.
func TestDeleteRemovesTheDurableRecord(t *testing.T) {
	audit := newRecordingAudit()
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       &fakeUnitManager{},
		Audit:       audit,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := manager.Delete(context.Background(), created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if got, ok := manager.Get(created.ID); ok {
		t.Fatalf("a deleted exec is still readable: %+v", got)
	}
	for _, exec := range manager.List() {
		if exec.ID == created.ID {
			t.Fatalf("a deleted exec is still listed: %+v", exec)
		}
	}
	if _, err := os.Stat(created.RuntimePath); !os.IsNotExist(err) {
		t.Fatalf("reading the exec back recreated its runtime file: %v", err)
	}
}

// An exec id can be reused — the primary terminal's is fixed, and a terminal
// whose install fails is deleted and created again under it. The new exec is
// the one its request describes, not the deleted one's identity, which a
// durable record left behind would keep: that record is written once and
// never overwritten.
func TestCreateAfterDeleteTakesTheNewIdentity(t *testing.T) {
	audit := newRecordingAudit()
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       &fakeUnitManager{},
		Audit:       audit,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	first, err := manager.Create(context.Background(), CreateRequest{
		ID:       "primary",
		Command:  []string{"claude"},
		Metadata: map[string]string{"harnessId": "claude"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := manager.Delete(context.Background(), first.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := manager.Create(context.Background(), CreateRequest{
		ID:       "primary",
		Command:  []string{"codex"},
		Metadata: map[string]string{"harnessId": "codex"},
	}); err != nil {
		t.Fatalf("create again: %v", err)
	}
	if record := audit.records["primary"]; record.Metadata["harnessId"] != "codex" {
		t.Fatalf("durable record harnessId = %q, want codex", record.Metadata["harnessId"])
	}
	// The runtime file is where a shim write can drop metadata; the durable
	// record is what restores it, so lose the file's copy and read it back.
	got, ok := manager.Get("primary")
	if !ok {
		t.Fatal("recreated exec not found")
	}
	got.Metadata = nil
	if err := writeRuntime(got.RuntimePath, got); err != nil {
		t.Fatalf("write runtime: %v", err)
	}
	if got, _ := manager.Get("primary"); got.Metadata["harnessId"] != "codex" {
		t.Fatalf("hydrated harnessId = %q, want codex", got.Metadata["harnessId"])
	}
}

// The durable record is the removal that can fail, so it goes first: a Delete
// that reports failure has not already made the exec look gone. What is left
// is an ended exec with its record whole, which a retry finds and finishes.
func TestDeleteThatCannotRemoveTheRecordLeavesTheExec(t *testing.T) {
	audit := newRecordingAudit()
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       &fakeUnitManager{},
		Audit:       audit,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	audit.deleteErr = errors.New("database is locked")
	if err := manager.Delete(context.Background(), created.ID); err == nil {
		t.Fatal("delete succeeded, want the record removal's failure")
	}
	if _, ok := audit.records[created.ID]; !ok {
		t.Fatal("the durable record is gone after a failed delete")
	}
	if _, err := os.Stat(created.RuntimePath); err != nil {
		t.Fatalf("runtime file removed by a failed delete: %v", err)
	}
	// Still writable: the failure did not mark the exec deleted.
	if _, err := manager.Stop(context.Background(), created.ID); err != nil {
		t.Fatalf("stop after a failed delete: %v", err)
	}

	audit.deleteErr = nil
	if err := manager.Delete(context.Background(), created.ID); err != nil {
		t.Fatalf("retry delete: %v", err)
	}
	if _, ok := manager.Get(created.ID); ok {
		t.Fatal("the exec survived the retried delete")
	}
}

// An exec's observed status row is removed with its record, and a status write
// already in flight when Delete ran must not put it back: the row would carry
// the deleted run's status into the history of the next exec with that id.
func TestDeleteIsNotFollowedByAStatusWrite(t *testing.T) {
	audit := newRecordingAudit()
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       &fakeUnitManager{},
		Audit:       audit,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	running := created
	startedAt := time.Now().UTC().Add(-time.Minute)
	running.Status = StatusRunning
	running.StartedAt = &startedAt
	if err := writeRuntime(running.RuntimePath, running); err != nil {
		t.Fatalf("write runtime: %v", err)
	}
	exited := running
	exited.Status = StatusExited

	if err := manager.Delete(context.Background(), created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	before := audit.observed[created.ID]
	// Both refresh paths: a live record observed against its gone unit, and
	// a settled one simply recorded.
	manager.refreshExec(context.Background(), running, true)
	manager.refreshExec(context.Background(), exited, true)
	// And a lifecycle write that began before the deletion.
	if err := manager.writeRecord(context.Background(), running); !errors.Is(err, ErrNotFound) {
		t.Fatalf("write after delete err = %v, want ErrNotFound", err)
	}
	if after := audit.observed[created.ID]; after != before {
		t.Fatalf("status written %d time(s) after the delete", after-before)
	}
}
