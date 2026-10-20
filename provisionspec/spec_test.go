package provisionspec

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Two trees are applied together, and a feature in both is refused.
func TestLoadMergesTreesAndRefusesAFeatureInTwo(t *testing.T) {
	dir := t.TempDir()
	write := func(tree, feature string) {
		path := filepath.Join(dir, tree, FeaturesDir, feature, FileName)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("apiVersion: discobox.ai/provision/v1\nsteps: []\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("platform", "core")
	write("harness", "claude-code")
	features, err := Load(filepath.Join(dir, "platform"), filepath.Join(dir, "harness"))
	if err != nil || len(features) != 2 {
		t.Fatalf("features = %v, %v", features, err)
	}
	write("harness", "core")
	if _, err := Load(filepath.Join(dir, "platform"), filepath.Join(dir, "harness")); err == nil || !strings.Contains(err.Error(), `feature "core" is in both`) {
		t.Fatalf("error = %v", err)
	}
}

// A step names a file the feature does not have: refused at load.
func TestLoadRefusesAMissingScript(t *testing.T) {
	tree := t.TempDir()
	path := filepath.Join(tree, FeaturesDir, "f", FileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("apiVersion: discobox.ai/provision/v1\nsteps:\n  - name: s\n    run: nope.sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(tree); err == nil || !strings.Contains(err.Error(), `"nope.sh" is not a file in the feature's directory`) {
		t.Fatalf("error = %v", err)
	}
}

func TestParseRefuses(t *testing.T) {
	const head = "apiVersion: discobox.ai/provision/v1\nsteps:\n"
	cases := map[string]string{
		"wrong version":       "apiVersion: discobox.ai/provision/v2\nsteps: []\n",
		"unknown field":       head + "  - name: a\n    run: a.sh\n    sudo: true\n",
		"no kind":             head + "  - name: a\n",
		"two kinds":           head + "  - name: a\n    run: a.sh\n    group: {name: g}\n",
		"duplicate name":      head + "  - name: a\n    run: a.sh\n  - name: a\n    run: b.sh\n",
		"bad name":            head + "  - name: A/b\n    run: a.sh\n",
		"inline script":       head + "  - name: a\n    run: \"echo hi; rm -rf /\"\n",
		"climbing script":     head + "  - name: a\n    run: ../other/a.sh\n",
		"absolute script":     head + "  - name: a\n    check: /bin/true\n    run: a.sh\n",
		"relative file path":  head + "  - name: a\n    file: {path: etc/a, source: a}\n",
		"bad mode":            head + "  - name: a\n    file: {path: /etc/a, source: a, mode: \"4755\"}\n",
		"unknown merge":       head + "  - name: a\n    file: {path: /etc/a, source: a, merge: yaml}\n",
		"bad unit":            head + "  - name: a\n    systemd: {enable: [\"docker\"]}\n",
		"empty systemd":       head + "  - name: a\n    systemd: {}\n",
		"drop-in name":        head + "  - name: a\n    systemd: {dropIn: {unit: a.service, name: x, source: x}}\n",
		"empty env":           head + "  - name: a\n    env: {}\n",
		"PATH in vars":        head + "  - name: a\n    env: {vars: {PATH: /x}}\n",
		"http download":       head + "  - name: a\n    download: {path: /x, arch: {amd64: {url: \"http://x\", sha256: \"" + sha("x") + "\"}}}\n",
		"short sha256":        head + "  - name: a\n    download: {path: /x, arch: {amd64: {url: \"https://x\", sha256: abc}}}\n",
		"unknown arch":        head + "  - name: a\n    download: {path: /x, arch: {riscv64: {url: \"https://x\", sha256: \"" + sha("x") + "\"}}}\n",
		"other family":        head + "  - name: a\n    packages: {fedora: [jq]}\n",
		"no packages":         head + "  - name: a\n    packages: {debian: []}\n",
		"bad package":         head + "  - name: a\n    packages: {debian: [\"jq; rm\"]}\n",
		"unknown os":          head + "  - name: a\n    when: {os: [plan9]}\n    run: a.sh\n",
		"bad requirement":     "apiVersion: discobox.ai/provision/v1\nrequires: [\"../x\"]\nsteps: []\n",
		"double separator":    head + "  - name: a--b\n    run: a.sh\n",
		"trailing separator":  head + "  - name: a-\n    run: a.sh\n",
		"numeric group":       head + "  - name: a\n    group: {name: \"997\"}\n",
		"group with zero gid": head + "  - name: a\n    group: {name: g, gid: 0}\n",
	}
	for name, data := range cases {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := Parse([]byte(head + "  - name: a\n    packages: {debian: [jq, \"git=1:2.43.0-1\"]}\n")); err != nil {
		t.Errorf("a pinned package: %v", err)
	}
}
