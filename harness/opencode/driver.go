package opencode

import "github.com/discobox-ai/discobox/harness"

type Driver struct{}

func (Driver) ID() string { return "opencode" }

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID: "opencode", Name: "OpenCode", Description: "OpenCode coding harness, with any provider it includes.",
		Bundle: harness.ImageRef("discobox-harness-opencode"), Image: harness.ImageRef("discobox-harness-opencode-image"),
	}
}
