package execs

import (
	"context"
	"os"
	"testing"
)

// stopRecordingUnitManager records stops so a test can assert the previous
// run was fenced before the new generation started.
type stopRecordingUnitManager struct {
	unloadedUnitManager
	stops []string
}

func (m *stopRecordingUnitManager) Stop(_ context.Context, unit string) error {
	m.stops = append(m.stops, unit)
	return nil
}

// The reboot scenario ADR 0038 exists for: a terminal's transient unit did not
// survive, the record reads lost, and relaunching revives it under the same
// exec id — new unit generation, reset run state, the resume command as the
// new startup command — instead of minting a sibling record.
func TestManagerRelaunchRevivesLostExecInPlace(t *testing.T) {
	units := &stopRecordingUnitManager{}
	audit := newRecordingAudit()
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       units,
		Audit:       audit,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{
		Shell:          true,
		StartupCommand: []string{"claude", "do a thing"},
		TTY:            true,
		Metadata:       map[string]string{"harnessId": "claude", "primary": "true"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The reboot: tmpfs runtime file gone, durable record left, unit unloaded.
	if err := os.Remove(created.RuntimePath); err != nil {
		t.Fatalf("remove runtime: %v", err)
	}
	if got, ok := manager.Get(created.ID); !ok || got.Status != StatusLost {
		t.Fatalf("pre-relaunch status = %q (found %v), want lost", got.Status, ok)
	}

	revived, err := manager.Relaunch(context.Background(), RelaunchRequest{
		ID:             created.ID,
		Env:            map[string]string{"FRESH": "yes"},
		StartupCommand: []string{"claude", "--continue"},
	})
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if revived.ID != created.ID {
		t.Fatalf("revived id = %q, want the same identity %q", revived.ID, created.ID)
	}
	if revived.Status != StatusStarting || revived.ExitedAt != nil || revived.Error != "" || revived.PID != 0 {
		t.Fatalf("revived = %#v, want run state reset to starting", revived)
	}
	if want := "discobox-exec-" + created.ID + "-g2"; revived.Unit != want {
		t.Fatalf("unit = %q, want next generation %q", revived.Unit, want)
	}
	if len(units.stops) != 1 || units.stops[0] != created.Unit {
		t.Fatalf("stops = %v, want the previous run's unit fenced first", units.stops)
	}
	if len(units.starts) != 2 {
		t.Fatalf("unit starts = %d, want the original plus the revival", len(units.starts))
	}
	start := units.starts[1]
	if start.Unit != revived.Unit || start.ID != created.ID {
		t.Fatalf("start = %#v, want same exec id under the new unit", start)
	}
	if len(start.Command) == 0 || start.Command[0] != created.Command[0] {
		t.Fatalf("command = %v, want the recorded shell argv %v reused", start.Command, created.Command)
	}
	if len(start.StartupCommand) != 2 || start.StartupCommand[1] != "--continue" {
		t.Fatalf("startupCommand = %v, want the caller's resume command", start.StartupCommand)
	}
	if start.Env["FRESH"] != "yes" {
		t.Fatalf("env = %#v, want the caller's freshly resolved env", start.Env)
	}
	if start.Metadata["harnessId"] != "claude" || start.Metadata["primary"] != "true" {
		t.Fatalf("metadata = %#v, want identity metadata carried over", start.Metadata)
	}
	// The revived record must read as one exec, not two: same id, starting.
	if got, ok := manager.Get(created.ID); !ok || got.Status != StatusStarting {
		t.Fatalf("post-relaunch status = %q (found %v), want starting", got.Status, ok)
	}
	if list := manager.List(); len(list) != 1 {
		t.Fatalf("list = %d execs, want exactly one identity", len(list))
	}
}

// A live exec is never fenced by Relaunch: reviving is only for ended runs.
func TestManagerRelaunchLeavesLiveExecAlone(t *testing.T) {
	units := &stopRecordingUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       units,
		Audit:       newRecordingAudit(),
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Command: []string{"codex"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := manager.Relaunch(context.Background(), RelaunchRequest{ID: created.ID})
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if got.Unit != created.Unit || len(units.stops) != 0 || len(units.starts) != 1 {
		t.Fatalf("live exec disturbed: unit %q→%q, stops %v, starts %d", created.Unit, got.Unit, units.stops, len(units.starts))
	}
}

func TestNextUnitGeneration(t *testing.T) {
	cases := []struct{ id, current, want string }{
		{"abc", "discobox-exec-abc", "discobox-exec-abc-g2"},
		{"abc", "discobox-exec-abc-g2", "discobox-exec-abc-g3"},
		{"abc", "discobox-exec-abc-g9", "discobox-exec-abc-g10"},
		// A unit name that predates or mangles the scheme restarts the count.
		{"abc", "", "discobox-exec-abc-g2"},
		{"abc", "discobox-exec-abc-gx", "discobox-exec-abc-g2"},
	}
	for _, c := range cases {
		if got := nextUnitGeneration(c.id, c.current); got != c.want {
			t.Fatalf("nextUnitGeneration(%q, %q) = %q, want %q", c.id, c.current, got, c.want)
		}
	}
}
