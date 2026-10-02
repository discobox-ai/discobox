package opencode

import (
	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/platform"
)

type Driver struct{}

func (Driver) ID() string { return "opencode" }

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID: "opencode", Name: "OpenCode", Description: "OpenCode coding harness, with any provider it includes.",
		Image: harness.ImageRef("discobox-harness-opencode"), Platform: platform.Pool(),
		Configure: &harness.Configure{},
	}
}
