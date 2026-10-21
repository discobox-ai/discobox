package omp

import "github.com/discobox-ai/discobox/harness"

type Driver struct{}

func (Driver) ID() string { return "omp" }

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID: "omp", Name: "omp", Description: "omp (oh-my-pi) coding harness, with any provider it signs in to.",
		Bundle: harness.ImageRef("discobox-harness-omp"), Image: harness.ImageRef("discobox-harness-omp-image"),
	}
}
