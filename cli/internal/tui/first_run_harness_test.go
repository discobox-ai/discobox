package tui

import (
	"strings"
	"testing"
)

func TestFirstRunOffersShellForThisBox(t *testing.T) {
	t.Parallel()
	ds := noDefaultSource(t)
	m := newTestModel(t, ds)
	send(t, m, keyPress("enter"), keyPress("enter"))

	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatal("first run should open the harness picker")
	}
	for _, item := range m.dialog.items {
		if strings.Contains(strings.ToLower(item.label), "shell") {
			send(t, m, keyPress(item.key))
			if len(ds.didHarness) != 0 {
				t.Fatalf("project harness changes = %v, want none", ds.didHarness)
			}
			if len(ds.runs) != 1 || ds.runs[0].Harness != "shell" {
				t.Fatalf("runs = %v, want one run with shell", ds.runs)
			}
			return
		}
	}
	t.Fatalf("first-run picker has no Shell choice: %v", m.dialog.items)
}
