// Package server exposes the Discobox control-plane server runtime.
package server

import (
	"context"

	internalserver "github.com/discobox-ai/discobox/server/internal/server"
	"github.com/discobox-ai/discobox/server/providers/libkrun"
)

// Run loads configuration, initializes storage and services, and starts the
// HTTP server.
func Run(ctx context.Context) error {
	return internalserver.Run(ctx)
}

// RunVMLauncherIfInvoked runs this process as a pool VM and never returns, or
// returns immediately when it was started for anything else.
//
// libkrun's krun_start_enter consumes the process that calls it, so a microVM
// needs a process of its own. That process is this binary re-executed rather
// than a second artifact to build and ship (ADR 0062 §9), which means the
// launcher argv has to be recognized before this binary parses its own.
//
// This binary alone. The CLI runs the server as a separate program it downloads
// (ADR 0099), so it never hosts a VM and has nothing to recognize — wiring this
// into it would re-add a dependency on the server module that that decision
// removed.
//
// Nothing else may happen first. The launcher must not open a database, bind a
// listener, or read the server's configuration, and being the first call in
// main is what guarantees it does none of them.
func RunVMLauncherIfInvoked() {
	libkrun.RunLauncherIfInvoked()
}
