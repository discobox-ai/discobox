package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
)

// The reaper finds Discobox images by label, so a Dockerfile that stops
// declaring it silently makes that image immortal — and a renamed Go constant
// silently makes every image immortal. Neither shows up as a failure anywhere
// else, since not reclaiming an image looks exactly like an image still in use.
//
// Only the two roots are checked: harness images are built FROM the sandbox
// agent and inherit the label through the image config, which is the same
// mechanism that carries it across a pull.
func TestImageRootsDeclareTheReclaimLabel(t *testing.T) {
	_, repoRoot := loadDockerImageSpecs(t)
	want := "LABEL " + harness.ReclaimLabel + "=" + harness.ReclaimLabelValue

	for _, dockerfile := range []string{"pool-agent/Dockerfile", "sandbox-agent/Dockerfile"} {
		data, err := os.ReadFile(filepath.Join(repoRoot, dockerfile))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), want) {
			t.Fatalf("%s does not declare %q", dockerfile, want)
		}
	}
}

// Harness images must not carry their own copy: a duplicate would keep working
// while the inherited one was removed, hiding the break in the base image that
// every other Discobox image depends on.
func TestHarnessImagesInheritTheReclaimLabel(t *testing.T) {
	_, repoRoot := loadDockerImageSpecs(t)

	for _, harnessImage := range harnessImages {
		dockerfile := filepath.Join("harness", harnessImage.dir, "Dockerfile")
		data, err := os.ReadFile(filepath.Join(repoRoot, dockerfile))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), harness.ReclaimLabel) {
			t.Fatalf("%s declares %s instead of inheriting it", dockerfile, harness.ReclaimLabel)
		}
	}
}
