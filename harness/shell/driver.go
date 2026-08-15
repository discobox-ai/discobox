// Package shell is the harness that is a plain login shell.
//
// It declares no run command, which is the contract for "the sandbox resolves
// the user's login shell" — the control plane cannot know whether that is bash,
// zsh, or fish, since the account lives in the image (ADR 0025 §3). Everything
// else about it is an ordinary harness: its own image built on the sandbox
// agent base, its own image.json, its own registry entry.
package shell

import (
	"context"

	"github.com/obot-platform/discobox/harness"
)

type Driver struct{}

func (Driver) ID() string { return "shell" }

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID: "shell", Name: "Shell",
		Description: "An interactive login shell, with no coding harness on top.",
		Image:       "discobox-harness-shell:local",
	}
}

// InstallHooks does nothing: hooks capture a coding harness's lifecycle events,
// and a shell has none. It is not an unimplemented stub — a shell session
// genuinely has nothing to report, and the installer runs every driver against
// every sandbox.
func (Driver) InstallHooks(context.Context, harness.HookInstallRequest) error { return nil }
