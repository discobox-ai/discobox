package provisionspec

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
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

// A bundle that declares no features — the shell harness's, which holds only
// its label — composes to a tree with no features directory, and adds nothing
// beside the platform's; a tree that is not there at all is still refused.
func TestLoadTakesATreeWithNoFeatures(t *testing.T) {
	dir := t.TempDir()
	platform := filepath.Join(dir, "platform", FeaturesDir, "core", FileName)
	if err := os.MkdirAll(filepath.Dir(platform), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(platform, []byte("apiVersion: discobox.ai/provision/v1\nsteps: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "harness-shell")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	features, err := Load(filepath.Join(dir, "platform"), empty)
	if err != nil || len(features) != 1 || features[0].Name != "core" {
		t.Fatalf("Load(platform, empty) = %v, %v; want the platform's feature alone", features, err)
	}
	if _, err := Load(filepath.Join(dir, "platform"), filepath.Join(dir, "never-composed")); err == nil {
		t.Fatal("a tree that is not there was taken for one with no features")
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
		"inputs, not run":     head + "  - name: a\n    inputs: [x]\n    file: {path: /etc/a, source: a}\n",
		"climbing input":      head + "  - name: a\n    run: a.sh\n    inputs: [../x]\n",
		"negative strip":      head + "  - name: a\n    download: {path: /x, unpack: {strip: -1}, arch: {amd64: {url: \"https://x\", sha256: \"" + sha("x") + "\"}}}\n",
		"mode on an archive":  head + "  - name: a\n    download: {path: /x, mode: \"0755\", unpack: {}, arch: {amd64: {url: \"https://x\", sha256: \"" + sha("x") + "\"}}}\n",
		"bad requirement":     "apiVersion: discobox.ai/provision/v1\nrequires: [\"../x\"]\nsteps: []\n",
		"double separator":    head + "  - name: a--b\n    run: a.sh\n",
		"trailing separator":  head + "  - name: a-\n    run: a.sh\n",
		"numeric group":       head + "  - name: a\n    group: {name: \"997\"}\n",
		"group with zero gid": head + "  - name: a\n    group: {name: g, gid: 0}\n",
		"install, no adopt":   head + "  - name: a\n    install: true\n    run: a.sh\n",
		"async wiring":        head + "  - name: a\n    async: true\n    env: {vars: {A: b}}\n",
		"install wiring":      "apiVersion: discobox.ai/provision/v1\nadopt: {commands: [g]}\nsteps:\n  - name: a\n    install: true\n    group: {name: g}\n",
		"apt on packages":     head + "  - name: a\n    apt: true\n    packages: {debian: [jq]}\n",
		"adopt no command":    "apiVersion: discobox.ai/provision/v1\nadopt: {commands: []}\nsteps: []\n",
		"adopt bad version":   "apiVersion: discobox.ai/provision/v1\nadopt: {commands: [node], version: {command: node, minimum: v22}}\nsteps: []\n",
		"adopt relative path": "apiVersion: discobox.ai/provision/v1\nadopt: {commands: [node], writable: [usr/lib]}\nsteps: []\n",
		"bad command":         "apiVersion: discobox.ai/provision/v1\ncommands: [\"a b\"]\nsteps: []\n",
	}
	for name, data := range cases {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := Parse([]byte(head + "  - name: a\n    packages: {debian: [jq, \"git=1:2.43.0-1\"]}\n")); err != nil {
		t.Errorf("a pinned package: %v", err)
	}
	feature := "apiVersion: discobox.ai/provision/v1\nasync: true\ncommands: [node, npm]\nadopt:\n  commands: [node]\n  version: {command: node, minimum: \"22.1\"}\n  writable: [/usr/local/lib/node_modules]\nsteps:\n  - name: a\n    install: true\n    apt: true\n    run: a.sh\n"
	if _, err := Parse([]byte(feature)); err != nil {
		t.Errorf("a feature with adopt, commands and async: %v", err)
	}
}

// A box installs its own distribution's list, else its nearest relative's:
// os-release lists ID_LIKE nearest first, so Pop!_OS (like "ubuntu debian")
// takes Ubuntu's, and a Debian derivative with no list of its own Debian's.
func TestPackagesForPrefersTheBoxsOwnDistribution(t *testing.T) {
	both := PackagesStep{Debian: []string{"chromium"}, Ubuntu: []string{"libnss3"}}
	for _, c := range []struct {
		step   PackagesStep
		id     string
		like   []string
		want   []string
		wantOK bool
	}{
		{both, "debian", nil, []string{"chromium"}, true},
		{both, "ubuntu", []string{"debian"}, []string{"libnss3"}, true},
		{both, "pop", []string{"ubuntu", "debian"}, []string{"libnss3"}, true},
		{both, "kali", []string{"debian"}, []string{"chromium"}, true},
		{PackagesStep{Debian: []string{"jq"}}, "ubuntu", []string{"debian"}, []string{"jq"}, true},
		{PackagesStep{Ubuntu: []string{"jq"}}, "debian", nil, nil, false},
	} {
		got, ok := c.step.For(c.id, c.like)
		if ok != c.wantOK || !slices.Equal(got, c.want) {
			t.Errorf("%s like %v: For = %v, %v; want %v, %v", c.id, c.like, got, ok, c.want, c.wantOK)
		}
	}
}
