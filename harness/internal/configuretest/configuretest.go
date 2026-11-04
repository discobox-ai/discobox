// Package configuretest runs one function of a harness's configure.sh on its
// own, the way the script calls it, and reports how it ended.
//
// A configure command's exit status is a contract with the control plane and
// the CLI (harness.ConfigureCanceledExitCode separates a person's choice to
// stop from a failure), so the test that proves a script keeps it is one that
// runs the script's own code rather than one that reads it.
package configuretest

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// RunFunction runs the shell function name, defined in ./configure.sh, with
// stdin as its input, and returns its exit status and what it wrote to stderr.
// Only the function's own definition is run, not the script around it, so it
// must not lean on anything the script sets up before calling it.
func RunFunction(t *testing.T, name, stdin string) (int, string) {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no POSIX shell to run the configure script with: %v", err)
	}
	script, err := os.ReadFile("configure.sh")
	if err != nil {
		t.Fatal(err)
	}
	body := Function(t, string(script), name)
	//nolint:gosec // G204: a function of a script in this repository, under test.
	cmd := exec.CommandContext(t.Context(), shell, "-eu", "-c", body+"\n"+name)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String()
	case errors.As(err, &exit):
		return exit.ExitCode(), stderr.String()
	default:
		t.Fatalf("run %s: %v", name, err)
		return 0, ""
	}
}

// Function is the definition of the shell function name in script, from its
// opening line to the closing brace at the start of a line.
func Function(t *testing.T, script, name string) string {
	t.Helper()
	start := strings.Index(script, "\n"+name+"() {\n")
	if start < 0 {
		t.Fatalf("configure script defines no %s function", name)
	}
	body := script[start+1:]
	end := strings.Index(body, "\n}\n")
	if end < 0 {
		t.Fatalf("configure script's %s function has no closing brace", name)
	}
	return body[:end+len("\n}")]
}
