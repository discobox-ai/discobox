package codexcli

import (
	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/platform"
)

type Driver struct{}

func (Driver) ID() string { return "codex-cli" }

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID: "codex", Name: "Codex", Description: "OpenAI Codex coding harness.",
		Image: harness.ImageRef("discobox-harness-codex"), Platform: platform.Pool(),
		Configure: &harness.Configure{},
	}
}
