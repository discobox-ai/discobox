package tui

import (
	"context"
	"strings"
	"testing"
	"time"
)

func makeConfigs() []HarnessConfig {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return []HarnessConfig{
		{ID: "hc_a", Name: "codex", Slug: "codex", BuiltIn: true, Configured: true, Default: true, Created: base, Updated: base},
		{ID: "hc_b", Name: "custom-one", Slug: "custom", Image: "reg/img:1", Created: base.Add(time.Minute), Updated: base.Add(time.Minute)},
	}
}

func newHarnessesTestScreen(f *fakeSource) *harnessesScreen {
	s := newHarnessesScreen(context.Background(), f, defaultKeyMap(), defaultStyles())
	s.setSize(120, 20)
	s.applyConfigs(f.configs)
	return s
}

// enterAgentTable is a no-op now that the table holds focus from the start; it is
// kept so the tests read as "put the cursor on row 0".
func enterAgentTable(_ *testing.T, s *harnessesScreen) *harnessesScreen {
	s.table.SetCursor(0)
	return s
}

func TestHarnessesTableShowsNamesAndDefault(t *testing.T) {
	f := &fakeSource{configs: makeConfigs()}
	s := newHarnessesTestScreen(f)
	view := s.View(120, 30)
	for _, want := range []string{"NAME", "CONFIGURED", "codex", "custom-one", "★", "yes", "no"} {
		if !strings.Contains(view, want) {
			t.Fatalf("table view missing %q\n%s", want, view)
		}
	}
}

func TestHarnessesTableFallsBackToSlugThenID(t *testing.T) {
	// name empty -> slug; both empty -> id.
	f := &fakeSource{configs: []HarnessConfig{
		{ID: "hc_a", Slug: "only-slug"},
		{ID: "hc_bare"},
	}}
	s := newHarnessesTestScreen(f)
	view := s.View(120, 30)
	if !strings.Contains(view, "only-slug") || !strings.Contains(view, "hc_bare") {
		t.Fatalf("table should fall back to slug then id\n%s", view)
	}
}

func TestHarnessesEnterConfirmsThenSetsDefault(t *testing.T) {
	f := &fakeSource{configs: makeConfigs()}
	s := newHarnessesTestScreen(f)
	// Into the table, then down to the second (non-default) row; enter asks to confirm.
	s = enterAgentTable(t, s)
	next, _ := s.Update(keyPress("down"))
	s = next.(*harnessesScreen)
	next, cmd := s.Update(keyPress("enter"))
	s = next.(*harnessesScreen)
	if s.confirm != confirmSetDefault {
		t.Fatalf("enter on a non-default should open the set-default confirm, got %v", s.confirm)
	}
	if runCmd(cmd) != nil {
		t.Fatal("enter should not set the default before confirmation")
	}
	if len(f.setDefaults) != 0 {
		t.Fatalf("set default must wait for confirmation, got %v", f.setDefaults)
	}
	// Confirm with y.
	_, cmd = s.Update(keyPress("y"))
	if _, ok := runCmd(cmd).(statusMsg); !ok {
		t.Fatal("confirming should report a statusMsg")
	}
	if ids := f.setDefaults; len(ids) != 1 || ids[0] != "hc_b" {
		t.Fatalf("setDefaults = %v, want [hc_b]", ids)
	}
}

func TestHarnessesSetDefaultCancel(t *testing.T) {
	f := &fakeSource{configs: makeConfigs()}
	s := newHarnessesTestScreen(f)
	s = enterAgentTable(t, s)
	next, _ := s.Update(keyPress("down"))
	s = next.(*harnessesScreen)
	next, _ = s.Update(keyPress("enter"))
	s = next.(*harnessesScreen)
	next, _ = s.Update(keyPress("n"))
	s = next.(*harnessesScreen)
	if s.confirm != confirmNone {
		t.Fatal("n should cancel the set-default confirm")
	}
	if len(f.setDefaults) != 0 {
		t.Fatalf("cancel should not set default, got %v", f.setDefaults)
	}
}

func TestHarnessesEnterOnDefaultIsNoop(t *testing.T) {
	f := &fakeSource{configs: makeConfigs()}
	s := newHarnessesTestScreen(f)
	// Row 0 is already the default: no confirm dialog, just a status note.
	s = enterAgentTable(t, s)
	next, cmd := s.Update(keyPress("enter"))
	s = next.(*harnessesScreen)
	if s.confirm != confirmNone {
		t.Fatal("enter on the current default should not open a confirm")
	}
	if _, ok := runCmd(cmd).(statusMsg); !ok {
		t.Fatal("expected a statusMsg noting it is already the default")
	}
	if len(f.setDefaults) != 0 {
		t.Fatalf("set default should not be called for the current default, got %v", f.setDefaults)
	}
}

func TestHarnessesConfigureEmitsRunConfigure(t *testing.T) {
	f := &fakeSource{configs: makeConfigs()}
	s := newHarnessesTestScreen(f)
	s = enterAgentTable(t, s)
	next, _ := s.Update(keyPress("down")) // cursor on hc_b
	s = next.(*harnessesScreen)
	_, cmd := s.Update(keyPress("c"))
	msg := runCmd(cmd)
	run, ok := msg.(runConfigureMsg)
	if !ok {
		t.Fatalf("c returned %T, want runConfigureMsg", msg)
	}
	if run.harness.ID != "hc_b" {
		t.Fatalf("configure target = %q, want hc_b", run.harness.ID)
	}
}

func TestHarnessesBackToSandboxes(t *testing.T) {
	f := &fakeSource{configs: makeConfigs()}
	s := newHarnessesTestScreen(f)
	_, cmd := s.Update(keyPress("esc"))
	if _, ok := runCmd(cmd).(backMsg); !ok {
		t.Fatal("esc should emit backMsg to return to the sandbox list")
	}
}
