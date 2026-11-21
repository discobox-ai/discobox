package sandboxcreate

import (
	"strings"

	"github.com/discobox-ai/discobox/harness"
)

const RunHarnessPrompt = "Choose a harness"

const NoRunHarness = "this project has no harness to run; register one with discobox admin harness create"

// HarnessChoice describes what choosing a harness does before creating a box.
type HarnessChoice struct {
	Label     string
	Detail    string
	DefaultID string
	Override  string
}

func RunHarnessChoice(id, name, slug, parent string) HarnessChoice {
	if strings.TrimSpace(slug) == harness.ShellSlug {
		return HarnessChoice{Label: "Shell (no agent)", Detail: "this box only", Override: harness.ShellSlug}
	}
	return HarnessChoice{
		Label:     HarnessDisplayName(id, name, slug, parent),
		Detail:    "set project default",
		DefaultID: id,
	}
}

func HarnessDisplayName(id, name, slug, parent string) string {
	if name = strings.TrimSpace(name); name != "" {
		if parent != "" {
			return name + " (variant of " + parent + ")"
		}
		return name
	}
	if slug = strings.TrimSpace(slug); slug != "" {
		return slug
	}
	return id
}
