package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/provisionspec"
	"github.com/discobox-ai/discobox/sandbox-agent/runuser"
)

var ubuntu = Platform{OS: "linux", Arch: "amd64", ID: "ubuntu", IDLike: []string{"debian"}}

// box is a runner over a temporary root, whose system tools are recorded
// rather than run. Scripts are run for real.
type box struct {
	t      *testing.T
	root   string
	tree   string
	runner *Runner
	tools  []string
	served map[string]string
}

func newBox(t *testing.T) *box {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(runuser.FixedDatabase())
	b := &box{t: t, root: filepath.Join(dir, "root"), tree: filepath.Join(dir, "tree"), served: map[string]string{}}
	b.runner = &Runner{
		Root:     b.root,
		Records:  Records{Dir: filepath.Join(b.root, "opt/discobox/provision")},
		Platform: ubuntu,
		Command: func(ctx context.Context, dir string, env []string, name string, args ...string) error {
			switch name {
			case "systemctl", "groupadd", "apt-get":
				b.tools = append(b.tools, strings.Join(append([]string{name}, args...), " "))
				return nil
			}
			return runCommand(ctx, dir, env, name, args...)
		},
		Fetch: func(_ context.Context, url string) (io.ReadCloser, error) {
			body, ok := b.served[url]
			if !ok {
				return nil, errors.New("not served: " + url)
			}
			return io.NopCloser(strings.NewReader(body)), nil
		},
	}
	if err := os.MkdirAll(filepath.Join(b.root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	return b
}

// feature writes a feature's files into the tree.
func (b *box) feature(name string, files map[string]string) {
	b.t.Helper()
	dir := filepath.Join(b.tree, provisionspec.FeaturesDir, name)
	for file, content := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(file, ".sh") {
			mode = 0o755
		}
		path := filepath.Join(dir, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			b.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			b.t.Fatal(err)
		}
	}
}

func (b *box) apply() ([]Outcome, error) {
	b.t.Helper()
	b.tools = nil
	features, err := provisionspec.Load(b.tree)
	if err != nil {
		b.t.Fatalf("load: %v", err)
	}
	return b.runner.Apply(context.Background(), features)
}

func (b *box) mustApply() []Outcome {
	b.t.Helper()
	outcomes, err := b.apply()
	if err != nil {
		b.t.Fatalf("apply: %v", err)
	}
	return outcomes
}

func (b *box) read(path string) string {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(b.root, path))
	if err != nil {
		b.t.Fatal(err)
	}
	return string(data)
}

func results(outcomes []Outcome) map[string]Result {
	got := map[string]Result{}
	for _, o := range outcomes {
		got[o.Feature+"/"+o.Step] = o.Result
	}
	return got
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Every step kind is applied on the first run, and a second run over the same
// box changes nothing: each step is skipped on its record, and no tool runs.
func TestEveryKindAppliesOnceAndTheSecondRunChangesNothing(t *testing.T) {
	b := newBox(t)
	marker := filepath.Join(t.TempDir(), "ran")
	b.served["https://example.com/tool-amd64"] = "tool for amd64"
	b.feature("core", map[string]string{
		provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: group
    group: {name: discobox}
  - name: launcher
    file: {path: /usr/local/bin/launch, source: launch.sh, mode: "0755"}
  - name: units
    systemd:
      enable: [discobox-agent.service]
      mask: [getty@.service]
      dropIn: {unit: docker.service, name: 10-discobox.conf, source: docker.conf}
  - name: env
    env:
      vars: {DISCOBOX_HOME: "/opt/it's here"}
      path: [/opt/discobox/bin, /opt/discobox/sbin]
  - name: tool
    download:
      path: /opt/discobox/bin/tool
      mode: "0755"
      arch:
        amd64: {url: "https://example.com/tool-amd64", sha256: "` + sha("tool for amd64") + `"}
        arm64: {url: "https://example.com/tool-arm64", sha256: "` + sha("tool for arm64") + `"}
  - name: packages
    packages: {debian: [jq, ripgrep]}
  - name: install
    run: install.sh
`,
		"launch.sh":   "#!/bin/sh\nexec true\n",
		"docker.conf": "[Service]\nEnvironment=A=1\n",
		"install.sh":  "#!/bin/sh\necho run >> " + marker + "\n",
	})

	first := results(b.mustApply())
	for _, step := range []string{"group", "launcher", "units", "env", "tool", "packages", "install"} {
		if first["core/"+step] != Applied {
			t.Errorf("first run: core/%s is %q, want applied", step, first["core/"+step])
		}
	}
	wantTools := []string{
		"groupadd --system discobox",
		"systemctl --root=" + b.root + " mask getty@.service",
		"systemctl --root=" + b.root + " enable discobox-agent.service",
		"apt-get update",
		"apt-get install -y --no-install-recommends jq ripgrep",
	}
	if !slices.Equal(b.tools, wantTools) {
		t.Errorf("tools run:\n%s\nwant:\n%s", strings.Join(b.tools, "\n"), strings.Join(wantTools, "\n"))
	}
	if got := b.read("usr/local/bin/launch"); got != "#!/bin/sh\nexec true\n" {
		t.Errorf("launcher = %q", got)
	}
	if info, err := os.Stat(filepath.Join(b.root, "usr/local/bin/launch")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("launcher mode = %v, %v", info.Mode(), err)
	}
	if got := b.read("etc/systemd/system/docker.service.d/10-discobox.conf"); got != "[Service]\nEnvironment=A=1\n" {
		t.Errorf("drop-in = %q", got)
	}
	if got := b.read("opt/discobox/bin/tool"); got != "tool for amd64" {
		t.Errorf("download = %q", got)
	}
	profile := b.read(EnvProfilePath("core", "env"))
	for _, want := range []string{
		`export DISCOBOX_HOME='/opt/it'\''s here'`,
		`*:'/opt/discobox/sbin':*) ;; *) PATH='/opt/discobox/sbin':"${PATH}"`,
		"export PATH\n",
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile lacks %q:\n%s", want, profile)
		}
	}
	// The first path listed is prepended last, so it comes first.
	if strings.Index(profile, "/opt/discobox/sbin") > strings.Index(profile, "/opt/discobox/bin") {
		t.Errorf("profile prepends /opt/discobox/bin before /opt/discobox/sbin:\n%s", profile)
	}

	second := results(b.mustApply())
	for step, result := range second {
		if result != Skipped {
			t.Errorf("second run: %s is %q, want skipped", step, result)
		}
	}
	if len(b.tools) != 0 {
		t.Errorf("second run ran tools: %v", b.tools)
	}
	ran, err := os.ReadFile(marker)
	if err != nil || string(ran) != "run\n" {
		t.Errorf("install.sh ran %q times, want once (%v)", ran, err)
	}
}

// The profile an env step writes is valid sh, and gives PATH each directory
// once in the order listed.
func TestEnvProfileIsSourceable(t *testing.T) {
	profile := envProfile("f", "s", &provisionspec.EnvStep{Vars: map[string]string{"A": "x y"}, Path: []string{"/a", "/b"}})
	path := filepath.Join(t.TempDir(), "profile.sh")
	if err := os.WriteFile(path, profile, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out")
	b := newBox(t)
	err := b.runner.command(context.Background(), "/", []string{"PATH=/b:/usr/bin:/bin"}, "/bin/sh", "-c", `. "$1"; . "$1"; printf '%s|%s' "$A" "$PATH" > "$2"`, "sh", path, out)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "x y|/a:/b:/usr/bin:/bin" {
		t.Errorf("sourced twice: %q", got)
	}
}

// Editing a file a step names re-applies that step and no other.
func TestEditingAStepsFileReappliesOnlyThatStep(t *testing.T) {
	b := newBox(t)
	b.feature("node", map[string]string{
		provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: a
    file: {path: /etc/a, source: a.txt}
  - name: b
    file: {path: /etc/b, source: b.txt}
`,
		"a.txt": "a1",
		"b.txt": "b1",
	})
	b.mustApply()
	b.feature("node", map[string]string{"b.txt": "b2"})
	got := results(b.mustApply())
	if got["node/a"] != Skipped || got["node/b"] != Applied {
		t.Errorf("after editing b.txt: %v, want a skipped and b applied", got)
	}
	if b.read("etc/b") != "b2" {
		t.Errorf("/etc/b = %q", b.read("etc/b"))
	}
}

// Changing a step's definition re-applies it.
func TestChangingAStepsDefinitionReappliesIt(t *testing.T) {
	b := newBox(t)
	spec := func(path string) string {
		return "apiVersion: discobox.ai/provision/v1\nsteps:\n  - name: a\n    file: {path: " + path + ", source: a.txt}\n"
	}
	b.feature("node", map[string]string{provisionspec.FileName: spec("/etc/a"), "a.txt": "a"})
	b.mustApply()
	b.feature("node", map[string]string{provisionspec.FileName: spec("/etc/a2")})
	if got := results(b.mustApply()); got["node/a"] != Applied {
		t.Errorf("after moving the file: %v, want applied", got)
	}
	if b.read("etc/a2") != "a" {
		t.Error("the moved file was not placed")
	}
}

// A recorded step whose check reports drift is applied again; one whose check
// finds it in place is skipped.
func TestCheckReportsDrift(t *testing.T) {
	b := newBox(t)
	target := filepath.Join(b.root, "etc/thing")
	b.feature("thing", map[string]string{
		provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: thing
    check: check.sh
    run: install.sh
`,
		"check.sh":   "#!/bin/sh\ntest -f " + target + "\n",
		"install.sh": "#!/bin/sh\necho installed > " + target + "\n",
	})
	if got := results(b.mustApply()); got["thing/thing"] != Applied {
		t.Fatalf("first run: %v", got)
	}
	if got := results(b.mustApply()); got["thing/thing"] != Skipped {
		t.Fatalf("in place: %v, want skipped", got)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if got := results(b.mustApply()); got["thing/thing"] != Reapplied {
		t.Fatalf("after removing it: %v, want reapplied", got)
	}
	if b.read("etc/thing") != "installed\n" {
		t.Error("the drifted step did not put it back")
	}
}

// A failed step is not recorded, so the next run tries it again, and the
// steps before it stay recorded.
func TestAFailedStepIsNotRecorded(t *testing.T) {
	b := newBox(t)
	flag := filepath.Join(t.TempDir(), "ok")
	b.feature("f", map[string]string{
		provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: first
    file: {path: /etc/first, source: first}
  - name: flaky
    run: flaky.sh
  - name: last
    file: {path: /etc/last, source: first}
`,
		"first":    "x",
		"flaky.sh": "#!/bin/sh\ntest -f " + flag + "\n",
	})
	outcomes, err := b.apply()
	if err == nil || !strings.Contains(err.Error(), `feature "f": step "flaky"`) {
		t.Fatalf("error = %v, want one naming the failed step", err)
	}
	if got := results(outcomes); got["f/first"] != Applied || got["f/last"] != "" {
		t.Fatalf("outcomes = %v, want first applied and last not reached", got)
	}
	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := results(b.mustApply())
	if got["f/first"] != Skipped || got["f/flaky"] != Applied || got["f/last"] != Applied {
		t.Fatalf("retry = %v", got)
	}
}

// Steps apply only where their selector selects the box, by os, arch, and
// os-release ID or ID_LIKE.
func TestSelectors(t *testing.T) {
	cases := []struct {
		selector *provisionspec.Selector
		want     bool
	}{
		{nil, true},
		{&provisionspec.Selector{OS: []string{"linux"}}, true},
		{&provisionspec.Selector{OS: []string{"darwin"}}, false},
		{&provisionspec.Selector{Arch: []string{"arm64"}}, false},
		{&provisionspec.Selector{Arch: []string{"arm64", "amd64"}}, true},
		{&provisionspec.Selector{Distro: []string{"ubuntu"}}, true},
		{&provisionspec.Selector{Distro: []string{"debian"}}, true}, // ID_LIKE
		{&provisionspec.Selector{Distro: []string{"fedora"}}, false},
		{&provisionspec.Selector{OS: []string{"linux"}, Arch: []string{"amd64"}, Distro: []string{"fedora"}}, false},
	}
	for _, c := range cases {
		if got := ubuntu.Selects(c.selector); got != c.want {
			t.Errorf("Selects(%+v) = %v, want %v", c.selector, got, c.want)
		}
	}

	b := newBox(t)
	b.feature("f", map[string]string{
		provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: here
    when: {arch: [amd64], distro: [debian]}
    file: {path: /etc/here, source: x}
  - name: elsewhere
    when: {arch: [arm64]}
    file: {path: /etc/elsewhere, source: x}
`,
		"x": "x",
	})
	got := results(b.mustApply())
	if got["f/here"] != Applied || got["f/elsewhere"] != Unselected {
		t.Fatalf("outcomes = %v", got)
	}
	if _, err := os.Stat(filepath.Join(b.root, "etc/elsewhere")); !os.IsNotExist(err) {
		t.Errorf("an unselected step ran: %v", err)
	}
}

// Only the Debian family is provisioned; any other box is refused by name,
// before any step runs.
func TestOtherDistributionsAreRefusedByName(t *testing.T) {
	for _, p := range []Platform{
		{OS: "linux", Arch: "amd64", ID: "fedora", IDLike: []string{"rhel"}},
		{OS: "linux", Arch: "amd64", ID: "alpine"},
		{OS: "darwin", Arch: "arm64"},
	} {
		b := newBox(t)
		b.runner.Platform = p
		b.feature("f", map[string]string{provisionspec.FileName: "apiVersion: discobox.ai/provision/v1\nsteps:\n  - name: a\n    file: {path: /etc/a, source: x}\n", "x": "x"})
		_, err := b.apply()
		name := p.ID
		if name == "" {
			name = p.OS
		}
		if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "not supported") {
			t.Errorf("%+v: error = %v, want one naming %q as not supported", p, err, name)
		}
		if _, err := os.Stat(filepath.Join(b.root, "etc/a")); !os.IsNotExist(err) {
			t.Errorf("%+v: a step ran", p)
		}
	}
	for _, p := range []Platform{ubuntu, {OS: "linux", ID: "debian"}} {
		if err := p.Supported(); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
}

func TestDetectPlatformReadsOSRelease(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "usr/lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := "# comment\nNAME=\"Ubuntu\"\nID=ubuntu\nID_LIKE='debian'\n"
	if err := os.WriteFile(filepath.Join(root, "usr/lib/os-release"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := DetectPlatform(root)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "ubuntu" || !slices.Equal(p.IDLike, []string{"debian"}) {
		t.Errorf("platform = %+v", p)
	}
	if _, err := DetectPlatform(t.TempDir()); err == nil {
		t.Error("a root with no os-release was read")
	}
}

// A feature is applied after the features it requires; one whose requirement
// failed is not applied, and the rest still are.
func TestRequiresOrdersAndAFailureStopsDependents(t *testing.T) {
	b := newBox(t)
	order := filepath.Join(t.TempDir(), "order")
	step := func(requires string) string {
		spec := "apiVersion: discobox.ai/provision/v1\n"
		if requires != "" {
			spec += "requires: [" + requires + "]\n"
		}
		return spec + "steps:\n  - name: s\n    run: s.sh\n"
	}
	script := func(name string, fail bool) string {
		s := "#!/bin/sh\necho " + name + " >> " + order + "\n"
		if fail {
			s += "exit 3\n"
		}
		return s
	}
	b.feature("a-docker", map[string]string{provisionspec.FileName: step("z-core"), "s.sh": script("a-docker", false)})
	b.feature("b-broken", map[string]string{provisionspec.FileName: step(""), "s.sh": script("b-broken", true)})
	b.feature("c-needs-broken", map[string]string{provisionspec.FileName: step("b-broken"), "s.sh": script("c-needs-broken", false)})
	b.feature("z-core", map[string]string{provisionspec.FileName: step(""), "s.sh": script("z-core", false)})
	_, err := b.apply()
	if err == nil || !strings.Contains(err.Error(), `"c-needs-broken": not applied: it requires "b-broken"`) {
		t.Fatalf("error = %v", err)
	}
	ran, _ := os.ReadFile(order)
	if got := strings.Fields(string(ran)); !slices.Equal(got, []string{"z-core", "a-docker", "b-broken"}) {
		t.Errorf("ran %v", got)
	}
}

func TestOrderRefusesMissingAndCyclicRequirements(t *testing.T) {
	feature := func(name string, requires ...string) provisionspec.Feature {
		return provisionspec.Feature{Name: name, Spec: provisionspec.Spec{APIVersion: provisionspec.APIVersion, Requires: requires}}
	}
	if _, err := order([]provisionspec.Feature{feature("a", "nope")}); err == nil || !strings.Contains(err.Error(), `"nope", which is not there`) {
		t.Errorf("missing: %v", err)
	}
	if _, err := order([]provisionspec.Feature{feature("a", "b"), feature("b", "a")}); err == nil || !strings.Contains(err.Error(), "a → b → a") {
		t.Errorf("cycle: %v", err)
	}
}

// A download whose bytes do not match its sha256 fails and leaves what was
// at its path; one with nothing for the box's architecture fails by name.
func TestDownloadVerifiesItsDigest(t *testing.T) {
	b := newBox(t)
	b.served["https://example.com/tool"] = "tampered"
	b.feature("f", map[string]string{provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: tool
    download:
      path: /opt/tool
      arch:
        amd64: {url: "https://example.com/tool", sha256: "` + sha("genuine") + `"}
`})
	if err := os.MkdirAll(filepath.Join(b.root, "opt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.root, "opt/tool"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.apply(); err == nil || !strings.Contains(err.Error(), "sha256 is "+sha("tampered")) {
		t.Fatalf("error = %v", err)
	}
	if b.read("opt/tool") != "old" {
		t.Error("a mismatched download replaced the file")
	}
	entries, _ := os.ReadDir(filepath.Join(b.root, "opt"))
	if len(entries) != 1 {
		t.Errorf("left behind: %v", entries)
	}

	b.runner.Platform.Arch = "arm64"
	if _, err := b.apply(); err == nil || !strings.Contains(err.Error(), "names nothing for arm64") {
		t.Fatalf("error = %v", err)
	}
}

// A group already there is left alone; one there with another id than the
// step names is refused rather than renumbered. Whether it is there is asked
// of runuser, as boot asks it.
func TestGroupAlreadyThere(t *testing.T) {
	b := newBox(t)
	b.feature("f", map[string]string{provisionspec.FileName: "apiVersion: discobox.ai/provision/v1\nsteps:\n  - name: g\n    group: {name: docker}\n"})
	b.mustApply()
	if len(b.tools) != 0 {
		t.Errorf("ran %v for a group already there", b.tools)
	}

	b = newBox(t)
	b.feature("f", map[string]string{provisionspec.FileName: "apiVersion: discobox.ai/provision/v1\nsteps:\n  - name: g\n    group: {name: docker, gid: 2001}\n"})
	if _, err := b.apply(); err == nil || !strings.Contains(err.Error(), "already there with gid 997, not 2001") {
		t.Fatalf("error = %v", err)
	}

	b = newBox(t)
	b.feature("f", map[string]string{provisionspec.FileName: "apiVersion: discobox.ai/provision/v1\nsteps:\n  - name: g\n    group: {name: kvm, gid: 2001}\n"})
	b.mustApply()
	if want := "groupadd --gid 2001 kvm"; !slices.Equal(b.tools, []string{want}) {
		t.Errorf("ran %v, want %q", b.tools, want)
	}
}

// No two env steps write one script, however their names split, and a
// script whose step is renamed or removed is taken away with its record.
func TestEnvScriptsAreTheirStepsAlone(t *testing.T) {
	if EnvProfilePath("claude-code", "env") == EnvProfilePath("claude", "code-env") {
		t.Fatal("two steps share a profile script")
	}
	b := newBox(t)
	spec := func(step string) string {
		return "apiVersion: discobox.ai/provision/v1\nsteps:\n  - name: " + step + "\n    env: {vars: {A: x}}\n"
	}
	b.feature("claude", map[string]string{provisionspec.FileName: spec("code-env")})
	b.feature("claude-code", map[string]string{provisionspec.FileName: spec("env")})
	b.mustApply()
	b.feature("claude-code", map[string]string{provisionspec.FileName: spec("renamed")})
	b.mustApply()
	for path, want := range map[string]bool{
		EnvProfilePath("claude", "code-env"):     true,
		EnvProfilePath("claude-code", "env"):     false,
		EnvProfilePath("claude-code", "renamed"): true,
	} {
		_, err := os.Stat(filepath.Join(b.root, path))
		if got := err == nil; got != want {
			t.Errorf("%s there = %v, want %v", path, got, want)
		}
	}
	if _, ok, _ := b.runner.Records.Step("claude-code", "env"); ok {
		t.Error("the removed step's record is still there")
	}
	// The step keeps its name but becomes a run step: its script goes, and
	// the record the run step was given stays, so a second run runs nothing.
	marker := filepath.Join(t.TempDir(), "ran")
	b.feature("claude-code", map[string]string{
		provisionspec.FileName: "apiVersion: discobox.ai/provision/v1\nsteps:\n  - name: renamed\n    run: s.sh\n",
		"s.sh":                 "#!/bin/sh\necho run >> " + marker + "\n",
	})
	b.mustApply()
	if got := results(b.mustApply()); got["claude-code/renamed"] != Skipped {
		t.Errorf("second run = %v, want skipped", got)
	}
	if _, err := os.Stat(filepath.Join(b.root, EnvProfilePath("claude-code", "renamed"))); !os.IsNotExist(err) {
		t.Errorf("the env script outlived its step: %v", err)
	}
	if ran, _ := os.ReadFile(marker); string(ran) != "run\n" {
		t.Errorf("ran %q, want once", ran)
	}
}

// A download whose server stops sending is given up, not waited on.
func TestFetchGivesUpOnAStalledServer(t *testing.T) {
	saved := fetchStall
	fetchStall = 50 * time.Millisecond
	t.Cleanup(func() { fetchStall = saved })
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	body, err := httpFetch(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	_, err = io.ReadAll(body)
	if err == nil || !strings.Contains(err.Error(), "nothing received") {
		t.Fatalf("read = %v, want a stall", err)
	}
}

// The sandbox phase records the sandbox it ran for beside the step records.
func TestRecordsKeepTheSandbox(t *testing.T) {
	records := Records{Dir: filepath.Join(t.TempDir(), "provision")}
	if got := records.Sandbox(); got != "" {
		t.Fatalf("empty records name %q", got)
	}
	if err := records.RecordSandbox("sbx_1"); err != nil {
		t.Fatal(err)
	}
	if got := records.Sandbox(); got != "sbx_1" {
		t.Fatalf("records name %q", got)
	}
}

// A record that cannot be read as one is no record: the step is applied again
// rather than the run failing.
func TestATornRecordIsNoRecord(t *testing.T) {
	b := newBox(t)
	b.feature("f", map[string]string{provisionspec.FileName: "apiVersion: discobox.ai/provision/v1\nsteps:\n  - name: a\n    file: {path: /etc/a, source: x}\n", "x": "x"})
	b.mustApply()
	if err := os.WriteFile(b.runner.Records.stepPath("f", "a"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := results(b.mustApply()); got["f/a"] != Applied {
		t.Fatalf("outcomes = %v", got)
	}
}
