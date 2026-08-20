package dockerworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/moby/client"

	"github.com/obot-platform/discobox/devimage"
)

// BuildArtifacts builds an image on a pool's own Docker daemon and writes its
// final stage's files back to a directory on this host.
//
// This is what closes the bootstrap loop on a host with no Docker daemon
// (ADR 0062 §7). A VM guest image is seeded from the registry once; after that,
// the pool VM it boots is the builder for its own successor. The build context
// streams from this host over the same BuildKit session that carries the dev
// image builds, and the local exporter streams the result back, so a developer
// can change the guest image and boot the change without ever installing Docker.
//
// The image being built must end in a stage that contains only the artifacts —
// a `FROM scratch` stage — because the local exporter writes that stage's whole
// filesystem into outputDir.
func BuildArtifacts(ctx context.Context, destination *client.Client, spec devimage.BuildSpec, outputDir string) error {
	if strings.TrimSpace(spec.Dockerfile) == "" {
		return errors.New("build artifacts: a Dockerfile is required")
	}
	if !filepath.IsAbs(spec.Context) {
		return fmt.Errorf("build artifacts: build context %q must be an absolute path", spec.Context)
	}
	if !filepath.IsAbs(outputDir) {
		return fmt.Errorf("build artifacts: output directory %q must be an absolute path", outputDir)
	}

	daemonID := "unknown"
	if info, err := destination.Info(ctx, client.InfoOptions{}); err == nil {
		if id := strings.TrimSpace(info.Info.ID); id != "" {
			daemonID = id
		}
	}
	bk, err := connectBuildKit(ctx, destination, daemonID)
	if err != nil {
		return err
	}
	defer func() { _ = bk.Close() }()

	// Staged and renamed rather than written in place: a driver may be booting
	// from outputDir right now, and a half-exported artifact set there would be
	// a guest that boots to nothing.
	if err := os.MkdirAll(filepath.Dir(outputDir), 0o755); err != nil {
		return fmt.Errorf("build artifacts: create %s: %w", filepath.Dir(outputDir), err)
	}
	staging, err := os.MkdirTemp(filepath.Dir(outputDir), "."+filepath.Base(outputDir)+"-")
	if err != nil {
		return fmt.Errorf("build artifacts: create staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	started := time.Now()
	slog.InfoContext(ctx, "building artifacts on pool Docker daemon",
		"daemon_id", daemonID, "dockerfile", spec.Dockerfile, "output", outputDir)
	err = solve(ctx, bk, bkclient.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: frontendAttrs(&spec),
		LocalDirs:     map[string]string{"context": spec.Context, "dockerfile": spec.Context},
		Exports:       []bkclient.ExportEntry{{Type: bkclient.ExporterLocal, OutputDir: staging}},
	}, spec.Dockerfile)
	if err != nil {
		return fmt.Errorf("build artifacts from %s on Docker daemon %s: %w", spec.Dockerfile, daemonID, err)
	}

	if err := os.RemoveAll(outputDir); err != nil {
		return fmt.Errorf("build artifacts: replace %s: %w", outputDir, err)
	}
	if err := os.Rename(staging, outputDir); err != nil {
		return fmt.Errorf("build artifacts: publish %s: %w", outputDir, err)
	}
	slog.InfoContext(ctx, "built artifacts on pool Docker daemon",
		"daemon_id", daemonID, "output", outputDir, "duration", time.Since(started))
	return nil
}
