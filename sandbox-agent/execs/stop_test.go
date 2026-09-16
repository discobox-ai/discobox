package execs

import (
	"context"
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
