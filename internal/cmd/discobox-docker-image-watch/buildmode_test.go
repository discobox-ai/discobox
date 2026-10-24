package main

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/devimage"
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
	specs, err := dockerImageSpecs(context.Background(), repoRoot, "discobox")
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

	// A bundle is never run, so it is recorded among the manifest's bundles,
	// with the build a pool's daemon runs and no layout or digest yet, and
	// never among the images a pool daemon is given.
	bundles := make(map[string]devimage.Bundle, len(manifest.Bundles))
	for _, bundle := range manifest.Bundles {
		if bundle.Build == nil {
			t.Fatalf("bundle %s was stamped without a build specification", bundle.Reference)
		}
		if bundle.Digest != "" || bundle.Layout != "" {
			t.Fatalf("bundle %s should not carry a copy-mode digest or layout", bundle.Reference)
		}
		if _, ok := references[bundle.Reference]; ok {
			t.Fatalf("bundle %s is stamped as an image too", bundle.Reference)
		}
		dockerfile := filepath.Join(bundle.Build.Context, filepath.FromSlash(bundle.Build.Dockerfile))
		if _, err := os.Stat(dockerfile); err != nil {
			t.Fatalf("bundle %s references a missing Dockerfile %s: %v", bundle.Reference, dockerfile, err)
		}
		bundles[bundle.Reference] = bundle
	}
	for _, spec := range specs {
		inBundles := false
		for reference := range bundles {
			inBundles = inBundles || strings.HasPrefix(reference, spec.devPrefix)
		}
		inImages := false
		for reference := range references {
			inImages = inImages || strings.HasPrefix(reference, spec.devPrefix)
		}
		if spec.bundle && (!inBundles || inImages) {
			t.Errorf("bundle spec %s: in bundles = %v, in images = %v; want only bundles", spec.name, inBundles, inImages)
		}
		if !spec.bundle && (inBundles || !inImages) {
			t.Errorf("image spec %s: in bundles = %v, in images = %v; want only images", spec.name, inBundles, inImages)
		}
	}

	// The server builds each bundle's stage and reconstructs its labels from
	// its arguments, so both must say which artifact it is.
	wantTargets := map[string]string{
		"discobox-platform:dev-":            "platform",
		"discobox-harness-claude-code:dev-": "bundle",
		"discobox-harness-shell:dev-":       "bundle",
	}
	wantArgs := map[string][]string{
		"discobox-platform:dev-":            {harness.LayerMetadataBuildArg},
		"discobox-harness-claude-code:dev-": {harness.MetadataBuildArg},
		"discobox-harness-shell:dev-":       {harness.MetadataBuildArg},
	}
	for prefix, target := range wantTargets {
		found := false
		for _, bundle := range bundles {
			if !strings.HasPrefix(bundle.Reference, prefix) {
				continue
			}
			found = true
			if bundle.Build.Target != target {
				t.Errorf("%s build target = %q, want %q", bundle.Reference, bundle.Build.Target, target)
			}
			got := slices.Sorted(maps.Keys(bundle.Build.Args))
			want := slices.Sorted(slices.Values(wantArgs[prefix]))
			if !slices.Equal(got, want) {
				t.Errorf("%s build args = %v, want %v", bundle.Reference, got, want)
			}
			for _, key := range []string{harness.MetadataBuildArg, harness.LayerMetadataBuildArg} {
				if value, ok := bundle.Build.Args[key]; ok && !strings.Contains(value, harness.PlatformAPIVersion) {
					t.Errorf("%s %s = %q, want the compacted image.json naming %s", bundle.Reference, key, value, harness.PlatformAPIVersion)
				}
			}
		}
		if !found {
			t.Errorf("stamped manifest has no %s bundle", prefix)
		}
	}

	env, err := os.ReadFile(filepath.Join(out, envFile))
	if err != nil {
		t.Fatalf("read stamped env file: %v", err)
	}
	for _, key := range []string{devimage.SyncEnv, devimage.ManifestEnv,
		"DISCOBOX_PLATFORM_BUNDLE", "DISCOBOX_HARNESS_CLAUDE_CODE_BUNDLE"} {
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
	specs, err := dockerImageSpecs(context.Background(), repoRoot, "discobox")
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
