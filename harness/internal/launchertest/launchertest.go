// Package launchertest runs a harness image's launch.sh the way a sandbox
// terminal does — as a POSIX shell script handed the words a login shell split
// a typed command into — and reports the argv its agent was finally given.
//
// It exists because the harness-run convention (ADR 0086 §3) is a contract
// between the runtime and a shell script, so the only test that proves a
// launcher keeps it is one that runs the script.
package launchertest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sourceDataPath is the mount the launchers look for before wiring up
// source-scoped memory. A test points it at a path that does not exist, so the
// run is hermetic: no directory is created, nothing is mounted, and the script
// reaches its `exec <agent> "$@"` the way a source-less sandbox does.
const sourceDataPath = "/.discobox/data-per-source/primary"

// RunLauncher runs ./launch.sh with args and returns the argv the named agent
// was executed with. The agent is stubbed, so nothing installed on the machine
// running the test is invoked.
func RunLauncher(t *testing.T, agent string, args []string) []string {
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
	launcher := filepath.Join(dir, "launch.sh")
	if err := os.WriteFile(launcher, []byte(strings.ReplaceAll(string(script), sourceDataPath, filepath.Join(dir, "no-source-data"))), 0o600); err != nil {
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
