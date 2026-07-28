package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/devimage"
)

// TestStampBuildModeImagesProducesBuildableManifest runs build-mode against the
// repository's real image specs, writing its outputs into a temporary directory
// so the developer's .env is untouched. It proves the watcher can describe every
// development image well enough for the server to build it without a local
// Docker daemon.
func TestStampBuildModeImagesProducesBuildableManifest(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Skipf("repository root not found: %v", err)
	}
	specs, err := dockerImageSpecs(context.Background(), repoRoot)
	if err != nil {
		t.Fatalf("dockerImageSpecs: %v", err)
	}

	out := t.TempDir()
	if err := stampBuildModeImages(out, specs); err != nil {
		t.Fatalf("stampBuildModeImages: %v", err)
	}

	manifest, err := devimage.Read(filepath.Join(out, developmentImageManifestFile))
	if err != nil {
		t.Fatalf("read stamped manifest: %v", err)
	}
	if len(manifest.Images) == 0 {
		t.Fatal("stamped manifest has no images")
	}

	references := make(map[string]struct{}, len(manifest.Images))
	for _, image := range manifest.Images {
		references[image.Reference] = struct{}{}
		if image.Build == nil {
			t.Fatalf("image %s was stamped without a build specification", image.Reference)
		}
		if image.ID != "" {
			t.Fatalf("image %s should not carry a copy-mode ID", image.Reference)
		}
		dockerfile := filepath.Join(image.Build.Context, filepath.FromSlash(image.Build.Dockerfile))
		if _, err := os.Stat(dockerfile); err != nil {
			t.Fatalf("image %s references a missing Dockerfile %s: %v", image.Reference, dockerfile, err)
		}
	}

	// Harness images build FROM the sandbox base, so their build argument must
	// name another image in the manifest; that is what orders the builds.
	sawSandboxDependency := false
	for _, image := range manifest.Images {
		base, ok := image.Build.Args["SANDBOX_AGENT_IMAGE"]
		if !ok {
			continue
		}
		if _, ok := references[base]; !ok {
			t.Fatalf("image %s depends on %q, which is not in the manifest", image.Reference, base)
		}
		sawSandboxDependency = true
	}
	if !sawSandboxDependency {
		t.Fatal("expected at least one harness image to depend on the sandbox base image")
	}

	env, err := os.ReadFile(filepath.Join(out, envFile))
	if err != nil {
		t.Fatalf("read stamped env file: %v", err)
	}
	for _, key := range []string{devimage.SyncEnv, devimage.ManifestEnv, "DISCOBOX_DOCKER_POOL_IMAGE"} {
		if !strings.Contains(string(env), key+"=") {
			t.Fatalf("stamped env file is missing %s", key)
		}
	}
}

// Stamping is content-addressed so an unchanged tree keeps the same references
// and the server rebuilds nothing.
func TestStampBuildModeImagesIsStableForUnchangedInputs(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Skipf("repository root not found: %v", err)
	}
	specs, err := dockerImageSpecs(context.Background(), repoRoot)
	if err != nil {
		t.Fatalf("dockerImageSpecs: %v", err)
	}

	first, err := contentImageTag(specs[0])
	if err != nil {
		t.Fatalf("contentImageTag: %v", err)
	}
	second, err := contentImageTag(specs[0])
	if err != nil {
		t.Fatalf("contentImageTag: %v", err)
	}
	if first != second {
		t.Fatalf("content tag is unstable: %q then %q", first, second)
	}
	if !strings.HasPrefix(first, specs[0].devPrefix) {
		t.Fatalf("content tag %q does not use the spec's dev prefix %q", first, specs[0].devPrefix)
	}
}
