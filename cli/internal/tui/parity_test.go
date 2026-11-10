package tui

import (
	"encoding/json"
	"fmt"
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
		Gesture string `json:"gesture"`
		Expect  struct {
			Shells    *int    `json:"shells"`
			Focus     *string `json:"focus"`
			Maximized *bool   `json:"maximized"`
			Targets   *int    `json:"targets"`
			Selected  *int    `json:"selected"`
			Archived  *bool   `json:"archived"`
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
				var d *driver
				var m *Model
				if sc.Attach {
					d, m, _ = openWorkspace(t, ds, "enter")
				} else {
					d, m = openList(t, ds)
					m.list.moveTo(0)
				}
				for i, step := range sc.Steps {
					var keys []string
					if sc.Attach {
						keys = paneKeys(t, m, step.Command)
					} else {
						keys = listKeys(t, step.Command, step.Gesture)
					}
					for _, k := range keys {
						d.key(k)
					}
					want := step.Expect
					got := func() string {
						focus := "terminals"
						if m.onShells {
							focus = "shells"
						}
						return fmt.Sprintf("shells=%d focus=%s maximized=%v targets=%d selected=%d archived=%v",
							m.Shells.Len(), focus, m.maximized, len(m.list.targets()), m.list.selectionCount(), m.list.Filter.Archived)
					}
					d.wait(step.Command+step.Gesture, func() bool {
						focus := "terminals"
						if m.onShells {
							focus = "shells"
						}
						return (want.Shells == nil || m.Shells.Len() == *want.Shells) &&
							(want.Focus == nil || focus == *want.Focus) &&
							(want.Maximized == nil || m.maximized == *want.Maximized) &&
							(want.Targets == nil || len(m.list.targets()) == *want.Targets) &&
							(want.Selected == nil || m.list.selectionCount() == *want.Selected) &&
							(want.Archived == nil || m.list.Filter.Archived == *want.Archived)
					})
					if t.Failed() {
						t.Fatalf("step %d (%s%s, keys %v): %s", i, step.Command, step.Gesture, keys, got())
					}
				}
			})
		})
	}
}

// listKeys is the key that runs a command or a gesture from the list: its
// bare key.
func listKeys(t *testing.T, command, gesture string) []string {
	t.Helper()
	var b console.TUIBinding
	if gesture != "" {
		g, ok := console.LookupGesture(gesture)
		if !ok {
			t.Fatalf("no gesture %s in the registry", gesture)
		}
		b = g.Bindings.TUI
	} else {
		c, ok := console.Lookup(command)
		if !ok {
			t.Fatalf("no command %s in the registry", command)
		}
		b = c.Bindings.TUI
	}
	if len(b.Keys) == 0 {
		t.Fatalf("%s%s has no key in the list", command, gesture)
	}
	return b.Keys[:1]
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
	return append([]string{m.leader()}, c.Bindings.TUI.Leader[0]...)
}
