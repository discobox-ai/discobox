package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"gopkg.in/yaml.v3"
)

// The primary terminal's first launch waits for readiness and then copies
// /usr/local/share/discobox/skills into the harness's skill directories, once
// (terminal.installSkills). A built-in skill a feature places after readiness
// is therefore missing from every box whose first launch beat it: each step
// that writes one has to be one readiness waits for.
func TestEveryBuiltinSkillIsPlacedBeforeReadiness(t *testing.T) {
	const builtinSkills = "/usr/local/share/discobox/skills/"
	entries, err := os.ReadDir(filepath.Join("..", "features"))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Read rather than loaded: core's tree is completed by the Dockerfile.
		data, err := os.ReadFile(filepath.Join("..", "features", entry.Name(), provisionspec.FileName))
		if err != nil {
			t.Fatal(err)
		}
		var spec provisionspec.Spec
		if err := yaml.Unmarshal(data, &spec); err != nil {
			t.Fatal(err)
		}
		feature := provisionspec.Feature{Name: entry.Name(), Spec: spec}
		for i, step := range feature.Spec.Steps {
			if step.File == nil || !strings.HasPrefix(step.File.Path, builtinSkills) {
				continue
			}
			found++
			background := feature.Spec.Async
			for _, earlier := range feature.Spec.Steps[:i+1] {
				background = background || earlier.Async
			}
			if background {
				t.Errorf("feature %s places %s after readiness (async), so the first launch can copy the built-in skills without it", feature.Name, step.File.Path)
			}
		}
	}
	if found == 0 {
		t.Fatal("no feature places a built-in skill")
	}
}
