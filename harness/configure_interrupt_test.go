package harness_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/harness"
)

// Ctrl-C while a configure script holds the terminal is its person leaving
// setup, the same as declining another attempt, so every built-in script
// turns the interrupt into the canceled status rather than dying of it with
// 130 — which the console would report as a failure. The script's own trap is
// run, on a real SIGINT, rather than read.
func TestInterruptingAConfigureScriptCancelsSetup(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no POSIX shell to run the configure scripts with: %v", err)
	}
	scripts, err := filepath.Glob("*/configure.sh")
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, path := range scripts {
		t.Run(filepath.Dir(path), func(t *testing.T) {
			script, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var trap string
			for line := range strings.SplitSeq(string(script), "\n") {
				if strings.HasPrefix(line, "trap ") && strings.HasSuffix(line, " INT") {
					trap = line
					break
				}
			}
			if trap == "" {
				t.Fatalf("%s sets no INT trap, so Ctrl-C at its prompts fails setup instead of canceling it", path)
			}
			// The prompt stands in for the script waiting on its person.
			//nolint:gosec // G204: the trap line of a script in this repository, under test.
			cmd := exec.CommandContext(t.Context(), shell, "-eu", "-c", trap+"\nkill -INT $$\nsleep 5\n")
			var stderr strings.Builder
			cmd.Stderr = &stderr
			cmd.WaitDelay = time.Second
			err = cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != harness.ConfigureCanceledExitCode {
				t.Fatalf("interrupted %s ended with %v (stderr %q), want the canceled status %d", path, err, stderr.String(), harness.ConfigureCanceledExitCode)
			}
		})
		ran++
	}
	if ran == 0 {
		t.Fatal("found no configure scripts to interrupt")
	}
}
