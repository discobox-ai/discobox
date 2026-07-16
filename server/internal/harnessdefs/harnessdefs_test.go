package harnessdefs

import (
	"testing"
)

func TestIncludedDefinitionsAreConfigurableImages(t *testing.T) {
	for _, id := range []string{"codex", "claude-code", "opencode"} {
		definition, ok := DefinitionByID(id)
		if !ok || definition.Image == "" || definition.Configure == nil {
			t.Fatalf("definition %q = %#v, want configurable image", id, definition)
		}
	}
}

func TestImageEnvVar(t *testing.T) {
	cases := map[string]string{
		"codex":       "DISCOBOX_HARNESS_CODEX_IMAGE",
		"claude-code": "DISCOBOX_HARNESS_CLAUDE_CODE_IMAGE",
		"opencode":    "DISCOBOX_HARNESS_OPENCODE_IMAGE",
	}
	for id, want := range cases {
		if got := ImageEnvVar(id); got != want {
			t.Fatalf("ImageEnvVar(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestImageOverridesApplied(t *testing.T) {
	overrides := map[string]string{"codex": "discobox-harness-codex:dev-abc"}

	definition, ok := DefinitionByIDWithImages("codex", overrides)
	if !ok {
		t.Fatal("codex definition not found")
	}
	if definition.Image != "discobox-harness-codex:dev-abc" {
		t.Fatalf("image = %q, want override applied", definition.Image)
	}
	if definition.Configure == nil || definition.Configure.Image != "discobox-harness-codex:dev-abc" {
		t.Fatalf("configure image = %#v, want override applied", definition.Configure)
	}

	// Definitions without an override keep their baked-in image.
	other, ok := DefinitionByIDWithImages("opencode", overrides)
	if !ok {
		t.Fatal("opencode definition not found")
	}
	if other.Image == "discobox-harness-codex:dev-abc" || other.Image == "" {
		t.Fatalf("opencode image = %q, want baked-in", other.Image)
	}
}

func TestImageOverridesFromEnv(t *testing.T) {
	env := map[string]string{"DISCOBOX_HARNESS_CODEX_IMAGE": "  discobox-harness-codex:dev-xyz  "}
	overrides := ImageOverridesFromEnv(func(key string) string { return env[key] })
	if overrides["codex"] != "discobox-harness-codex:dev-xyz" {
		t.Fatalf("overrides[codex] = %q, want trimmed value", overrides["codex"])
	}
	if _, ok := overrides["opencode"]; ok {
		t.Fatalf("overrides = %v, want no opencode entry", overrides)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Claude Code":    "claude-code",
		"  My Harness! ": "my-harness",
		"codex":          "codex",
		"a__b--c":        "a-b-c",
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
