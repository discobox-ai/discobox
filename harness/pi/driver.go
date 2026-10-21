package pi

import "github.com/discobox-ai/discobox/harness"

type Driver struct{}

func (Driver) ID() string { return "pi" }

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID: "pi", Name: "pi", Description: "pi coding harness, with any provider it signs in to.",
		Bundle: harness.ImageRef("discobox-harness-pi"), Image: harness.ImageRef("discobox-harness-pi-image"),
	}
}
