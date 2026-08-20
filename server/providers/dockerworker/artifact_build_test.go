package dockerworker

import (
	"strings"
	"testing"

	"github.com/obot-platform/discobox/devimage"
)

// The arguments are checked before a daemon is contacted, so a
// misconfiguration is reported as itself rather than as a build failure
// halfway through streaming a context.
func TestBuildArtifactsRejectsUnusableArguments(t *testing.T) {
	absolute := t.TempDir()
	for name, testCase := range map[string]struct {
		spec   devimage.BuildSpec
		output string
	}{
		"no dockerfile":    {spec: devimage.BuildSpec{Context: absolute}, output: absolute},
		"relative context": {spec: devimage.BuildSpec{Dockerfile: "Dockerfile", Context: "image"}, output: absolute},
		"relative output":  {spec: devimage.BuildSpec{Dockerfile: "Dockerfile", Context: absolute}, output: "guest"},
	} {
		t.Run(name, func(t *testing.T) {
			err := BuildArtifacts(t.Context(), nil, testCase.spec, testCase.output)
			if err == nil {
				t.Fatal("BuildArtifacts accepted the arguments")
			}
		})
	}
}

// Both build paths render the same frontend attributes, because they are the
// same Dockerfile frontend: only the exporter differs.
func TestFrontendAttrsRenderTheBuildSpecification(t *testing.T) {
	attrs := frontendAttrs(&devimage.BuildSpec{
		Dockerfile: "server/providers/vz/image/Dockerfile",
		Context:    "/repo",
		Platform:   "linux/arm64",
		Target:     "assemble",
		Args:       map[string]string{"ROOT_SLACK_MIB": "256"},
	})
	for key, want := range map[string]string{
		"filename":                 "server/providers/vz/image/Dockerfile",
		"platform":                 "linux/arm64",
		"target":                   "assemble",
		"build-arg:ROOT_SLACK_MIB": "256",
	} {
		if got := attrs[key]; got != want {
			t.Errorf("attrs[%q] = %q, want %q", key, got, want)
		}
	}
}

// An empty platform or target must be absent rather than present and blank:
// BuildKit treats a blank platform as a parse error, not as "unspecified".
func TestFrontendAttrsOmitEmptyOptionalFields(t *testing.T) {
	attrs := frontendAttrs(&devimage.BuildSpec{Dockerfile: "Dockerfile", Platform: "  ", Target: ""})
	for _, key := range []string{"platform", "target"} {
		if _, ok := attrs[key]; ok {
			t.Errorf("attrs carries an empty %q", key)
		}
	}
	if len(attrs) != 1 || !strings.Contains(attrs["filename"], "Dockerfile") {
		t.Errorf("attrs = %v, want only a filename", attrs)
	}
}
