package harness_test

import (
	"os"
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/harness/internal/harnesstest"
	"github.com/discobox-ai/discobox/internal/harnessbundle"
)

// Each coding harness's bundle carries its harness as a feature (ADR
// 26-10-16-423 §§2, 4), generated from its declaration (ADR 26-11-06-596).
// The feature must load as the sandbox agent loads it, with every file its
// steps name staged beside it, and require only what the platform provides,
// or the first box to apply the bundle is the first thing to say so.
func TestEveryHarnessFeatureLoadsAndRequiresOnlyThePlatform(t *testing.T) {
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
	for _, dir := range harness.BuiltinDirectories() {
		t.Run(dir, func(t *testing.T) {
			staged := harnesstest.Stage(t, dir)
			if !staged.Declaration().Coding() {
				if staged.Target != harnessbundle.TargetLabel || len(staged.Files) != 3 {
					t.Fatalf("%s installs no agent, so its bundle is its label alone; staged %d files for target %s", dir, len(staged.Files), staged.Target)
				}
				return
			}
			if staged.Target != harnessbundle.TargetBundle || staged.Feature.Name != dir {
				t.Fatalf("staged feature %q for target %s, want feature %q built by the bundle stage", staged.Feature.Name, staged.Target, dir)
			}
			for _, name := range staged.Feature.Spec.Requires {
				if !slices.Contains(provided, name) {
					t.Errorf("requires %q, which the platform does not provide", name)
				}
			}
		})
	}
}

// Staging is deterministic: the same declaration and runtime stage the same
// bytes, so an unchanged harness's bundle layers are the same ones a box
// already holds.
func TestStagingIsDeterministic(t *testing.T) {
	for _, dir := range harness.BuiltinDirectories() {
		a, b := harnesstest.Stage(t, dir), harnesstest.Stage(t, dir)
		if len(a.Files) != len(b.Files) {
			t.Fatalf("%s staged %d files, then %d", dir, len(a.Files), len(b.Files))
		}
		for name, f := range a.Files {
			if g, ok := b.Files[name]; !ok || string(g.Content) != string(f.Content) || g.Mode != f.Mode {
				t.Errorf("%s: %s differs between two stagings", dir, name)
			}
		}
	}
}
