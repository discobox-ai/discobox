package server

import (
	"fmt"
	"runtime"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/config"
	"github.com/discobox-ai/discobox/server/internal/harnessdefs"
	"github.com/discobox-ai/discobox/server/providers"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

// Images is every image a first run on this machine will want, for the CLI that
// stages them before starting this server (ADR 0113 §1): what the default
// provider on this OS boots, then the pool agent, the default sandbox image, and
// the built-in harnesses — in the order a first run needs them.
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
		refs := cfg.Release.Images.References(runtime.GOOS, runtime.GOARCH)
		// A server told to install Docker boots no libkrun image, and staging
		// one would download hundreds of megabytes for nothing.
		if cfg.DefaultProvider == "docker" {
			refs = slices.DeleteFunc(refs, func(ref string) bool { return ref == cfg.Release.Images.Libkrun })
		}
		return refs, nil
	}
	images := append([]string{}, providers.DefaultBootImages(cfg.DefaultProvider)...)
	images = append(images, dockerworker.EffectivePoolImage("", dockerworker.ServerDefaults{PoolImage: cfg.DockerPoolImage}), cfg.DefaultSandboxImage)
	for _, definition := range harnessdefs.Seeds(cfg.HarnessImages, false) {
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
