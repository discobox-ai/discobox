package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// The pool bundle's tree loads (every provision.yaml valid, every feature's
// requirement in it), with the binaries the build copies beside the
// features in place, and its units read the files a maker places at the
// paths this package names.
func TestThePoolBundleLoads(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(filepath.Join(root, provisionspec.FeaturesDir), os.DirFS("features")); err != nil {
		t.Fatal(err)
	}
	// What the Dockerfile's bundle stage copies in from the build.
	for _, binary := range []string{
		"pool-agent/discobox-pool-agent",
		"pool-builder/bin/discobox-pool-runc",
		"pool-builder/bin/buildkitd",
		"pool-builder/bin/registry",
	} {
		path := filepath.Join(root, provisionspec.FeaturesDir, binary)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	features, err := provisionspec.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, feature := range features {
		names = append(names, feature.Name)
	}
	if got := strings.Join(names, " "); got != "pool-agent pool-builder" {
		t.Fatalf("features = %s", got)
	}
	unit, err := os.ReadFile(filepath.Join("features", "pool-agent", "systemd", "discobox-pool-agent.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DISCOBOX_POOL_BOOTSTRAP_FILE=" + BootstrapFile, "EnvironmentFile=-" + EnvironmentFile, "Type=notify"} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("the agent's unit does not say %s", want)
		}
	}
}
