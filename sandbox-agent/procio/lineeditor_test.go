//go:build !windows

package procio

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// readFor collects a process's output for a while, so a test can look at what
// a terminal would have shown.
func readFor(t *testing.T, r io.Reader, d time.Duration) string {
	t.Helper()
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(&out, r)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
	return out.String()
}

func startShell(t *testing.T) *Process {
	t.Helper()
	proc, err := Start(Options{
		Command: []string{"/bin/bash", "-i"},
		Env:     []string{"PS1=PROMPT$ ", "TERM=dumb", "HOME=" + t.TempDir()},
		TTY:     true,
		Winsize: &pty.Winsize{Rows: 24, Cols: 80},
	})
	if err != nil {
		if _, statErr := exec.LookPath("/bin/bash"); statErr != nil {
			t.Skip("no bash on this machine")
		}
		t.Fatalf("start shell: %v", err)
	}
	t.Cleanup(proc.Close)
	return proc
}

// Typing into a shell that has taken the terminal shows the command once. The
// kernel echoes what arrives while ECHO is still on, and the line editor
// displays the same bytes again when it reads them, so writing too early puts
// the command on screen twice.
func TestTypedInputIsShownOnce(t *testing.T) {
	proc := startShell(t)

	if !proc.WaitForLineEditor() {
		t.Skip("this shell never took the terminal")
	}
	if _, err := proc.WriteInput([]byte("echo marker-one-two\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	shown := readFor(t, proc.Stdout(), 2*time.Second)
	if n := strings.Count(shown, "marker-one-two"); n != 2 {
		// Twice: the line editor's display of what was typed, and the output of
		// running it. A third is the kernel having echoed it as well.
		t.Fatalf("the command appears %d times, want the editor's copy and the output:\n%s", n, shown)
	}
}

// The wait is what makes that true: without it the same write lands while the
// kernel is still echoing.
func TestWithoutWaitingTheInputIsEchoedTwice(t *testing.T) {
	proc := startShell(t)

	// No wait: this is what the shim used to do.
	if _, err := proc.WriteInput([]byte("echo marker-one-two\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	shown := readFor(t, proc.Stdout(), 2*time.Second)
	if n := strings.Count(shown, "marker-one-two"); n < 3 {
		t.Skipf("this shell did not double-echo (%d occurrences), so there is nothing for the wait to fix:\n%s", n, shown)
	}
}

// A process with no TTY has no line discipline to wait for.
func TestWaitForLineEditorOnAPipeProcessReturnsAtOnce(t *testing.T) {
	proc, err := Start(Options{Command: []string{"/bin/cat"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(proc.Close)

	start := time.Now()
	if !proc.WaitForLineEditor() {
		t.Fatal("a pipe process has nothing to wait for and should say so")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("waited %s for a process with no TTY", waited)
	}
}
