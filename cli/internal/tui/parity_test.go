package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
)

// The parity scenarios in testdata/parity are run by both consoles: here, with
// each command's keys from this window's keymap in the spec, and by the desktop
// app (console-rune) with its own. A scenario says what the workspace looks
// like after each command, in terms both can answer, so the two are held to
// one behavior while each keeps its platform's keys.

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
	files, err := filepath.Glob("testdata/parity/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no parity scenarios: %v", err)
	}
	spec := Spec("ctrl+a")
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
					keys := tuiKeys(t, spec, step.Command)
					for _, k := range keys {
						d.key(k)
					}
					want := step.Expect
					d.wait(step.Command, func() bool {
						focus := "terminals"
						if m.onShells {
							focus = "shells"
						}
						return m.shells.len() == want.Shells && focus == want.Focus && m.maximized == want.Maximized
					})
					if t.Failed() {
						t.Fatalf("step %d (%s, keys %v): shells=%d onShells=%v maximized=%v", i, step.Command, keys, m.shells.len(), m.onShells, m.maximized)
					}
				}
			})
		})
	}
}

// tuiKeys is the key presses a command's first window binding is: "leader+s"
// is the leader, then s.
func tuiKeys(t *testing.T, spec UXSpec, id string) []string {
	t.Helper()
	for _, c := range spec.Commands {
		if c.ID != id {
			continue
		}
		if len(c.Keys["tui"]) == 0 {
			t.Fatalf("command %s has no key in the window", id)
		}
		k := c.Keys["tui"][0]
		if rest, ok := strings.CutPrefix(k, "leader+"); ok {
			return []string{spec.Leader, rest}
		}
		return []string{k}
	}
	t.Fatalf("no command %s in the spec", id)
	return nil
}
