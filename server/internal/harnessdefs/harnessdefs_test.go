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
