// Package launchertest runs a harness image's launch.sh the way a sandbox
// terminal does — as a POSIX shell script handed the words a login shell split
// a typed command into — and reports the argv its agent was finally given.
//
// It exists because the harness-run convention (ADR 0086 §3) is a contract
// between the runtime and a shell script, so the only test that proves a
// launcher keeps it is one that runs the script.
package launchertest

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
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
	launcher := filepath.Join(dir, "launch.sh")
	if err := os.WriteFile(launcher, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	// The stub prints one argument per line, which is unambiguous for the
	// prompts a launcher test asks about: what it is proving is that words the
	// shell split arrive as one argument, so an argument containing a newline
	// would be testing the printf rather than the launcher.
	stub := filepath.Join(dir, agent)
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\"; done\n"), 0o755); err != nil { //nolint:gosec // The stub is the agent the launcher execs; it has to be executable.
		t.Fatal(err)
	}
	sudo := filepath.Join(dir, "sudo")
	if err := os.WriteFile(sudo, []byte("#!/bin/sh\n[ \"$1\" = -n ] && shift\nexec \"$@\"\n"), 0o755); err != nil { //nolint:gosec // The stub stands in for sudo; it has to be executable.
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), shell, append([]string{launcher}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+dir,
	)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("launch.sh %v: %v", args, err)
	}
	trimmed := strings.TrimSuffix(string(out), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}
