package server

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/config"
	"github.com/discobox-ai/discobox/server/internal/harnessdefs"
	"github.com/discobox-ai/discobox/server/providers"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

// Images is every image a first run on this machine will want, for the CLI that
// stages them before starting this server (ADR 0113 §1): what the default
// provider on this OS boots, then the pool agent, the sandbox base image, the
// default sandbox image, and the built-in harnesses' images — in the order a
// first run needs them. Their bundles are not among them, for the reason
// releasemanifest.Images.References gives.
//
// Configuration is read as Run reads it, so an overridden image is the one
// named. Local tags are left out: they exist on no registry, and a development
// server's images are on its daemon already.
func Images() ([]string, error) {
	config.LoadEnvFile()
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if cfg.Release != nil {
		return cfg.Release.Images.References(runtime.GOOS, runtime.GOARCH), nil
	}
	images := append([]string{}, providers.DefaultBootImages()...)
	defaults := dockerworker.ServerDefaults{PoolImage: cfg.DockerPoolImage, SandboxBaseImage: cfg.DockerSandboxBaseImage}
	images = append(images, dockerworker.EffectivePoolImage("", defaults), dockerworker.EffectiveSandboxBaseImage("", defaults), cfg.DefaultSandboxImage)
	for _, definition := range harnessdefs.Seeds(cfg.Harnesses, false) {
		images = append(images, definition.Image)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(images))
	for _, image := range images {
		image = strings.TrimSpace(image)
		if image == "" || strings.HasSuffix(image, ":local") || seen[image] {
			continue
		}
		seen[image] = true
		out = append(out, image)
	}
	return out, nil
}
