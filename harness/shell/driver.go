// Package shell is the harness that is a plain login shell.
//
// It declares no run command, which is the contract for "the sandbox resolves
// the user's login shell" — the control plane cannot know whether that is bash,
// zsh, or fish, since the account lives in the image (ADR 0025 §3). Everything
// else about it is an ordinary harness: its own image built on the sandbox
// agent base, its own image.json, its own registry entry.
package shell

import "github.com/discobox-ai/discobox/harness"

type Driver struct{}

func (Driver) ID() string { return harness.ShellSlug }

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID: harness.ShellSlug, Name: "Shell",
		Description: "An interactive login shell, with no coding harness on top.",
		Image:       harness.ImageRef("discobox-harness-shell"),
	}
}
