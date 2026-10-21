// Package featuretest reads a harness's feature (ADR 26-10-16-423 §4), the
// provision.yaml in its directory that installs it, so the harness's tests can
// say where a file of its lands and what installs its CLI.
package featuretest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/provisionspec"
)

// Feature is a harness's loaded feature.
type Feature struct {
	provisionspec.Feature
}

// Load loads the feature in the test's working directory, which is the
// harness's own.
func Load(t testing.TB) Feature {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	feature, err := provisionspec.LoadFeature(dir)
	if err != nil {
		t.Fatal(err)
	}
	return Feature{feature}
}

// Installs is the path a file step places source at, or "" when no step
// places it.
func (f Feature) Installs(source string) string {
	for _, step := range f.Spec.Steps {
		if step.File != nil && step.File.Source == source {
			return step.File.Path
		}
	}
	return ""
}

// Runs reports whether a script one of its run steps names holds text.
func (f Feature) Runs(t testing.TB, text string) bool {
	t.Helper()
	for _, step := range f.Spec.Steps {
		if step.Run == "" {
			continue
		}
		script, err := os.ReadFile(filepath.Join(f.Dir, step.Run))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(script), text) {
			return true
		}
	}
	return false
}

// InstallsPackage reports whether a packages step installs the package on
// some distribution.
func (f Feature) InstallsPackage(name string) bool {
	for _, step := range f.Spec.Steps {
		if step.Packages != nil && slices.Contains(slices.Concat(step.Packages.Debian, step.Packages.Ubuntu), name) {
			return true
		}
	}
	return false
}
