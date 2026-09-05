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
// Only the one root is checked: every other Discobox image is built FROM the
// shared base, directly or through the sandbox agent, and inherits the label
// through the image config — the same mechanism that carries it across a pull.
func TestImageRootDeclaresTheReclaimLabel(t *testing.T) {
	_, repoRoot := loadDockerImageSpecs(t)
	want := "LABEL " + harness.ReclaimLabel + "=" + harness.ReclaimLabelValue

	data, err := os.ReadFile(filepath.Join(repoRoot, "base-image", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("base-image/Dockerfile does not declare %q", want)
	}
}

// No image below the root may carry its own copy: a duplicate would keep working
// while the inherited one was removed, hiding the break in the base image that
// every other Discobox image depends on.
func TestImagesBelowTheRootInheritTheReclaimLabel(t *testing.T) {
	_, repoRoot := loadDockerImageSpecs(t)

	dockerfiles := []string{
		filepath.Join("pool-agent", "Dockerfile"),
		filepath.Join("sandbox-agent", "Dockerfile"),
	}
	for _, harnessImage := range harnessImages {
		dockerfiles = append(dockerfiles, filepath.Join("harness", harnessImage.dir, "Dockerfile"))
	}
	for _, dockerfile := range dockerfiles {
		data, err := os.ReadFile(filepath.Join(repoRoot, dockerfile))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), harness.ReclaimLabel) {
			t.Fatalf("%s declares %s instead of inheriting it", dockerfile, harness.ReclaimLabel)
		}
	}
}
