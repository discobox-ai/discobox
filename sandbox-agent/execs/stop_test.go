package execs

import (
	"context"
	"os"
	"testing"
)

// Stop is not Delete. Delete tears the record down; Stop ends the run and keeps
// it, which is what lets a long-lived exec — a service, a terminal — be started
// again under the same identity (ADR 0038, ADR 0068 §6).
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

// A stopped exec is not lost. The reconcile loop finds the unit gone and calls
// a live exec lost — true of a unit that vanished underneath one, and wrong for
// one that was asked to stop.
func TestStopSurvivesReconcile(t *testing.T) {
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       &unloadedUnitManager{},
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
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	current, ok := manager.Get(created.ID)
	if !ok {
		t.Fatal("exec not found after reconcile")
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
