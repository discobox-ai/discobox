package console

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/assets/brand"
)

// The desktop app draws every state and runs every command from the spec, so
// the spec has to name them all, in colors the design language names.
func TestSpecCoversTheConsole(t *testing.T) {
	spec := NewSpec()
	for _, s := range States {
		g, ok := spec.States[string(s)]
		if !ok || g.Glyph == "" || g.Word == "" {
			t.Errorf("state %s: %+v", s, g)
		}
	}
	for _, s := range AgentStates {
		if g, ok := spec.Agents[string(s)]; !ok || strings.TrimSpace(g.Glyph) == "" || g.Word == "" {
			t.Errorf("agent state %s: %+v", s, g)
		}
	}
	for _, glyphs := range []map[string]Glyph{spec.States, spec.Agents} {
		for state, g := range glyphs {
			if _, ok := spec.Colors[g.Color]; !ok {
				t.Errorf("%s is drawn in %q, which is no color token", state, g.Color)
			}
		}
	}
	for _, c := range brand.Colors() {
		got := spec.Colors[c.Name]
		if got.Dark != c.Dark || got.Light != c.Light || got.XTerm != c.XTerm {
			t.Errorf("color %s = %+v, want the token's %+v", c.Name, got, c)
		}
	}
	for _, d := range brand.Dimensions() {
		if got, ok := spec.Dimensions[d.Name]; !ok || got != d.Points {
			t.Errorf("dimension %s = %v, want the token's %v", d.Name, got, d.Points)
		}
	}
	if len(spec.Commands) != len(registry) {
		t.Fatalf("%d commands in the spec, %d in the registry", len(spec.Commands), len(registry))
	}
	if len(spec.Gestures) != len(gestures) {
		t.Fatalf("%d gestures in the spec, %d in the registry", len(spec.Gestures), len(gestures))
	}
	for i, c := range spec.Commands {
		if c.ID != registry[i].ID {
			t.Errorf("command %d is %s, the registry's is %s", i, c.ID, registry[i].ID)
		}
		if c.Target != "box" && c.Target != "window" {
			t.Errorf("command %s targets %q", c.ID, c.Target)
		}
	}
}

// The spec reaches the desktop app as JSON, where an absent list is an empty
// one, never null.
func TestSpecCarriesNoNulls(t *testing.T) {
	data, err := json.Marshal(NewSpec())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "null") {
		t.Errorf("the spec carries a null: %s", data)
	}
}

// A parity scenario is written in registry commands, which each console runs
// by its own binding: a step naming anything else is a scenario neither can
// run.
func TestParityScenariosNameRegistryCommands(t *testing.T) {
	files, err := filepath.Glob("testdata/parity/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no parity scenarios: %v", err)
	}
	menu, _ := LookupGesture("context menu")
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var sc struct {
			Attach bool `json:"attach"`
			Steps  []struct {
				Command string `json:"command"`
				Gesture string `json:"gesture"`
			} `json:"steps"`
		}
		if err := json.Unmarshal(data, &sc); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if len(sc.Steps) == 0 {
			t.Errorf("%s has no steps", file)
		}
		for i, step := range sc.Steps {
			if (step.Command == "") == (step.Gesture == "") {
				t.Errorf("%s step %d names %q and %q; want a command or a gesture", file, i, step.Command, step.Gesture)
				continue
			}
			if step.Gesture != "" {
				// A gesture is pressed in the list, never from a pane.
				g, ok := LookupGesture(step.Gesture)
				switch {
				case !ok:
					t.Errorf("%s step %d: %q is not a registry gesture", file, i, step.Gesture)
				case sc.Attach:
					t.Errorf("%s step %d: the gesture %s is the list's, and the scenario is in a pane", file, i, step.Gesture)
				case len(g.Bindings.TUI.Keys) == 0 || len(g.Bindings.Desktop.Keys) == 0:
					t.Errorf("%s step %d: %s is not bound in the list in both consoles", file, i, step.Gesture)
				}
				continue
			}
			c, ok := Lookup(step.Command)
			if !ok {
				t.Errorf("%s step %d: %q is not a registry command", file, i, step.Command)
				continue
			}
			if sc.Attach {
				if len(c.Bindings.TUI.Leader) == 0 || len(c.Bindings.Desktop.Keys) == 0 {
					t.Errorf("%s step %d: %s is not bound from a pane in both consoles", file, i, step.Command)
				}
				continue
			}
			// From the list: the TUI's bare key, and the desktop's shortcut — or, for a box's
			// lifecycle, its row's menu, which is how the desktop reaches a verb.
			desktop := len(c.Bindings.Desktop.Keys) > 0 || c.Lifecycle && len(menu.Bindings.Desktop.Keys) > 0
			if len(c.Bindings.TUI.Keys) == 0 || !desktop {
				t.Errorf("%s step %d: %s is not reached from the list in both consoles", file, i, step.Command)
			}
		}
	}
}
