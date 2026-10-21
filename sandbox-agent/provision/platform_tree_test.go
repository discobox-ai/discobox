package provision

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/provisionspec"
)

// The image build assembles the platform tree from sandbox-agent/features and
// files it copies in from elsewhere, and provisions it a layer at a time.
// Replaying its COPY lines into a directory proves, without building it, that
// at every provisioning step the tree loads (each step names files that are
// there, and every provision.yaml is valid) and every feature it holds finds
// what it requires already in it.
func TestTheImageBuildProvisionsAPlatformTreeThatLoads(t *testing.T) {
	const repo = "../.."
	root := t.TempDir()
	tree := filepath.Join(root, PlatformTree)
	file, err := os.Open(filepath.Join(repo, "sandbox-agent", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	runs := 0
	var loaded []provisionspec.Feature
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		if strings.Contains(scanner.Text(), "discobox-image-provision "+PlatformTree) {
			runs++
			if loaded, err = provisionspec.Load(tree); err != nil {
				t.Fatalf("provisioning step %d: %v", runs, err)
			}
			if _, err := order(loaded); err != nil {
				t.Fatalf("provisioning step %d: %v", runs, err)
			}
			continue
		}
		if fields[0] != "COPY" {
			continue
		}
		var args []string
		stage := false
		for _, field := range fields[1:] {
			if strings.HasPrefix(field, "--from=") {
				stage = true
			}
			if !strings.HasPrefix(field, "--") {
				args = append(args, field)
			}
		}
		dest := args[len(args)-1]
		if !strings.HasPrefix(dest, PlatformTree+"/") {
			continue
		}
		target := filepath.Join(root, dest)
		if stage {
			// A binary another stage built: what matters here is only that
			// it is there.
			writeTestFile(t, target, "binary")
			continue
		}
		for _, src := range args[:len(args)-1] {
			copyTestTree(t, filepath.Join(repo, src), target)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if runs == 0 {
		t.Fatal("the Dockerfile never provisions the platform tree")
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
			t.Errorf("sandbox-agent/features/%s is not in the tree the image provisions", entry.Name())
		}
	}
	// Core, and every feature ADR 26-10-16-423 §4 names the platform's.
	for _, want := range []string{
		provisionspec.Core, "node", "docker", "browser", "desktop", "nix", "brew", "go", "python", "rust",
		"bun", "mise", "build", "cli", "shells-editors", "direnv", "review", "discobox-cli",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("the platform tree has no %s feature", want)
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

// The platform bundle (the Dockerfile's FROM scratch `platform` stage) holds
// the same trees the runnable stage copies into the platform tree, from the
// same sources, under /features/ (ADR 26-10-16-423 §7). It copies them itself
// rather than from the runnable stage, so building it never builds the 4GB
// image — which leaves two lists of COPY lines, and two copies of the layer
// label, that must not drift apart.
func TestThePlatformBundleCarriesTheTreeTheImageProvisions(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("../..", "sandbox-agent", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	copies := map[string][]string{}
	labels := map[string]string{}
	stage := ""
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "FROM":
			stage = ""
			if n := len(fields); n >= 4 && strings.EqualFold(fields[n-2], "AS") {
				stage = fields[n-1]
			}
		case "COPY":
			copies[stage] = append(copies[stage], strings.Join(fields, " "))
		case "LABEL":
			if strings.HasPrefix(fields[1], "io.discobox.image.v1.10-platform=") {
				labels[stage] = strings.Join(fields, " ")
			}
		}
	}
	// And the same layer label: the bundle's is what the server composes on,
	// and the image's is what moves the digest a sandbox pins when it changes.
	if labels["platform"] == "" || labels["platform"] != labels["sandbox-agent"] {
		t.Errorf("the platform layer label differs between the bundle and the image:\nimage:  %q\nbundle: %q", labels["sandbox-agent"], labels["platform"])
	}
	featuresDir := "/" + provisionspec.FeaturesDir + "/"
	var image []string
	for _, line := range copies["sandbox-agent"] {
		if dest := PlatformTree + featuresDir; strings.Contains(line, " "+dest) {
			image = append(image, strings.Replace(line, " "+dest, " "+featuresDir, 1))
		}
	}
	bundle := copies["platform"]
	if len(bundle) == 0 {
		t.Fatal("the Dockerfile has no platform stage that copies anything")
	}
	for _, line := range bundle {
		if !strings.Contains(line, " "+featuresDir) {
			t.Errorf("the platform bundle copies outside %s: %s", featuresDir, line)
		}
	}
	slices.Sort(image)
	slices.Sort(bundle)
	if !slices.Equal(image, bundle) {
		t.Fatalf("the platform bundle's COPY lines differ from the image's:\nimage:  %q\nbundle: %q", image, bundle)
	}
}
