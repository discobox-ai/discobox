package copilot

import "github.com/discobox-ai/discobox/harness"

type Driver struct{}

func (Driver) ID() string { return "copilot" }

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID: "copilot", Name: "GitHub Copilot", Description: "GitHub Copilot CLI coding harness.",
		Bundle: harness.BundleRef("discobox-harness-copilot"),
	}
}
