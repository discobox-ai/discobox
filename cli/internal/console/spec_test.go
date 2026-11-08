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
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var sc struct {
			Steps []struct {
				Command string `json:"command"`
			} `json:"steps"`
		}
		if err := json.Unmarshal(data, &sc); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if len(sc.Steps) == 0 {
			t.Errorf("%s has no steps", file)
		}
		for i, step := range sc.Steps {
			c, ok := Lookup(step.Command)
			if !ok {
				t.Errorf("%s step %d: %q is not a registry command", file, i, step.Command)
				continue
			}
			if len(c.Bindings.TUI.Leader) == 0 || len(c.Bindings.Desktop.Keys) == 0 {
				t.Errorf("%s step %d: %s is not bound from a pane in both consoles", file, i, step.Command)
			}
		}
	}
}
