package dockerworker

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// The pool agent runs the sandbox base image for every sandbox delivered its
// platform (ADR 26-10-16-423 §§7, 10), so the image travels in its
// environment, and a moved one recreates the pool container that would
// otherwise run the old one.
func TestTheSandboxBaseImageReachesThePoolAgent(t *testing.T) {
	engine, err := New(Config{SandboxBaseImage: "base:v1"}, nopDriver{})
	if err != nil {
		t.Fatal(err)
	}
	env := engine.poolPolicyEnv()
	if got := env[sandboxruntime.EnvSandboxBaseImage]; got != "base:v1" {
		t.Fatalf("%s = %q, want base:v1", sandboxruntime.EnvSandboxBaseImage, got)
	}
	moved, err := New(Config{SandboxBaseImage: "base:v2"}, nopDriver{})
	if err != nil {
		t.Fatal(err)
	}
	if moved.ConfigRevision() == engine.ConfigRevision() {
		t.Fatal("a moved sandbox base image left the pool container's revision as it was")
	}
}

// The sandbox base builds in the Node the node feature would install (ADR
// 26-11-19-780): an exact package from the NodeSource line install-node.sh
// installs, new enough for the feature's adopt check, so a box adopts it and
// its first run installs nothing; and both trust NodeSource's repository by
// the same key, the feature's own copy holding the image's pinned
// fingerprint. A feature moved to a new line, a raised minimum or a key moved
// in one place alone fails here until the other moves with it.
func TestTheSandboxBaseShipsTheNodeTheFeatureWouldInstall(t *testing.T) {
	const feature = "../../../sandbox-agent/features/node"
	dockerfile, err := os.ReadFile("sandbox-image/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	pin := regexp.MustCompile(`(?m)^ARG NODE_VERSION=((\d+)\.\d+\.\d+)-\d+nodesource\d+$`).FindSubmatch(dockerfile)
	if pin == nil {
		t.Fatal("the sandbox base image's Dockerfile pins no NODE_VERSION=<major.minor.patch>-<n>nodesource<n>")
	}
	install, err := os.ReadFile(feature + "/install-node.sh")
	if err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`(?m)^line=(\d+)$`).FindSubmatch(install)
	if line == nil {
		t.Fatal("install-node.sh names no NodeSource line (line=<major>)")
	}
	if string(pin[2]) != string(line[1]) {
		t.Errorf("the image's Node is %s.x and the node feature installs %s.x: move NODE_VERSION with install-node.sh", pin[2], line[1])
	}

	node, err := provisionspec.LoadFeature(feature)
	if err != nil {
		t.Fatal(err)
	}
	adopt := node.Spec.Adopt
	if adopt == nil || adopt.Version == nil || adopt.Version.Command != "node" {
		t.Fatalf("the node feature's adopt check has no node version floor: %+v", adopt)
	}
	if !adopt.Version.Admits(string(pin[1])) {
		t.Errorf("the image's Node %s is older than the node feature adopts (%s)", pin[1], adopt.Version.Minimum)
	}

	fingerprint := regexp.MustCompile(`(?m)^ARG NODESOURCE_APT_KEY_FINGERPRINT=([0-9A-F]{40})$`).FindSubmatch(dockerfile)
	if fingerprint == nil {
		t.Fatal("the sandbox base image's Dockerfile pins no NODESOURCE_APT_KEY_FINGERPRINT")
	}
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skipf("gpg reads the node feature's key: %v", err)
	}
	//nolint:gosec // The key is the repository's own file.
	cmd := exec.CommandContext(t.Context(), "gpg", "--show-keys", "--with-colons", feature+"/nodesource-repo.asc")
	cmd.Env = append(os.Environ(), "GNUPGHOME="+t.TempDir())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gpg --show-keys: %v", err)
	}
	var primaries []string
	primary := false
	for _, record := range strings.Split(string(out), "\n") {
		fields := strings.Split(record, ":")
		switch {
		case fields[0] == "pub":
			primary = true
		case fields[0] == "fpr" && primary && len(fields) > 9:
			primaries = append(primaries, fields[9])
			primary = false
		}
	}
	if strings.Join(primaries, " ") != string(fingerprint[1]) {
		t.Errorf("the node feature's nodesource-repo.asc holds %v, and the image pins %s: move both together", primaries, fingerprint[1])
	}
}
