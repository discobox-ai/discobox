package provision

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// The platform bundle (the Dockerfile's `platform` stage, which picks its OS's
// entry from the FROM scratch stages below it) is assembled from sandbox-agent/features and files it copies in from
// elsewhere, under /features/ (ADR 26-10-16-423 §7): one entry per OS, each
// `platform-<os>` stage and the stages it is built FROM (ADR 26-11-11-963
// §9). Replaying each OS's COPY lines into a directory proves, without
// building it, that the tree a box is delivered loads (each step names files
// that are there, and every provision.yaml is valid), that every feature it
// holds finds what it requires in it, and that every COPY is --link, so an
// edit to one feature moves one layer. Linux's holds every feature; darwin's
// every one but the desktop and nested Docker, which a macOS sandbox lacks
// (ADR 0145 §7).
func TestThePlatformBundleCarriesATreeThatLoads(t *testing.T) {
	const repo = "../.."
	data, err := os.ReadFile(filepath.Join(repo, "sandbox-agent", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	stages := dockerfileStages(string(data))
	if parent := stages["platform"].parent; parent != "platform-${TARGETOS}" {
		t.Fatalf("the platform stage is FROM %q, want each OS's entry by platform-${TARGETOS}", parent)
	}

	// Core, and every feature ADR 26-10-16-423 §4 names the platform's.
	platformFeatures := []string{
		provisionspec.Core, "node", "docker", "browser", "desktop", "nix", "brew", "go", "python", "rust",
		"bun", "mise", "build", "cli", "shells-editors", "direnv", "review", "discobox-cli",
	}
	absent := map[string][]string{"linux": nil, "darwin": {"desktop", "docker"}}
	entries, err := os.ReadDir(filepath.Join(repo, "sandbox-agent", "features"))
	if err != nil {
		t.Fatal(err)
	}
	for goos, lacks := range absent {
		names := replayPlatformTree(t, repo, stages, "platform-"+goos)
		for _, entry := range entries {
			if entry.IsDir() && !slices.Contains(lacks, entry.Name()) && !slices.Contains(names, entry.Name()) {
				t.Errorf("sandbox-agent/features/%s is not in the platform bundle's %s entry", entry.Name(), goos)
			}
		}
		for _, want := range platformFeatures {
			if has := slices.Contains(names, want); has == slices.Contains(lacks, want) {
				t.Errorf("the platform bundle's %s entry has the %s feature: %v, want %v", goos, want, has, !has)
			}
		}
	}
}

// stage is one of a Dockerfile's stages: what it is FROM, and its COPY lines.
type stage struct {
	parent string
	copies []string
}

// dockerfileStages reads a Dockerfile's named stages.
func dockerfileStages(dockerfile string) map[string]stage {
	stages := map[string]stage{}
	name := ""
	for line := range strings.SplitSeq(dockerfile, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "FROM" {
			name = ""
			if n := len(fields); n >= 4 && strings.EqualFold(fields[n-2], "AS") {
				name = fields[n-1]
				stages[name] = stage{parent: fields[n-3]}
			}
			continue
		}
		if fields[0] == "COPY" && name != "" {
			s := stages[name]
			s.copies = append(s.copies, line)
			stages[name] = s
		}
	}
	return stages
}

// replayPlatformTree replays the COPY lines of name and of every stage it is
// built FROM, oldest first, into a directory, and loads and orders the
// features there, answering their names.
func replayPlatformTree(t *testing.T, repo string, stages map[string]stage, name string) []string {
	t.Helper()
	var chain []string
	for at, ok := name, true; ok; at = stages[at].parent {
		chain = append([]string{at}, chain...)
		_, ok = stages[stages[at].parent]
	}
	root := t.TempDir()
	featuresDir := "/" + provisionspec.FeaturesDir + "/"
	copies := 0
	for _, at := range chain {
		for _, line := range stages[at].copies {
			copies++
			fields := strings.Fields(line)
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
				// A binary another stage built: what matters here is only
				// that it is there.
				writeTestFile(t, target, "binary")
				continue
			}
			for _, src := range args[:len(args)-1] {
				copyTestTree(t, filepath.Join(repo, src), target)
			}
		}
	}
	if copies == 0 {
		t.Fatalf("the Dockerfile's %s stage copies nothing", name)
	}
	loaded, err := provisionspec.Load(root)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if _, err := order(loaded); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var names []string
	for _, feature := range loaded {
		names = append(names, feature.Name)
	}
	return names
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
