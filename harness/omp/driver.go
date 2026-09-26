package omp

import "github.com/discobox-ai/discobox/harness"

type Driver struct{}

func (Driver) ID() string { return "omp" }

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID: "omp", Name: "Oh My Pi", Description: "Oh My Pi (omp) coding harness, with any provider it includes.",
		Image: harness.ImageRef("discobox-harness-omp"), Configure: &harness.Configure{},
	}
}
