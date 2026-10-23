package provision

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// The platform bundle (the Dockerfile's FROM scratch `platform` stage) is
// assembled from sandbox-agent/features and files it copies in from
// elsewhere, under /features/ (ADR 26-10-16-423 §7). Replaying its COPY lines
// into a directory proves, without building it, that the tree a box is
// delivered loads (each step names files that are there, and every
// provision.yaml is valid), that every feature it holds finds what it
// requires in it, and that every COPY is --link, so an edit to one feature
// moves one layer.
func TestThePlatformBundleCarriesATreeThatLoads(t *testing.T) {
	const repo = "../.."
	root := t.TempDir()
	data, err := os.ReadFile(filepath.Join(repo, "sandbox-agent", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	featuresDir := "/" + provisionspec.FeaturesDir + "/"
	stage, copies := "", 0
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "FROM" {
			stage = ""
			if n := len(fields); n >= 4 && strings.EqualFold(fields[n-2], "AS") {
				stage = fields[n-1]
			}
			continue
		}
		if fields[0] != "COPY" || stage != "platform" {
			continue
		}
		copies++
		if fields[1] != "--link" {
			t.Errorf("the platform bundle copies without --link: %s", line)
		}
		var args []string
		fromStage := false
		for _, field := range fields[1:] {
			if strings.HasPrefix(field, "--from=") {
				fromStage = true
			}
			if !strings.HasPrefix(field, "--") {
				args = append(args, field)
			}
		}
		dest := args[len(args)-1]
		if !strings.HasPrefix(dest, featuresDir) {
			t.Errorf("the platform bundle copies outside %s: %s", featuresDir, line)
			continue
		}
		target := filepath.Join(root, dest)
		if fromStage {
			// A binary another stage built: what matters here is only that
			// it is there.
			writeTestFile(t, target, "binary")
			continue
		}
		for _, src := range args[:len(args)-1] {
			copyTestTree(t, filepath.Join(repo, src), target)
		}
	}
	if copies == 0 {
		t.Fatal("the Dockerfile has no platform stage that copies anything")
	}
	loaded, err := provisionspec.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := order(loaded); err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, feature := range loaded {
		names = append(names, feature.Name)
	}
	entries, err := os.ReadDir(filepath.Join(repo, "sandbox-agent", "features"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && !slices.Contains(names, entry.Name()) {
			t.Errorf("sandbox-agent/features/%s is not in the platform bundle", entry.Name())
		}
	}
	// Core, and every feature ADR 26-10-16-423 §4 names the platform's.
	for _, want := range []string{
		provisionspec.Core, "node", "docker", "browser", "desktop", "nix", "brew", "go", "python", "rust",
		"bun", "mise", "build", "cli", "shells-editors", "direnv", "review", "discobox-cli",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("the platform bundle has no %s feature", want)
		}
	}
}

// copyTestTree copies src, a file or a directory, to dest, as a COPY does.
func copyTestTree(t *testing.T, src, dest string) {
	t.Helper()
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		if err := os.CopyFS(dest, os.DirFS(src)); err != nil {
			t.Fatal(err)
		}
		return
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dest, string(data))
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
