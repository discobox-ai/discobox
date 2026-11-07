package tui

import (
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The desktop app draws every state and runs every action the window does, so
// the spec has to name them all.
func TestSpecCoversTheWindow(t *testing.T) {
	spec := Spec("ctrl+a")
	for _, s := range []console.State{console.StateRunning, console.StateStarting, console.StateRepairing, console.StateStopped, console.StateSuspended, console.StateArchived, console.StateError} {
		if g, ok := spec.States[string(s)]; !ok || g.Glyph == "" {
			t.Errorf("state %s has no glyph", s)
		}
	}
	ids := map[string]bool{}
	for _, c := range spec.Commands {
		if ids[c.ID] {
			t.Errorf("command %s twice", c.ID)
		}
		ids[c.ID] = true
		if len(c.Keys["tui"]) == 0 {
			t.Errorf("command %s has no key in the window", c.ID)
		}
	}
	for _, a := range ActionsFor([]console.Sandbox{{State: console.StateRunning}}) {
		if a.Command == "" || !ids[a.Command] {
			t.Errorf("action %q (%s) is not a command in the spec", a.Label, a.Key)
		}
	}
	if got := colorHex("220"); got != "#ffd700" {
		t.Errorf("colorHex(220) = %s, want #ffd700", got)
	}
}
