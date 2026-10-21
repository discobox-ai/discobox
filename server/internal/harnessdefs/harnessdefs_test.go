package harnessdefs

import (
	"testing"
)

func TestSeedsCoverIncludedHarnesses(t *testing.T) {
	seeds := Seeds(References{}, false)
	bySlug := map[string]Seed{}
	for _, seed := range seeds {
		bySlug[seed.Slug] = seed
	}
	for _, slug := range []string{"codex", "claude-code"} {
		seed, ok := bySlug[slug]
		if !ok || seed.Image == "" || seed.Bundle == "" || seed.Name == "" {
			t.Fatalf("seed %q = %#v, want a named seed with a bundle and an image", slug, seed)
		}
	}
}

func TestEnvVars(t *testing.T) {
	cases := map[string][2]string{
		"codex":       {"DISCOBOX_HARNESS_CODEX_BUNDLE", "DISCOBOX_HARNESS_CODEX_IMAGE"},
		"claude-code": {"DISCOBOX_HARNESS_CLAUDE_CODE_BUNDLE", "DISCOBOX_HARNESS_CLAUDE_CODE_IMAGE"},
	}
	for id, want := range cases {
		if got := BundleEnvVar(id); got != want[0] {
			t.Fatalf("BundleEnvVar(%q) = %q, want %q", id, got, want[0])
		}
		if got := ImageEnvVar(id); got != want[1] {
			t.Fatalf("ImageEnvVar(%q) = %q, want %q", id, got, want[1])
		}
	}
}

func TestSeedsApplyImageOverrides(t *testing.T) {
	refs := References{
		Bundles: map[string]string{"codex": "discobox-harness-codex:dev-def"},
		Images:  map[string]string{"codex": "discobox-harness-codex-image:dev-abc"},
	}
	bySlug := map[string]Seed{}
	for _, seed := range Seeds(refs, false) {
		bySlug[seed.Slug] = seed
	}
	if got := bySlug["codex"].Image; got != "discobox-harness-codex-image:dev-abc" {
		t.Fatalf("codex image = %q, want override applied", got)
	}
	if got := bySlug["codex"].Bundle; got != "discobox-harness-codex:dev-def" {
		t.Fatalf("codex bundle = %q, want override applied", got)
	}
	// Harnesses without an override keep their baked-in references.
	if got := bySlug["claude-code"]; got.Image != "discobox-harness-claude-code-image:local" || got.Bundle != "discobox-harness-claude-code:local" {
		t.Fatalf("claude-code = %+v, want baked-in", got)
	}
}

func TestOverridesFromEnv(t *testing.T) {
	env := map[string]string{
		"DISCOBOX_HARNESS_CODEX_IMAGE":  "  discobox-harness-codex-image:dev-xyz  ",
		"DISCOBOX_HARNESS_CODEX_BUNDLE": " discobox-harness-codex:dev-xyz ",
	}
	bundles, images := OverridesFromEnv(func(key string) string { return env[key] })
	if images["codex"] != "discobox-harness-codex-image:dev-xyz" || bundles["codex"] != "discobox-harness-codex:dev-xyz" {
		t.Fatalf("overrides[codex] = %q, %q, want trimmed values", bundles["codex"], images["codex"])
	}
	if _, ok := images["claude-code"]; ok {
		t.Fatalf("images = %v, want no claude-code entry", images)
	}
	if _, ok := bundles["claude-code"]; ok {
		t.Fatalf("bundles = %v, want no claude-code entry", bundles)
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

func TestManifestSeedsAreExactlyItsHarnesses(t *testing.T) {
	seeds := Seeds(References{
		Images:  map[string]string{"other-agent": "example.com/other-image:v1", "shell": "example.com/shell-image:v1"},
		Bundles: map[string]string{"other-agent": "example.com/other:v1", "shell": "example.com/shell:v1"},
	}, true)
	if len(seeds) != 2 || seeds[0].Slug != "other-agent" || seeds[0].Image != "example.com/other-image:v1" || seeds[0].Bundle != "example.com/other:v1" || seeds[1].Slug != "shell" {
		t.Fatalf("seeds = %+v", seeds)
	}
}
