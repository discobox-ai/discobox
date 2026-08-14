package harnessdefs

import (
	"testing"
)

func TestSeedsCoverIncludedHarnesses(t *testing.T) {
	seeds := Seeds(nil)
	bySlug := map[string]Seed{}
	for _, seed := range seeds {
		bySlug[seed.Slug] = seed
	}
	for _, slug := range []string{"codex", "claude-code"} {
		seed, ok := bySlug[slug]
		if !ok || seed.Image == "" || seed.Name == "" {
			t.Fatalf("seed %q = %#v, want a named seed with an image", slug, seed)
		}
	}
}

func TestImageEnvVar(t *testing.T) {
	cases := map[string]string{
		"codex":       "DISCOBOX_HARNESS_CODEX_IMAGE",
		"claude-code": "DISCOBOX_HARNESS_CLAUDE_CODE_IMAGE",
	}
	for id, want := range cases {
		if got := ImageEnvVar(id); got != want {
			t.Fatalf("ImageEnvVar(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestSeedsApplyImageOverrides(t *testing.T) {
	overrides := map[string]string{"codex": "discobox-harness-codex:dev-abc"}
	bySlug := map[string]Seed{}
	for _, seed := range Seeds(overrides) {
		bySlug[seed.Slug] = seed
	}
	if got := bySlug["codex"].Image; got != "discobox-harness-codex:dev-abc" {
		t.Fatalf("codex image = %q, want override applied", got)
	}
	// Harnesses without an override keep their baked-in image.
	if got := bySlug["claude-code"].Image; got == "discobox-harness-codex:dev-abc" || got == "" {
		t.Fatalf("claude-code image = %q, want baked-in", got)
	}
}

func TestImageOverridesFromEnv(t *testing.T) {
	env := map[string]string{"DISCOBOX_HARNESS_CODEX_IMAGE": "  discobox-harness-codex:dev-xyz  "}
	overrides := ImageOverridesFromEnv(func(key string) string { return env[key] })
	if overrides["codex"] != "discobox-harness-codex:dev-xyz" {
		t.Fatalf("overrides[codex] = %q, want trimmed value", overrides["codex"])
	}
	if _, ok := overrides["claude-code"]; ok {
		t.Fatalf("overrides = %v, want no claude-code entry", overrides)
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
