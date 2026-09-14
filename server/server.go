// Package server exposes the Discobox control-plane server runtime.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/discobox-ai/discobox/harness/registry"
	"github.com/discobox-ai/discobox/releasemanifest"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/guestimage"
	"github.com/discobox-ai/discobox/version"

	internalserver "github.com/discobox-ai/discobox/server/internal/server"
	"github.com/discobox-ai/discobox/server/providers/libkrun"
)

// Run loads configuration, initializes storage and services, and starts the
// HTTP server.
func Run(ctx context.Context) error {
	return internalserver.Run(ctx)
}

// PrintImages writes, one per line, the images a first run on this machine will
// want, for the CLI that staged this binary to stage them before starting it
// (ADR 0113 §1). It reads the configuration and starts nothing.
func PrintImages(w io.Writer) error {
	images, err := internalserver.Images()
	if err != nil {
		return err
	}
	for _, image := range images {
		if _, err := fmt.Fprintln(w, image); err != nil {
			return err
		}
	}
	return nil
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

// PrintReleaseManifest exports the image set this binary was built against.
// It does not read machine configuration, so a release build exports its own
// artifacts even on a machine configured for a different release.
func PrintReleaseManifest(w io.Writer) error {
	m := releasemanifest.Manifest{Format: 1, Version: version.String(), Images: releasemanifest.Images{
		PoolAgent: dockerworker.DefaultPoolImage, SandboxAgent: sandbox.DefaultSandboxImageName,
		VM: guestimage.DefaultVMImage, Kernel: libkrun.DefaultKernelImage, Harnesses: map[string]string{},
	}}
	for _, definition := range registry.Definitions() {
		m.Images.Harnesses[definition.ID] = definition.Image
	}
	if err := m.Validate(); err != nil {
		return err
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(m)
}
