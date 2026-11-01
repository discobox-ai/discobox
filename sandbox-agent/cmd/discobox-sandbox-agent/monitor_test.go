package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/monitors"
)

const repoMonitor = `#!/bin/sh
#---
# name: Repository
# description: Watches a repository
# interval: 60s
# options:
# - {name: repo, required: true, description: owner/name}
# - {name: pr, description: PR numbers}
#---
echo hi
`

func testMonitorEnv(t *testing.T) monitorEnv {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	env := monitorEnv{
		dirs:       monitors.HomeDirs(home),
		home:       home,
		builtinDir: filepath.Join(root, "builtin"),
		sourceRoot: filepath.Join(root, "src"),
		now:        func() time.Time { return now },
		ctx:        context.Background(),
	}
	write := func(dir, name, body string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(env.builtinDir, "10-github-repo.sh", repoMonitor)
	write(filepath.Join(env.sourceRoot, monitors.DirName), "hello.sh", "#!/bin/sh\n#---\n# description: says hello\n#---\necho hello\n")
	return env
}

func runMonitorCmd(t *testing.T, env monitorEnv, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := monitor(env, args, &out)
	return out.String(), err
}

func TestMonitorListShowsBothDirectories(t *testing.T) {
	env := testMonitorEnv(t)
	out, err := runMonitorCmd(t, env, "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"github-repo", "image", "every 1m0s", "repo*,pr", "hello", "repo", "streaming"} {
		if !strings.Contains(out, want) {
			t.Errorf("list does not say %q:\n%s", want, out)
		}
	}
}

func TestMonitorEnableRecordsTheInstanceAndRefusesWhatItCannotRun(t *testing.T) {
	env := testMonitorEnv(t)
	out, err := runMonitorCmd(t, env, "enable", "github-repo", "--name", "pr-412", "-o", "repo=a/b,pr=412")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "enabled pr-412") || !strings.Contains(out, "pop --wait") {
		t.Fatalf("enable said:\n%s", out)
	}
	inst, err := monitors.ReadInstance(env.dirs, "pr-412")
	if err != nil {
		t.Fatal(err)
	}
	// Every monitor runs in the repository, the image's included.
	if inst.Monitor != "github-repo" || inst.Options["pr"] != "412" || inst.Workdir != env.sourceRoot || !inst.Builtin || inst.ID == "" {
		t.Fatalf("instance = %+v", inst)
	}
	// The same again changes nothing; other options under the name are refused.
	if out, err := runMonitorCmd(t, env, "enable", "github-repo", "--name", "pr-412", "-o", "pr=412", "-o", "repo=a/b"); err != nil || !strings.Contains(out, "already enabled") {
		t.Fatalf("same enable = %q, %v", out, err)
	}
	if _, err := runMonitorCmd(t, env, "enable", "github-repo", "--name", "pr-412", "-o", "repo=a/b,pr=9"); err == nil || !strings.Contains(err.Error(), "--name") || !strings.Contains(err.Error(), "--update") {
		t.Fatalf("enable over another instance = %v", err)
	}
	// --update gives the instance the new options, and keeps it.
	out, err = runMonitorCmd(t, env, "enable", "github-repo", "--name", "pr-412", "--update", "-o", "repo=a/b,pr=412,413")
	if err != nil || !strings.Contains(out, "updated pr-412") {
		t.Fatalf("enable --update = %q, %v", out, err)
	}
	if updated, _ := monitors.ReadInstance(env.dirs, "pr-412"); updated.ID != inst.ID || updated.Options["pr"] != "412,413" {
		t.Fatalf("updated instance = %+v, was %+v", updated, inst)
	}
	if _, err := runMonitorCmd(t, env, "enable", "hello", "--name", "pr-412", "--update"); err == nil || !strings.Contains(err.Error(), "not its monitor") {
		t.Fatalf("enable --update of another monitor = %v", err)
	}
	if _, err := runMonitorCmd(t, env, "enable", "github-repo", "-o", "issues=1"); err == nil || !strings.Contains(err.Error(), "requires repo") {
		t.Fatalf("enable with a wrong option = %v", err)
	}
	if _, err := runMonitorCmd(t, env, "enable", "nope"); err == nil || !strings.Contains(err.Error(), "github-repo, hello") {
		t.Fatalf("enable of an unknown monitor = %v", err)
	}
	// Without a repository, a monitor runs in home.
	env.sourceRoot = ""
	if _, err := runMonitorCmd(t, env, "enable", "github-repo", "--name", "nosrc", "-o", "repo=a/b"); err != nil {
		t.Fatal(err)
	}
	if inst, _ := monitors.ReadInstance(env.dirs, "nosrc"); inst.Workdir != env.home {
		t.Fatalf("a monitor with no repository runs in %q", inst.Workdir)
	}
}

func TestMonitorStatusListsInstancesAndPendingEvents(t *testing.T) {
	env := testMonitorEnv(t)
	if _, err := runMonitorCmd(t, env, "enable", "github-repo", "-o", "repo=a/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.dirs.Queue().Add(monitors.Event{Instance: "github-repo", Title: "t"}); err != nil {
		t.Fatal(err)
	}
	out, err := runMonitorCmd(t, env, "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"github-repo", "starting", "repo=a/b", "1 events pending"} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not say %q:\n%s", want, out)
		}
	}
	if _, err := runMonitorCmd(t, env, "status", "nope"); !errors.Is(err, monitors.ErrNotEnabled) {
		t.Fatalf("status of an unknown instance = %v", err)
	}
}

func TestMonitorEmitPopAndRead(t *testing.T) {
	env := testMonitorEnv(t)
	body := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(body, []byte("the whole log\nline two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runMonitorCmd(t, env, "emit", "--title", "tests done", "--kind", "build.done", "--body-file", body); err != nil || !strings.Contains(out, "queued e1") {
		t.Fatalf("emit = %q, %v", out, err)
	}
	out, err := runMonitorCmd(t, env, "peek")
	if err != nil || !strings.Contains(out, "e1") || !strings.Contains(out, "1 pending, none taken") {
		t.Fatalf("peek = %q, %v", out, err)
	}
	out, err = runMonitorCmd(t, env, "pop")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"e1", "emit", "build.done", "tests done", "body 23 B", "1 popped, 0 pending"} {
		if !strings.Contains(out, want) {
			t.Errorf("pop does not say %q:\n%s", want, out)
		}
	}
	if out, _ := runMonitorCmd(t, env, "pop"); !strings.Contains(out, "no events pending") {
		t.Fatalf("second pop = %q", out)
	}
	out, err = runMonitorCmd(t, env, "read", "e1")
	if err != nil || !strings.Contains(out, "line two") || !strings.Contains(out, "kind:     build.done") {
		t.Fatalf("read = %q, %v", out, err)
	}
}

func TestMonitorPopJSONCarriesHeadersNotBodies(t *testing.T) {
	env := testMonitorEnv(t)
	if _, err := env.dirs.Queue().Add(monitors.Event{Instance: "a", Title: "t", Body: "secret detail"}); err != nil {
		t.Fatal(err)
	}
	out, err := runMonitorCmd(t, env, "pop", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var head header
	if err := json.Unmarshal([]byte(out), &head); err != nil {
		t.Fatalf("pop --json = %q: %v", out, err)
	}
	if head.ID != "e1" || head.BodyBytes != len("secret detail") || strings.Contains(out, "secret detail") {
		t.Fatalf("header = %+v from %q", head, out)
	}
}

func TestMonitorPopWaitReturnsWhenAnEventArrivesOrItsTimeEnds(t *testing.T) {
	env := testMonitorEnv(t)
	start := time.Now()
	out, err := runMonitorCmd(t, env, "pop", "--wait=200ms")
	if err != nil || !strings.Contains(out, "no events in 200ms") || time.Since(start) < 200*time.Millisecond {
		t.Fatalf("pop --wait=200ms = %q, %v after %v", out, err, time.Since(start))
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = env.dirs.Queue().Add(monitors.Event{Instance: "a", Title: "arrived"})
	}()
	out, err = runMonitorCmd(t, env, "pop", "--wait")
	if err != nil || !strings.Contains(out, "arrived") {
		t.Fatalf("pop --wait = %q, %v", out, err)
	}
}

func TestMonitorDisableSaysWhatIsStillPending(t *testing.T) {
	env := testMonitorEnv(t)
	if _, err := runMonitorCmd(t, env, "enable", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.dirs.Queue().Add(monitors.Event{Instance: "hello", Title: "t"}); err != nil {
		t.Fatal(err)
	}
	out, err := runMonitorCmd(t, env, "disable", "hello")
	if err != nil || !strings.Contains(out, "1 of its events are still pending") {
		t.Fatalf("disable = %q, %v", out, err)
	}
	if _, err := runMonitorCmd(t, env, "disable", "hello"); !errors.Is(err, monitors.ErrNotEnabled) {
		t.Fatalf("disable again = %v", err)
	}
}

func TestMonitorTestPrintsEventsAndEnablesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs a shell script by its path, which Windows cannot")
	}
	env := testMonitorEnv(t)
	out, err := runMonitorCmd(t, env, "test", "hello")
	if err != nil {
		t.Fatal(err)
	}
	var ev monitors.Event
	if err := json.Unmarshal([]byte(out), &ev); err != nil || ev.Title != "hello" {
		t.Fatalf("test printed %q: %v", out, err)
	}
	if instances, _ := monitors.Enabled(env.dirs); len(instances) != 0 {
		t.Fatalf("test enabled %v", instances)
	}
	if n, _ := env.dirs.Queue().Count(monitors.Filter{}); n != 0 {
		t.Fatalf("test queued %d events", n)
	}
}

func TestMonitorEnablesAScriptByItsPath(t *testing.T) {
	env := testMonitorEnv(t)
	// A skill's own monitor: beside its SKILL.md, reached through a link as a
	// skills source's skill is.
	checkout := t.TempDir()
	dir := filepath.Join(checkout, "lead", "monitors")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "10-disk.sh"), []byte("#!/bin/sh\n#---\n# description: free disk\n# interval: 5m\n# options:\n# - {name: min, required: true}\n#---\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	skills := filepath.Join(env.home, ".claude", "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(checkout, "lead"), filepath.Join(skills, "lead")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(skills, "lead", "monitors", "10-disk.sh")

	// It is not discovered: list shows the declaration directories only.
	if out, err := runMonitorCmd(t, env, "list"); err != nil || strings.Contains(out, "disk") {
		t.Fatalf("list = %q, %v", out, err)
	}
	if _, err := runMonitorCmd(t, env, "enable", path); err == nil || !strings.Contains(err.Error(), "requires min") {
		t.Fatalf("enable by path without its required option = %v", err)
	}
	out, err := runMonitorCmd(t, env, "enable", path, "-o", "min=10G")
	if err != nil || !strings.Contains(out, "enabled disk") {
		t.Fatalf("enable by path = %q, %v", out, err)
	}
	// The link is kept, so an edit in the checkout reaches the instance.
	if inst, _ := monitors.ReadInstance(env.dirs, "disk"); inst.Path != path || inst.Monitor != "disk" || inst.Builtin || inst.Workdir != env.sourceRoot {
		t.Fatalf("instance = %+v", inst)
	}
	if out, err := runMonitorCmd(t, env, "enable", path, "--update", "-o", "min=20G"); err != nil || !strings.Contains(out, "updated disk") {
		t.Fatalf("enable --update by path = %q, %v", out, err)
	}
	if out, err := runMonitorCmd(t, env, "show", path); err != nil || !strings.Contains(out, "from:  path") || !strings.Contains(out, "enabled as disk") {
		t.Fatalf("show by path = %q, %v", out, err)
	}
	if _, err := runMonitorCmd(t, env, "enable", filepath.Join(skills, "lead", "monitors")); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("enable of a directory = %v", err)
	}
	if _, err := runMonitorCmd(t, env, "enable", "./nope.sh"); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("enable of a missing path = %v", err)
	}
}

func TestMonitorTestSinceGivesTheFirstRunABaselineInThePast(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs a shell script by its path, which Windows cannot")
	}
	env := testMonitorEnv(t)
	dir := filepath.Join(env.sourceRoot, monitors.DirName)
	if err := os.WriteFile(filepath.Join(dir, "recent.sh"), []byte("#!/bin/sh\n#---\n# since: true\n#---\necho \"from $DISCOBOX_MONITOR_SINCE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runMonitorCmd(t, env, "test", "recent", "--since", "2h")
	if err != nil {
		t.Fatal(err)
	}
	var ev monitors.Event
	if err := json.Unmarshal([]byte(out), &ev); err != nil || ev.Title != "from "+now.Add(-2*time.Hour).UTC().Format(time.RFC3339) {
		t.Fatalf("test --since printed %q: %v", out, err)
	}
	// A monitor that does not declare it would print nothing, which reads as
	// nothing having happened: it is refused.
	if _, err := runMonitorCmd(t, env, "test", "hello", "--since", "2h"); err == nil || !strings.Contains(err.Error(), "since: true") {
		t.Fatalf("test --since of a monitor without since = %v", err)
	}
}

func TestMonitorUsageErrors(t *testing.T) {
	env := testMonitorEnv(t)
	for _, args := range [][]string{{}, {"bogus"}, {"enable"}, {"read"}, {"pop", "extra"}, {"emit"}} {
		var u usageError
		if _, err := runMonitorCmd(t, env, args...); !errors.As(err, &u) {
			t.Errorf("%v = %v, want a usage error", args, err)
		}
	}
}

func TestMonitorPrintsNoControlCharacterAThirdPartyWrote(t *testing.T) {
	env := testMonitorEnv(t)
	hostile := "\x1b]52;c;cGF3bmVk\x07title\x1b[2J"
	if _, err := env.dirs.Queue().Add(monitors.Event{Instance: "a", Kind: "k\x1b", Title: hostile, URL: "https://x/\x9b", Body: "line one\n\x1b]7501;idle\x07line two\ttab"}); err != nil {
		t.Fatal(err)
	}
	peek, err := runMonitorCmd(t, env, "peek")
	if err != nil {
		t.Fatal(err)
	}
	read, err := runMonitorCmd(t, env, "read", "e1")
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"peek": peek, "read": read} {
		for _, r := range out {
			if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r < 0xa0) {
				t.Fatalf("%s printed control character %#x:\n%q", name, r, out)
			}
		}
	}
	if !strings.Contains(read, `\x1b]52;c;cGF3bmVk\x07title`) || !strings.Contains(read, "line one\n") || !strings.Contains(read, "line two\ttab") {
		t.Fatalf("read did not show the escapes, or lost the body's lines:\n%s", read)
	}
}
