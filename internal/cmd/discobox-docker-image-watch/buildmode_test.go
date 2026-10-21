package main

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/devimage"
	"github.com/discobox-ai/discobox/harness"
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

	// The server builds each entry's stage and reconstructs its labels from
	// its arguments, so both must say which artifact it is.
	wantTargets := map[string]string{
		"discobox-sandbox-agent:dev-":             "sandbox-agent",
		"discobox-platform:dev-":                  "platform",
		"discobox-harness-claude-code:dev-":       "bundle",
		"discobox-harness-claude-code-image:dev-": "image",
		"discobox-harness-shell:dev-":             "bundle",
		"discobox-harness-shell-image:dev-":       "image",
	}
	wantArgs := map[string][]string{
		"discobox-sandbox-agent:dev-":             {"BASE_IMAGE", harness.LayerMetadataBuildArg},
		"discobox-platform:dev-":                  {harness.LayerMetadataBuildArg},
		"discobox-harness-claude-code:dev-":       {harness.MetadataBuildArg},
		"discobox-harness-claude-code-image:dev-": {harness.MetadataBuildArg, "SANDBOX_AGENT_IMAGE"},
		"discobox-harness-shell:dev-":             {harness.MetadataBuildArg},
		"discobox-harness-shell-image:dev-":       {harness.MetadataBuildArg, "SANDBOX_AGENT_IMAGE"},
	}
	for prefix, target := range wantTargets {
		found := false
		for _, image := range manifest.Images {
			if !strings.HasPrefix(image.Reference, prefix) {
				continue
			}
			found = true
			if image.Build.Target != target {
				t.Errorf("%s build target = %q, want %q", image.Reference, image.Build.Target, target)
			}
			got := slices.Sorted(maps.Keys(image.Build.Args))
			want := slices.Sorted(slices.Values(wantArgs[prefix]))
			if !slices.Equal(got, want) {
				t.Errorf("%s build args = %v, want %v", image.Reference, got, want)
			}
			for _, key := range []string{harness.MetadataBuildArg, harness.LayerMetadataBuildArg} {
				if value, ok := image.Build.Args[key]; ok && !strings.Contains(value, harness.PlatformAPIVersion) {
					t.Errorf("%s %s = %q, want the compacted image.json naming %s", image.Reference, key, value, harness.PlatformAPIVersion)
				}
			}
		}
		if !found {
			t.Errorf("stamped manifest has no %s image", prefix)
		}
	}

	env, err := os.ReadFile(filepath.Join(out, envFile))
	if err != nil {
		t.Fatalf("read stamped env file: %v", err)
	}
	for _, key := range []string{devimage.SyncEnv, devimage.ManifestEnv, "DISCOBOX_DOCKER_POOL_IMAGE",
		"DISCOBOX_PLATFORM_BUNDLE", "DISCOBOX_HARNESS_CLAUDE_CODE_BUNDLE", "DISCOBOX_HARNESS_CLAUDE_CODE_IMAGE"} {
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
