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

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// Feature is a harness's loaded feature.
type Feature struct {
	provisionspec.Feature
}

// Load assembles a harness feature as its Dockerfile does: its own directory
// plus the shared exporter that each coding harness owns in its bundle.
func Load(t testing.TB, source string) Feature {
	t.Helper()
	dir, err := filepath.Abs(source)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(t.TempDir(), filepath.Base(dir))
	if err := os.CopyFS(staged, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(staged, "harness-capture"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"runtime.cjs", "settings.cjs", "settings-LICENSE.txt"} {
		data, err := os.ReadFile(filepath.Join(filepath.Dir(dir), "capture", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staged, "harness-capture", name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	feature, err := provisionspec.LoadFeature(staged)
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
