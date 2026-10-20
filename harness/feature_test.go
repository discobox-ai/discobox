package harness_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/provisionspec"
)

// Each harness image installs its harness as a feature (ADR 26-10-16-423 §4):
// its directory is the feature, and its Dockerfile copies the files the
// feature names into the harness tree. The feature must load, require only
// what the platform provides, and have everything it names copied, or the
// image build is the first thing to say so.
func TestEveryHarnessFeatureLoadsAndIsCopiedWhole(t *testing.T) {
	platform, err := os.ReadDir("../sandbox-agent/features")
	if err != nil {
		t.Fatal(err)
	}
	var provided []string
	for _, entry := range platform {
		if entry.IsDir() {
			provided = append(provided, entry.Name())
		}
	}
	specs, err := filepath.Glob(filepath.Join("*", provisionspec.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) == 0 {
		t.Fatal("no harness declares a feature")
	}
	for _, spec := range specs {
		dir := filepath.Dir(spec)
		t.Run(dir, func(t *testing.T) {
			feature, err := provisionspec.LoadFeature(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range feature.Spec.Requires {
				if !slices.Contains(provided, name) {
					t.Errorf("requires %q, which the platform does not provide", name)
				}
			}
			dockerfile, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
			if err != nil {
				t.Fatal(err)
			}
			tree := filepath.ToSlash(filepath.Join("/opt/discobox/bundles/harness", provisionspec.FeaturesDir, feature.Name))
			copied := copiedInto(string(dockerfile), tree)
			files := []string{provisionspec.FileName}
			for _, step := range feature.Spec.Steps {
				files = append(files, step.Files()...)
			}
			for _, file := range files {
				if !slices.ContainsFunc(copied, func(c string) bool { return file == c || strings.HasPrefix(file, c+"/") }) {
					t.Errorf("the Dockerfile copies no %s into %s", file, tree)
				}
			}
		})
	}
}

// copiedInto is each path, relative to tree, that a COPY in dockerfile puts
// there from the build context.
func copiedInto(dockerfile, tree string) []string {
	var copied []string
	for line := range strings.SplitSeq(dockerfile, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "COPY" {
			continue
		}
		var args []string
		for _, field := range fields[1:] {
			if !strings.HasPrefix(field, "--") {
				args = append(args, field)
			}
		}
		dest := args[len(args)-1]
		switch {
		case strings.HasSuffix(dest, "/") && strings.TrimSuffix(dest, "/") == tree:
			copied = append(copied, args[:len(args)-1]...)
		case strings.HasPrefix(dest, tree+"/"):
			copied = append(copied, strings.TrimPrefix(dest, tree+"/"))
		}
	}
	return copied
}
