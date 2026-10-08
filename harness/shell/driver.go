// Package shell is the harness that is a plain login shell.
//
// Its slug is the reserved harness.ShellSlug, and that is what withholds the
// conventional run command when its image is registered: a harness with none is
// the contract for "the sandbox resolves the user's login shell" (ADR 0043 §2)
// — the control plane cannot know whether that is bash, zsh, or fish, since the
// account lives in the image (ADR 0025). Everything else about it is an
// ordinary harness: its own image built on the sandbox agent base, its own
// registry entry. It has no image.json: it has nothing to override.
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
