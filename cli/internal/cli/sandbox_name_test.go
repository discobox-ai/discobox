package cli

import (
	"testing"

	apimodel "github.com/discobox-ai/discobox/api/model"
)

// namedSandbox builds a sandbox configured as "generated-name" that the server
// says to display under displayName.
func namedSandbox(displayName string) apimodel.Sandbox {
	sb := apimodel.Sandbox{DisplayName: displayName}
	sb.Config.Name = "generated-name"
	return sb
}

// The launcher's row carries the name the server computed, and the flag that
// tells rename the configured name is not the one on screen.
func TestToTUISandboxUsesTheServerDisplayName(t *testing.T) {
	titled := toTUISandbox(namedSandbox("fix the reaper"))
	if titled.Name != "fix the reaper" || !titled.NameIsTitle {
		t.Fatalf("row = %q (NameIsTitle=%t), want the title, flagged", titled.Name, titled.NameIsTitle)
	}
	untitled := toTUISandbox(namedSandbox("generated-name"))
	if untitled.Name != "generated-name" || untitled.NameIsTitle {
		t.Fatalf("row = %q (NameIsTitle=%t), want the configured name, unflagged", untitled.Name, untitled.NameIsTitle)
	}
}

// A sandbox with no configured name displays as its ID. Rename does change
// what that row shows, so it is not flagged as titled.
func TestSandboxNameIsTitleIgnoresTheIDFallback(t *testing.T) {
	sb := apimodel.Sandbox{ID: "sbx_abc12345000000p3", DisplayName: "sbx_abc12345000000p3"}
	if sandboxNameIsTitle(sb) {
		t.Fatal("the ID fallback was read as a terminal title")
	}
}
