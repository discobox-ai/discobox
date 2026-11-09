// Package launchertest runs a harness image's launch.sh the way a sandbox
// terminal does — as a POSIX shell script handed the words a login shell split
// a typed command into — and reports the argv its agent was finally given.
//
// It exists because the harness-run convention (ADR 0086 §3) is a contract
// between the runtime and a shell script, so the only test that proves a
// launcher keeps it is one that runs the script.
package launchertest

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// SourceDataPath is the mount the launchers look for before wiring up
// source-scoped memory.
const SourceDataPath = "/.discobox/data-per-source/primary"

// RunLauncher runs ./launch.sh with args and returns the argv the named agent
// was executed with. The agent is stubbed, so nothing installed on the machine
// running the test is invoked.
//
// paths maps absolute paths the script names to the test directories standing
// in for them. SourceDataPath is pointed at a path that does not exist unless
// paths says otherwise, so by default the script takes the branch a
// source-less sandbox does. `sudo` is stubbed to run its command as the test's
// own user, which is enough for the directories and files a launcher makes; a
// mount it attempts fails, and the launcher falls back the way it would in a
// sandbox without the grant.
func RunLauncher(t *testing.T, agent string, paths map[string]string, args []string) []string {
	t.Helper()
	out, status := Run(t, agent, nil, paths, args)
	if status != 0 {
		t.Fatalf("launch.sh %v: exit status %d", args, status)
	}
	trimmed := strings.TrimSuffix(out, "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// Run runs ./launch.sh as RunLauncher does and returns its standard output and
// exit status, so a test can drive a launcher whose agent fails. stubs maps
// command names to POSIX shell scripts put ahead of PATH, replacing the agent
// stub when one is named for the agent. A stub keeps any state it needs under
// $HOME, the test directory the launcher runs with.
func Run(t *testing.T, agent string, stubs, paths map[string]string, args []string) (string, int) {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no POSIX shell to run the launcher with: %v", err)
	}
	script, err := os.ReadFile("launch.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rewrites := map[string]string{SourceDataPath: filepath.Join(dir, "no-source-data")}
	maps.Copy(rewrites, paths)
	rewritten := string(script)
	for from, to := range rewrites {
		rewritten = strings.ReplaceAll(rewritten, from, to)
	}
	var captureEnv []string
	if agent == "codex" {
		// Run the real system-trust writer against an isolated system layer.
		trust, err := os.ReadFile("trust.cjs")
		if err != nil {
			t.Fatal(err)
		}
		baseline, err := filepath.Abs("system-config.toml")
		if err != nil {
			t.Fatal(err)
		}
		settings, err := filepath.Abs("../capture/settings.cjs")
		if err != nil {
			t.Fatal(err)
		}
		writer := filepath.Join(dir, "trust.cjs")
		config := filepath.Join(dir, "config.toml")
		replace := strings.NewReplacer(
			"'/usr/local/libexec/discobox/harness-capture/settings.cjs'", strconv.Quote(filepath.ToSlash(settings)),
			"'/usr/local/libexec/discobox/codex-system-config.toml'", strconv.Quote(filepath.ToSlash(baseline)),
			"'/etc/codex/config.toml'", strconv.Quote(filepath.ToSlash(config)),
		)
		if err := os.WriteFile(writer, []byte(replace.Replace(string(trust))), 0o600); err != nil {
			t.Fatal(err)
		}
		// These paths are shell data, including Windows backslashes.
		rewritten = strings.ReplaceAll(rewritten, "sudo -n flock /run/lock/discobox-codex-trust.lock", "")
		rewritten = strings.ReplaceAll(rewritten, "/usr/local/libexec/discobox/codex-trust.cjs", "\"$DISCOBOX_TEST_CODEX_TRUST\"")
		captureEnv = []string{"DISCOBOX_TEST_CODEX_TRUST=" + writer}
	}
	if agent == "claude" {
		captureDir, err := filepath.Abs("../capture")
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := filepath.Abs("image.json")
		if err != nil {
			t.Fatal(err)
		}
		// Paths are data, not shell source. In particular a Windows path's
		// backslashes must survive the POSIX shell that drives this fixture.
		captureEnv = []string{"DISCOBOX_TEST_CAPTURE_RUNTIME=" + filepath.Join(captureDir, "runtime.cjs"), "DISCOBOX_TEST_CAPTURE_METADATA=" + metadata}
		rewritten = strings.ReplaceAll(rewritten, "/usr/local/libexec/discobox/claude-code-capture init", `node "$DISCOBOX_TEST_CAPTURE_RUNTIME" claude-code "$DISCOBOX_TEST_CAPTURE_METADATA" init`)
	}
	launcher := filepath.Join(dir, "launch.sh")
	if err := os.WriteFile(launcher, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	// The stub prints one argument per line, which is unambiguous for the
	// prompts a launcher test asks about: what it is proving is that words the
	// shell split arrive as one argument, so an argument containing a newline
	// would be testing the printf rather than the launcher.
	executables := map[string]string{
		agent:  "#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\"; done\n",
		"sudo": "#!/bin/sh\n[ \"$1\" = -n ] && shift\nexec \"$@\"\n",
	}
	maps.Copy(executables, stubs)
	for name, body := range executables {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil { //nolint:gosec // A stub stands in for a command the launcher runs; it has to be executable.
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), shell, append([]string{launcher}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+dir,
	)
	cmd.Env = append(cmd.Env, captureEnv...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		t.Fatalf("launch.sh %v: %v", args, err)
	}
	return string(out), 0
}
