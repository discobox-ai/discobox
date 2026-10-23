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
		if !ok || seed.Bundle == "" || seed.Name == "" {
			t.Fatalf("seed %q = %#v, want a named seed with a bundle", slug, seed)
		}
	}
}

func TestEnvVars(t *testing.T) {
	cases := map[string]string{
		"codex":       "DISCOBOX_HARNESS_CODEX_BUNDLE",
		"claude-code": "DISCOBOX_HARNESS_CLAUDE_CODE_BUNDLE",
	}
	for id, want := range cases {
		if got := BundleEnvVar(id); got != want {
			t.Fatalf("BundleEnvVar(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestSeedsApplyBundleOverrides(t *testing.T) {
	refs := References{Bundles: map[string]string{"codex": "discobox-harness-codex:dev-def"}}
	bySlug := map[string]Seed{}
	for _, seed := range Seeds(refs, false) {
		bySlug[seed.Slug] = seed
	}
	if got := bySlug["codex"].Bundle; got != "discobox-harness-codex:dev-def" {
		t.Fatalf("codex bundle = %q, want override applied", got)
	}
	// Harnesses without an override keep their baked-in references.
	if got := bySlug["claude-code"]; got.Bundle != "discobox-harness-claude-code:local" {
		t.Fatalf("claude-code = %+v, want baked-in", got)
	}
}

func TestOverridesFromEnv(t *testing.T) {
	env := map[string]string{
		"DISCOBOX_HARNESS_CODEX_BUNDLE": " discobox-harness-codex:dev-xyz ",
	}
	bundles := OverridesFromEnv(func(key string) string { return env[key] })
	if bundles["codex"] != "discobox-harness-codex:dev-xyz" {
		t.Fatalf("overrides[codex] = %q, want the trimmed value", bundles["codex"])
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
		Bundles: map[string]string{"other-agent": "example.com/other:v1", "shell": "example.com/shell:v1"},
	}, true)
	if len(seeds) != 2 || seeds[0].Slug != "other-agent" || seeds[0].Name != "other-agent" || seeds[0].Bundle != "example.com/other:v1" || seeds[1].Slug != "shell" {
		t.Fatalf("seeds = %+v", seeds)
	}
}
