package claudecode

import (
	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/platform"
)

type Driver struct{}

func (Driver) ID() string { return "claude-code" }

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID: "claude-code", Name: "Claude Code", Description: "Anthropic Claude Code coding harness.",
		Image: harness.ImageRef("discobox-harness-claude-code"), Platform: platform.Pool(),
		Configure: &harness.Configure{},
	}
}
