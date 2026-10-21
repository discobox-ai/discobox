package codexcli

import "github.com/discobox-ai/discobox/harness"

type Driver struct{}

func (Driver) ID() string { return "codex-cli" }

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID: "codex", Name: "Codex", Description: "OpenAI Codex coding harness.",
		Bundle: harness.ImageRef("discobox-harness-codex"), Image: harness.ImageRef("discobox-harness-codex-image"),
	}
}
