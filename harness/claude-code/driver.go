package claudecode

import "github.com/discobox-ai/discobox/harness"

type Driver struct{}

func (Driver) ID() string { return "claude-code" }

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID: "claude-code", Name: "Claude Code", Description: "Anthropic Claude Code coding harness.",
		Bundle: harness.ImageRef("discobox-harness-claude-code"), Image: harness.ImageRef("discobox-harness-claude-code-image"),
	}
}
