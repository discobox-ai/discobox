package agentdefs

import (
	"reflect"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/server/internal/model"
)

func TestClaudeCodeDefinitionTrustsPrimarySource(t *testing.T) {
	definition, ok := DefinitionByID("claude-code")
	if !ok {
		t.Fatal("claude-code definition not found")
	}
	for _, file := range definition.Files {
		if file.Path != ".claude.json" {
			continue
		}
		if !file.Template || !file.CreateOnly {
			t.Fatalf("claude state file = %#v, want create-only template", file)
		}
		if !strings.Contains(file.Content, ".source") || !strings.Contains(file.Content, "hasTrustDialogAccepted") {
			t.Fatalf("claude state template does not trust sandbox source: %s", file.Content)
		}
		return
	}
	t.Fatal("claude-code definition has no .claude.json file")
}

func TestResolveInheritsDefinitionForUnsetFields(t *testing.T) {
	config := &model.AgentConfig{
		ID:           "ac_1",
		Slug:         "codex",
		DefinitionID: "codex",
		Name:         "Codex",
		// RunCommand / RelaunchCommand / InstallCommand unset → inherit.
	}
	resolved := Resolve(config)
	if got := resolved.RunCommand; !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("run command = %#v, want [codex]", got)
	}
	if got := resolved.RelaunchCommand; !reflect.DeepEqual(got, []string{"codex", "resume", "--last"}) {
		t.Fatalf("relaunch command = %#v, want [codex resume --last]", got)
	}
	// Resolution must not mutate the stored sparse config.
	if config.RunCommand != nil {
		t.Fatalf("resolve mutated stored config: %#v", config.RunCommand)
	}
}

func TestResolveKeepsOverridesAndInheritsRest(t *testing.T) {
	config := &model.AgentConfig{
		ID:              "ac_2",
		Slug:            "codex",
		DefinitionID:    "codex",
		Name:            "Codex",
		RelaunchCommand: []string{"codex", "resume", "--session", "x"},
	}
	resolved := Resolve(config)
	if got := resolved.RelaunchCommand; !reflect.DeepEqual(got, []string{"codex", "resume", "--session", "x"}) {
		t.Fatalf("override relaunch = %#v, want the override", got)
	}
	if got := resolved.RunCommand; !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("run command = %#v, want inherited [codex]", got)
	}
}

func TestResolveLeavesCustomConfigUnchanged(t *testing.T) {
	config := &model.AgentConfig{ID: "ac_3", Slug: "mine", Name: "Mine", RunCommand: []string{"my-agent"}}
	resolved := Resolve(config)
	if !reflect.DeepEqual(resolved.RunCommand, []string{"my-agent"}) || len(resolved.RelaunchCommand) != 0 {
		t.Fatalf("custom config changed: %#v", resolved)
	}

	unknown := &model.AgentConfig{ID: "ac_4", Slug: "x", DefinitionID: "does-not-exist", RunCommand: []string{"x"}}
	if got := Resolve(unknown); len(got.RelaunchCommand) != 0 {
		t.Fatalf("unknown definition should not inherit: %#v", got)
	}
}

func TestSparsifyDropsFieldsEqualToDefinition(t *testing.T) {
	config := &model.AgentConfig{
		ID:              "ac_1",
		Slug:            "codex",
		DefinitionID:    "codex",
		Name:            "Codex",
		InstallCommand:  []string{"npm", "install", "-g", "@openai/codex"}, // == definition
		RunCommand:      []string{"codex"},                                 // == definition
		RelaunchCommand: []string{"codex", "resume", "--session", "x"},     // override
	}
	Sparsify(config)
	if config.InstallCommand != nil || config.RunCommand != nil {
		t.Fatalf("fields equal to definition were not dropped: %#v", config)
	}
	if !reflect.DeepEqual(config.RelaunchCommand, []string{"codex", "resume", "--session", "x"}) {
		t.Fatalf("override was dropped: %#v", config.RelaunchCommand)
	}
}

// A client that reads the resolved config and writes the whole object back must
// not accidentally pin every inherited field.
func TestResolveThenSparsifyRoundTrip(t *testing.T) {
	stored := &model.AgentConfig{ID: "ac_2", Slug: "codex", DefinitionID: "codex", Name: "Codex"}

	// Client fetches the full resolved config...
	resolved := Resolve(stored)
	if len(resolved.RunCommand) == 0 {
		t.Fatalf("resolved config should be full")
	}
	// ...changes one field and writes the whole object back.
	resolved.RelaunchCommand = []string{"codex", "resume"}
	Sparsify(resolved)

	if resolved.RunCommand != nil || resolved.InstallCommand != nil || resolved.Files != nil {
		t.Fatalf("round-trip pinned inherited fields: %#v", resolved)
	}
	if !reflect.DeepEqual(resolved.RelaunchCommand, []string{"codex", "resume"}) {
		t.Fatalf("changed field not kept as override: %#v", resolved.RelaunchCommand)
	}
}

func TestResolveInheritsAndSparsifyDropsSecrets(t *testing.T) {
	definitionSecrets := []model.AgentConfigSecret{{Name: "OPENAI_API_KEY", Required: true}}

	// Unset secrets inherit the definition's declared secrets.
	inherit := &model.AgentConfig{ID: "ac_1", Slug: "codex", DefinitionID: "codex", Name: "Codex"}
	if got := Resolve(inherit).Secrets; !reflect.DeepEqual(got, definitionSecrets) {
		t.Fatalf("secrets = %#v, want inherited %#v", got, definitionSecrets)
	}

	// An override that equals the definition is dropped back to nil (inherit).
	equal := &model.AgentConfig{ID: "ac_2", Slug: "codex", DefinitionID: "codex", Name: "Codex", Secrets: definitionSecrets}
	Sparsify(equal)
	if equal.Secrets != nil {
		t.Fatalf("secrets equal to definition were not dropped: %#v", equal.Secrets)
	}

	// A differing override is kept.
	override := []model.AgentConfigSecret{{Name: "OPENAI_API_KEY", Required: true}, {Name: "OPENAI_BASE_URL"}}
	custom := &model.AgentConfig{ID: "ac_3", Slug: "codex", DefinitionID: "codex", Name: "Codex", Secrets: override}
	Sparsify(custom)
	if !reflect.DeepEqual(custom.Secrets, override) {
		t.Fatalf("secret override was dropped: %#v", custom.Secrets)
	}
}

func TestSparsifyLeavesCustomConfig(t *testing.T) {
	config := &model.AgentConfig{ID: "ac_3", Slug: "mine", Name: "Mine", RunCommand: []string{"my-agent"}}
	Sparsify(config)
	if !reflect.DeepEqual(config.RunCommand, []string{"my-agent"}) {
		t.Fatalf("custom config run command dropped: %#v", config)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Claude Code":  "claude-code",
		"  My Agent! ": "my-agent",
		"codex":        "codex",
		"a__b--c":      "a-b-c",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Fatalf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateSlug(t *testing.T) {
	for _, ok := range []string{"codex", "claude-code", "a1", "x9-y"} {
		if err := ValidateSlug(ok); err != nil {
			t.Fatalf("ValidateSlug(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "-codex", "Codex", "co dex", "co_dex", "über"} {
		if err := ValidateSlug(bad); err == nil {
			t.Fatalf("ValidateSlug(%q) = nil, want error", bad)
		}
	}
}
