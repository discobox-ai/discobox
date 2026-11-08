package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The parity scenarios in the core's testdata/parity are run by both consoles
// (ADR 26-11-07-598 §5): here, pressing each command's binding in this
// window from the registry, and by the desktop app (desktop/) with its own. A
// scenario says what the workspace looks like after each command, in terms
// both can answer, so the two are held to one behavior while each keeps its
// platform's keys.

type parityScenario struct {
	Name   string `json:"name"`
	Attach bool   `json:"attach"`
	Steps  []struct {
		Command string `json:"command"`
		Expect  struct {
			Shells    int    `json:"shells"`
			Focus     string `json:"focus"`
			Maximized bool   `json:"maximized"`
		} `json:"expect"`
	} `json:"steps"`
}

func TestParityScenarios(t *testing.T) {
	files, err := filepath.Glob("../console/testdata/parity/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no parity scenarios: %v", err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var sc parityScenario
		if err := json.Unmarshal(data, &sc); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		t.Run(filepath.Base(file), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ds := newFakeSource(testSandboxes()...)
				d, m, _ := openWorkspace(t, ds, "enter")
				for i, step := range sc.Steps {
					keys := paneKeys(t, m, step.Command)
					for _, k := range keys {
						d.key(k)
					}
					want := step.Expect
					d.wait(step.Command, func() bool {
						focus := "terminals"
						if m.onShells {
							focus = "shells"
						}
						return m.Shells.Len() == want.Shells && focus == want.Focus && m.maximized == want.Maximized
					})
					if t.Failed() {
						t.Fatalf("step %d (%s, keys %v): shells=%d onShells=%v maximized=%v", i, step.Command, keys, m.Shells.Len(), m.onShells, m.maximized)
					}
				}
			})
		})
	}
}

// paneKeys is the key presses that run a command from a workspace's focused
// pane: the leader, then the command's leader sequence.
func paneKeys(t *testing.T, m *Model, id string) []string {
	t.Helper()
	c, ok := console.Lookup(id)
	if !ok {
		t.Fatalf("no command %s in the registry", id)
	}
	if len(c.Bindings.TUI.Leader) == 0 {
		t.Fatalf("command %s has no leader binding to reach it from a pane", id)
	}
	return append([]string{m.leader()}, c.Bindings.TUI.Leader...)
}
