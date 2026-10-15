package registry

import (
	"testing"

	"github.com/discobox-ai/discobox/harness"
)

func TestDefinitionsCoverKnownHarnesses(t *testing.T) {
	definitions := Definitions()
	if len(definitions) != len(DefaultDrivers()) {
		t.Fatalf("definitions = %d, want one per default driver (%d)", len(definitions), len(DefaultDrivers()))
	}
	byID := map[string]harness.Definition{}
	for _, definition := range definitions {
		if definition.ID == "" || definition.Name == "" || definition.Image == "" {
			t.Fatalf("definition %#v must identify an image", definition)
		}
		byID[definition.ID] = definition
	}
	for _, id := range []string{"claude-code", "codex", "opencode", "copilot", "omp", "pi"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("missing definition %q", id)
		}
	}
	if _, ok := byID["shell"]; !ok {
		t.Fatal("missing definition \"shell\"; it is the end of the harness resolution chain")
	}
}
