package pi

import "github.com/discobox-ai/discobox/harness"

type Driver struct{}

func (Driver) ID() string { return "pi" }

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID: "pi", Name: "Pi", Description: "Pi coding harness, with any provider it includes.",
		Image: harness.ImageRef("discobox-harness-pi"), Configure: &harness.Configure{},
	}
}
