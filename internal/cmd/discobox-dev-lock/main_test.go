package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/internal/filelock"
)

// selfCommand is this test binary run as a no-op child: a portable "true" that
// exists on every platform the dev loop runs on.
func selfCommand() []string {
	return []string{os.Args[0], "-test.run=^$"}
}

func TestRunHoldsTheLockAndRunsTheCommand(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := run(append([]string{"server"}, selfCommand()...), io.Discard, io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Released when the command finished, so the next loop can start.
	lock, err := filelock.TryAcquire(filepath.Join(lockDir, "server.lock"))
	if err != nil {
		t.Fatalf("acquire after run: %v", err)
	}
	_ = lock.Release()
}

// TestRunRefusesASecondLoop is the regression this command exists for: two
// `task dev` in one checkout displace each other's server forever, and neither
// loop can tell from the inside.
func TestRunRefusesASecondLoop(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatalf("create lock dir: %v", err)
	}
	held, err := filelock.TryAcquire(filepath.Join(lockDir, "server.lock"))
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer held.Release()

	err = run(append([]string{"server"}, selfCommand()...), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("second loop started, want a refusal")
	}
	if !strings.Contains(err.Error(), "already running in this checkout") {
		t.Fatalf("refusal says %q, want it to name the running loop", err)
	}
}

// TestRunLocksPerName keeps the server loop and the image watcher independent.
func TestRunLocksPerName(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatalf("create lock dir: %v", err)
	}
	held, err := filelock.TryAcquire(filepath.Join(lockDir, "server.lock"))
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer held.Release()

	if err := run(append([]string{"docker-image-watch"}, selfCommand()...), io.Discard, io.Discard); err != nil {
		t.Fatalf("run under a different name: %v", err)
	}
}

// TestRunReportsTheCommandExitCode keeps a failed loop a failed task.
func TestRunReportsTheCommandExitCode(t *testing.T) {
	t.Chdir(t.TempDir())
	// Selects the failing helper below in the child, which inherits it. The
	// parent's own run of that test sees the variable unset and skips.
	t.Setenv("DEV_LOCK_HELPER", "1")
	err := run([]string{"server", os.Args[0], "-test.run=^TestHelperFails$"}, io.Discard, io.Discard)
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("run err = %v, want an exec.ExitError", err)
	}
	if exit.ExitCode() == 0 {
		t.Fatal("failing command reported exit code 0")
	}
}

// TestHelperFails is the failing child of TestRunReportsTheCommandExitCode. It
// only fails when run as that child, which is the only time it is selected.
func TestHelperFails(t *testing.T) {
	if os.Getenv("DEV_LOCK_HELPER") == "" {
		t.Skip("helper process only")
	}
	t.Fatal("failing on purpose")
}

func TestRunNeedsANameAndACommand(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := run([]string{"server"}, io.Discard, io.Discard); err == nil {
		t.Fatal("run without a command succeeded, want a usage error")
	}
}
