package registry

import (
	"testing"

	"github.com/discobox-ai/discobox/harness"
	claudecode "github.com/discobox-ai/discobox/harness/claude-code"
	codexcli "github.com/discobox-ai/discobox/harness/codex-cli"
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
	for _, id := range []string{"claude-code", "codex"} {
		definition, ok := byID[id]
		if !ok {
			t.Fatalf("missing definition %q", id)
		}
		if definition.Configure == nil {
			t.Fatalf("definition %q must be configurable", id)
		}
	}
	shell, ok := byID["shell"]
	if !ok {
		t.Fatal("missing definition \"shell\"; it is the end of the harness resolution chain")
	}
	if shell.Configure != nil {
		t.Fatalf("shell definition = %#v, want no configure flow: it collects no credentials", shell)
	}
}

func TestDriverForHarnessSelectsByImageType(t *testing.T) {
	cases := []struct {
		harness harness.Harness
		want    string
	}{
		{harness: harness.Harness{TypeID: "claude-code"}, want: claudecode.Driver{}.ID()},
		{harness: harness.Harness{TypeID: "codex"}, want: codexcli.Driver{}.ID()},
	}
	for _, tc := range cases {
		got := DriverForHarness(tc.harness)
		if len(got) != 1 || got[0].ID() != tc.want {
			t.Fatalf("DriverForHarness(%#v) = %#v, want %s", tc.harness, got, tc.want)
		}
	}
}

func TestDriverForHarnessFallsBackForUnknownType(t *testing.T) {
	for _, harness := range []harness.Harness{
		{},
		{TypeID: "unknown"},
		{ID: "claude-code"},
	} {
		if got := DriverForHarness(harness); len(got) != len(DefaultDrivers()) {
			t.Fatalf("DriverForHarness(%#v) = %#v, want all default drivers", harness, got)
		}
	}
}
