package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/provisionspec"
)

const head = "apiVersion: discobox.ai/provision/v1\n"

// tracked gives the box's runner a tracker writing provision.json under its
// root, placeholders and logs, and background provisioning.
func (b *box) tracked() *Tracker {
	b.t.Helper()
	tracker := NewTracker(filepath.Join(b.root, "run/discobox/provision.json"))
	b.runner.Tracker = tracker
	b.runner.Placeholders = PlaceholderDir
	b.runner.LogDir = LogDir
	b.opts.Background = true
	return tracker
}

func (b *box) status() provisionspec.Status {
	b.t.Helper()
	status, ok, err := provisionspec.ReadStatus(filepath.Join(b.root, "run/discobox/provision.json"))
	if err != nil || !ok {
		b.t.Fatalf("provision.json: %v, %v", ok, err)
	}
	return status
}

func (b *box) exists(path string) bool {
	_, err := os.Stat(filepath.Join(b.root, path))
	return err == nil
}

// A feature the project turned off is not provisioned, its env leaves every
// login shell, and a feature requiring it is off too; the rest are untouched.
func TestAFeatureTurnedOffIsNeitherProvisionedNorWired(t *testing.T) {
	b := newBox(t)
	ran := filepath.Join(t.TempDir(), "ran")
	b.feature("brew", map[string]string{
		provisionspec.FileName: head + "steps:\n  - name: env\n    env: {path: [/home/linuxbrew/.linuxbrew/bin]}\n  - name: install\n    run: install.sh\n",
		"install.sh":           "#!/bin/sh\necho brew >> " + ran + "\n",
	})
	b.feature("brew-bundle", map[string]string{provisionspec.FileName: head + "requires: [brew]\nsteps: []\n"})
	b.feature("go", map[string]string{provisionspec.FileName: head + "steps:\n  - name: install\n    run: install.sh\n", "install.sh": "#!/bin/sh\necho go >> " + ran + "\n"})
	b.mustApply()
	if !b.exists(EnvProfilePath("brew", "env")) {
		t.Fatal("brew's env was not written while it was on")
	}
	os.Remove(ran)

	b.tracked()
	b.opts.Off = map[string]string{"brew": "turned off in .discobox/settings.json"}
	b.mustApply()
	if data, _ := os.ReadFile(ran); string(data) != "" {
		t.Errorf("ran %q: go was recorded and brew is off", data)
	}
	if b.exists(EnvProfilePath("brew", "env")) {
		t.Error("brew's env is still on every login shell")
	}
	status := b.status()
	if got := status.Features["brew"]; got.State != provisionspec.StateOff || got.Reason != "turned off in .discobox/settings.json" {
		t.Errorf("brew = %+v", got)
	}
	if got := status.Features["brew-bundle"]; got.State != provisionspec.StateOff || !strings.Contains(got.Reason, `requires "brew"`) {
		t.Errorf("brew-bundle = %+v", got)
	}
	if got := status.Features["go"]; got.State != provisionspec.StateReady || got.How != provisionspec.HowSnapshot {
		t.Errorf("go = %+v", got)
	}
	if status.State != provisionspec.StateReady || !status.Ready {
		t.Errorf("status = %+v", status)
	}
}

// node, as a feature that installs its own copy unless the box's is usable.
const nodeSpec = head + `adopt:
  commands: [node]
  version: {command: node, minimum: "22"}
steps:
  - name: env
    env: {vars: {NODE_OPTIONS: --no-warnings}}
  - name: install
    install: true
    run: install.sh
  - name: wire
    run: wire.sh
`

func (b *box) node(ran string) {
	b.feature("node", map[string]string{
		provisionspec.FileName: nodeSpec,
		"install.sh":           "#!/bin/sh\necho install >> " + ran + "\n",
		"wire.sh":              "#!/bin/sh\necho wire >> " + ran + "\n",
	})
}

// executable writes a script under the box's root.
func (b *box) executable(path, script string, mode os.FileMode) {
	b.t.Helper()
	full := filepath.Join(b.root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(script), mode); err != nil {
		b.t.Fatal(err)
	}
	if err := os.Chmod(full, mode); err != nil {
		b.t.Fatal(err)
	}
}

// A usable copy on the box is adopted: its install steps are skipped, its
// wiring still applied, and its version reported.
func TestAUsableCopyIsAdopted(t *testing.T) {
	b := newBox(t)
	ran := filepath.Join(t.TempDir(), "ran")
	b.node(ran)
	b.executable("usr/bin/node", "#!/bin/sh\necho v24.9.0\n", 0o755)
	b.tracked()
	got := results(b.mustApply())
	if got["node/install"] != NotInstalled || got["node/wire"] != Applied || got["node/env"] != Applied {
		t.Errorf("results = %v", got)
	}
	if data, _ := os.ReadFile(ran); string(data) != "wire\n" {
		t.Errorf("ran %q", data)
	}
	if node := b.status().Features["node"]; node.How != provisionspec.HowAdopted || node.Version != "24.9.0" {
		t.Errorf("node = %+v", node)
	}
}

// A copy that is not usable is not adopted: too old, a link into a user's
// home, or one other users cannot run.
func TestACopyThatIsNotUsableIsNotAdopted(t *testing.T) {
	cases := map[string]func(b *box){
		"too old": func(b *box) { b.executable("usr/bin/node", "#!/bin/sh\necho v18.2.0\n", 0o755) },
		"in a user's home": func(b *box) {
			b.executable("home/boxd/.nvm/bin/node", "#!/bin/sh\necho v24.9.0\n", 0o755)
			if err := os.Symlink("../../home/boxd/.nvm/bin/node", filepath.Join(b.root, "usr/local/bin/node")); err != nil {
				b.t.Fatal(err)
			}
		},
		"not every user's": func(b *box) { b.executable("usr/bin/node", "#!/bin/sh\necho v24.9.0\n", 0o750) },
		"absent":           func(*box) {},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			b := newBox(t)
			ran := filepath.Join(t.TempDir(), "ran")
			b.node(ran)
			if err := os.MkdirAll(filepath.Join(b.root, "usr/local/bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			setup(b)
			b.tracked()
			b.mustApply()
			if data, _ := os.ReadFile(ran); string(data) != "install\nwire\n" {
				t.Errorf("ran %q", data)
			}
			if node := b.status().Features["node"]; node.How != provisionspec.HowInstalled {
				t.Errorf("node = %+v", node)
			}
		})
	}
}

// What a box installed is its own: a later run does not adopt it, even once
// it is usable.
func TestWhatWasInstalledIsNotAdoptedLater(t *testing.T) {
	b := newBox(t)
	ran := filepath.Join(t.TempDir(), "ran")
	b.node(ran)
	b.tracked()
	b.mustApply()
	b.executable("usr/bin/node", "#!/bin/sh\necho v24.9.0\n", 0o755)
	if got := results(b.mustApply()); got["node/install"] != Skipped {
		t.Errorf("results = %v", got)
	}
	if node := b.status().Features["node"]; node.How != provisionspec.HowSnapshot {
		t.Errorf("node = %+v", node)
	}
}

// A feature the harness requires cannot be turned off, naming both, unless
// the box's own copy is usable.
func TestAHarnessRequirementOutranksAnOptOut(t *testing.T) {
	b := newBox(t)
	b.node(filepath.Join(t.TempDir(), "ran"))
	b.feature("claude-code", map[string]string{provisionspec.FileName: head + "requires: [node]\nsteps: []\n"})
	tracker := b.tracked()
	b.opts.Harness = []string{"claude-code"}
	b.opts.Off = map[string]string{"node": "turned off in .discobox/settings.json"}
	_, err := b.apply()
	if err == nil || !strings.Contains(err.Error(), `the harness's feature "claude-code" requires "node", which is turned off`) {
		t.Fatalf("error = %v", err)
	}
	if err := tracker.AwaitReady(context.Background()); err == nil {
		t.Error("readiness was reached")
	}
	if status := b.status(); status.State != provisionspec.StateFailed || status.Ready || !strings.Contains(status.Error, "claude-code") {
		t.Errorf("status = %+v", status)
	}

	b.executable("usr/bin/node", "#!/bin/sh\necho v24.9.0\n", 0o755)
	b.tracked()
	b.mustApply()
	if node := b.status().Features["node"]; node.State != provisionspec.StateReady || node.How != provisionspec.HowAdopted {
		t.Errorf("node = %+v", node)
	}
}

// provisionspec.Core is never turned off.
func TestCoreCannotBeTurnedOff(t *testing.T) {
	b := newBox(t)
	b.feature(provisionspec.Core, map[string]string{provisionspec.FileName: head + "steps: []\n"})
	b.opts.Off = map[string]string{provisionspec.Core: "turned off"}
	if _, err := b.apply(); err == nil || !strings.Contains(err.Error(), "core is not a feature") {
		t.Fatalf("error = %v", err)
	}
}

// gate is a script that waits for its release file before it exits.
func gate(release string) string {
	return "#!/bin/sh\nwhile [ ! -e " + release + " ]; do sleep 0.02; done\n"
}

// Readiness comes once core and every feature's wiring are in place; a slow
// feature goes on in the background, its commands standing as placeholders,
// and provision.json says so throughout.
func TestReadinessDoesNotWaitForTheBackground(t *testing.T) {
	b := newBox(t)
	release := filepath.Join(t.TempDir(), "release")
	b.feature(provisionspec.Core, map[string]string{provisionspec.FileName: head + "steps:\n  - name: s\n    file: {path: /etc/core, source: core}\n", "core": "core\n"})
	b.feature("docker", map[string]string{
		provisionspec.FileName: head + "commands: [docker, dockerd]\nsteps:\n  - name: group\n    group: {name: dockremote}\n  - name: install\n    run: install.sh\n",
		"install.sh":           gate(release),
	})
	tracker := b.tracked()
	done := make(chan error, 1)
	go func() {
		_, err := b.apply()
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tracker.AwaitReady(ctx); err != nil {
		t.Fatalf("readiness: %v", err)
	}
	if !b.exists("etc/core") || len(b.tools) == 0 || b.tools[0] != "groupadd --system dockremote" {
		t.Errorf("core or docker's group not in place at readiness: tools %v", b.tools)
	}
	placeholder := b.read(filepath.Join(PlaceholderDir, "dockerd"))
	if !strings.Contains(placeholder, "exec "+ProvisionCommand+" run docker dockerd \"$@\"") {
		t.Errorf("placeholder:\n%s", placeholder)
	}
	if !strings.Contains(b.read(PlaceholderProfile), PlaceholderDir) {
		t.Error("the placeholders are not on a login shell's PATH")
	}
	waitFor(t, func() bool { return b.status().Features["docker"].Step == "install" })
	status := b.status()
	if status.State != provisionspec.StateProvisioning || !status.Ready {
		t.Errorf("status = %+v", status)
	}
	if docker := status.Features["docker"]; docker.State != provisionspec.StateProvisioning || !docker.Async || docker.Log != filepath.Join(LogDir, "docker.log") {
		t.Errorf("docker = %+v", docker)
	}
	if core := status.Features[provisionspec.Core]; core.State != provisionspec.StateReady || core.Async {
		t.Errorf("core = %+v", core)
	}

	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if b.exists(filepath.Join(PlaceholderDir, "dockerd")) {
		t.Error("the placeholder outlived its feature")
	}
	if status := b.status(); status.State != provisionspec.StateReady || status.Features["docker"].How != provisionspec.HowInstalled {
		t.Errorf("status = %+v", status)
	}
}

// A feature the harness requires blocks readiness, its async mark
// notwithstanding, and so does what it requires.
func TestTheHarnessesRequirementsBlockReadiness(t *testing.T) {
	b := newBox(t)
	release := filepath.Join(t.TempDir(), "release")
	b.feature("node", map[string]string{provisionspec.FileName: head + "async: true\nsteps:\n  - name: install\n    async: true\n    run: install.sh\n", "install.sh": gate(release)})
	b.feature("claude-code", map[string]string{provisionspec.FileName: head + "requires: [node]\nsteps: []\n"})
	tracker := b.tracked()
	b.opts.Harness = []string{"claude-code"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.apply()
	}()
	defer func() { <-done }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := tracker.AwaitReady(ctx); err == nil {
		t.Fatal("ready before what the harness requires")
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tracker.AwaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if node := tracker.Status().Features["node"]; node.Async {
		t.Errorf("node = %+v", node)
	}
}

// A failed background feature fails itself and what requires it, and not
// readiness; a failed blocking step fails readiness.
func TestFailures(t *testing.T) {
	b := newBox(t)
	b.feature(provisionspec.Core, map[string]string{provisionspec.FileName: head + "steps: []\n"})
	b.feature("browser", map[string]string{provisionspec.FileName: head + "steps:\n  - name: install\n    run: install.sh\n", "install.sh": "#!/bin/sh\necho no network >&2\nexit 1\n"})
	b.feature("desktop", map[string]string{provisionspec.FileName: head + "requires: [browser]\nsteps: []\n"})
	tracker := b.tracked()
	if _, err := b.apply(); err == nil {
		t.Fatal("no error")
	}
	if err := tracker.AwaitReady(context.Background()); err != nil {
		t.Errorf("readiness: %v", err)
	}
	status := b.status()
	if browser := status.Features["browser"]; browser.State != provisionspec.StateFailed || !strings.Contains(browser.Error, "exit status 1") {
		t.Errorf("browser = %+v", browser)
	}
	if !strings.Contains(b.read(filepath.Join(LogDir, "browser.log")), "no network") {
		t.Error("the step's output is not in the feature's log")
	}
	if desktop := status.Features["desktop"]; desktop.State != provisionspec.StateFailed || !strings.Contains(desktop.Error, `requires "browser"`) {
		t.Errorf("desktop = %+v", desktop)
	}
	if status.State != provisionspec.StateFailed || !status.Ready {
		t.Errorf("status = %+v", status)
	}

	b.feature(provisionspec.Core, map[string]string{provisionspec.FileName: head + "steps:\n  - name: s\n    run: s.sh\n", "s.sh": "#!/bin/sh\nexit 2\n"})
	tracker = b.tracked()
	b.apply()
	if err := tracker.AwaitReady(context.Background()); err == nil || !strings.Contains(err.Error(), `feature "core"`) || !strings.Contains(err.Error(), "(log: "+filepath.Join(LogDir, "core.log")+")") {
		t.Errorf("readiness: %v", err)
	}
}

// Steps using apt or dpkg never run at once, however many features
// provision in parallel.
func TestAptStepsTakeOneLock(t *testing.T) {
	b := newBox(t)
	lock := filepath.Join(t.TempDir(), "held")
	script := "#!/bin/sh\nmkdir " + lock + " || exit 9\nsleep 0.1\nrmdir " + lock + "\n"
	for _, name := range []string{"a", "b", "c"} {
		b.feature(name, map[string]string{provisionspec.FileName: head + "steps:\n  - name: s\n    apt: true\n    run: s.sh\n", "s.sh": script})
	}
	b.tracked()
	b.mustApply()
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// What core requires is in place before core runs and before readiness, its
// async mark notwithstanding, as what the harness requires is.
func TestCoresRequirementsBlockReadiness(t *testing.T) {
	b := newBox(t)
	release := filepath.Join(t.TempDir(), "release")
	b.feature("trust", map[string]string{provisionspec.FileName: head + "async: true\nsteps:\n  - name: s\n    run: s.sh\n", "s.sh": gate(release)})
	b.feature(provisionspec.Core, map[string]string{provisionspec.FileName: head + "requires: [trust]\nsteps: []\n"})
	tracker := b.tracked()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.apply()
	}()
	defer func() { <-done }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := tracker.AwaitReady(ctx); err == nil {
		t.Fatal("ready before what core requires")
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tracker.AwaitReady(ctx); err != nil {
		t.Fatal(err)
	}
}

// A step marked async in a feature that blocks defers itself and every step
// after it, so the steps still run in the order written.
func TestAnAsyncStepDefersTheStepsAfterIt(t *testing.T) {
	b := newBox(t)
	order := filepath.Join(t.TempDir(), "order")
	release := filepath.Join(t.TempDir(), "release")
	b.feature(provisionspec.Core, map[string]string{
		provisionspec.FileName: head + "steps:\n  - name: first\n    run: first.sh\n  - name: slow\n    async: true\n    run: slow.sh\n  - name: last\n    run: last.sh\n",
		"first.sh":             "#!/bin/sh\necho first >> " + order + "\n",
		"slow.sh":              gate(release) + "echo slow >> " + order + "\n",
		"last.sh":              "#!/bin/sh\necho last >> " + order + "\n",
	})
	tracker := b.tracked()
	done := make(chan error, 1)
	go func() {
		_, err := b.apply()
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tracker.AwaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if ran, _ := os.ReadFile(order); string(ran) != "first\n" {
		t.Errorf("ran %q by readiness, want only the step before the async one", ran)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if ran, _ := os.ReadFile(order); string(ran) != "first\nslow\nlast\n" {
		t.Errorf("ran %q", ran)
	}
}

// Features provisioning in parallel that merge into one file each keep their
// keys: a merge's read and write are one step under the merge lock.
func TestParallelMergesIntoOneFileKeepEveryKey(t *testing.T) {
	b := newBox(t)
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for _, name := range names {
		b.feature("merge-"+name, map[string]string{
			provisionspec.FileName: head + "steps:\n  - name: s\n    file: {path: /etc/shared.json, source: patch.json, merge: json}\n",
			"patch.json":           `{"` + name + `": true}`,
		})
	}
	b.tracked()
	b.mustApply()
	got := b.read("etc/shared.json")
	for _, name := range names {
		if !strings.Contains(got, `"`+name+`": true`) {
			t.Fatalf("%q lost from %s", name, got)
		}
	}
}
