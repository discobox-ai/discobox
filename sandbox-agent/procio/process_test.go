package procio

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func start(t *testing.T, opts Options) *Process {
	t.Helper()
	// Every child below is a POSIX command — sh, cat, sleep, printf — and the
	// TTY cases want a PTY this package deliberately does not open on Windows
	// (see lineeditor_windows.go). procio starts processes inside the Linux
	// sandbox, so a Windows host has neither half of the premise.
	if runtime.GOOS == "windows" {
		t.Skip("procio starts POSIX children on a PTY; the sandbox it runs in is Linux")
	}
	if opts.SysProcAttr == nil {
		opts.SysProcAttr = newSessionAttr()
	}
	if opts.Dir == "" {
		// Run every test child in its own scratch directory. A signal whose
		// default action dumps core (TestExitCodeUsesShellConventionForSignals
		// sends SIGQUIT) writes that dump to the *child's* working directory,
		// which would otherwise be this package's source directory: the kernel
		// core_pattern is a bare "core" on the machines this runs on. Landing
		// it under t.TempDir() means the test framework removes it.
		opts.Dir = t.TempDir()
	}
	p, err := Start(opts)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { p.Terminate(); p.Close() })
	return p
}

// A command that exits immediately must not lose its output. Wait runs
// concurrently with the reader, and os/exec closes the pipes *it* created as
// soon as the process exits — so pipes owned elsewhere are the only way this
// holds.
func TestFastCommandOutputSurvivesConcurrentWait(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("printf is a POSIX command; procio's children run in the Linux sandbox")
	}
	for i := range 50 {
		p, err := Start(Options{Command: []string{"printf", "hi"}, Env: os.Environ()})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		// Wait in parallel with draining, which is how a caller uses this.
		waited := make(chan Status, 1)
		go func() { waited <- p.Wait() }()
		out, err := io.ReadAll(p.Stdout())
		if err != nil {
			t.Fatalf("run %d: read stdout: %v", i, err)
		}
		<-waited
		p.Close()
		if string(out) != "hi" {
			t.Fatalf("run %d: stdout = %q, want hi", i, string(out))
		}
	}
}

// A pipe process keeps its streams apart, so a caller can route each the way a
// local command does.
func TestPipeProcessSeparatesStdoutAndStderr(t *testing.T) {
	p := start(t, Options{
		Command: []string{"sh", "-c", "printf out; printf err >&2"},
		Env:     os.Environ(),
	})
	var stdout, stderr []byte
	done := make(chan struct{}, 2)
	go func() { stdout, _ = io.ReadAll(p.Stdout()); done <- struct{}{} }()
	go func() { stderr, _ = io.ReadAll(p.Stderr()); done <- struct{}{} }()
	<-done
	<-done
	p.Wait()

	if string(stdout) != "out" {
		t.Fatalf("stdout = %q, want out", stdout)
	}
	if string(stderr) != "err" {
		t.Fatalf("stderr = %q, want err", stderr)
	}
}

// A TTY process has nothing to separate: the kernel merged both onto the PTY
// before the parent can see them, so Stderr is nil and everything is stdout.
func TestTTYProcessMergesOntoStdout(t *testing.T) {
	p := start(t, Options{
		Command: []string{"sh", "-c", "printf out; printf err >&2"},
		Env:     os.Environ(),
		TTY:     true,
		Winsize: &pty.Winsize{Rows: 24, Cols: 80},
	})
	if p.Stderr() != nil {
		t.Fatal("a TTY process must not expose a stderr stream")
	}
	if p.TTY() == nil {
		t.Fatal("a TTY process must expose its PTY")
	}
	out := readAvailable(p.Stdout(), 500*time.Millisecond)
	p.Wait()
	if !bytes.Contains(out, []byte("out")) || !bytes.Contains(out, []byte("err")) {
		t.Fatalf("stdout = %q, want both writes merged", out)
	}
}

// Go reports -1 for a signal death, which loses the signal. Callers need the
// shell convention so an interrupted command is distinguishable from a failure.
func TestExitCodeUsesShellConventionForSignals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no signal exit convention on windows")
	}
	for _, tc := range []struct {
		name string
		sig  string
		want int64
	}{
		{"interrupt", "INT", 130},
		{"terminate", "TERM", 143},
		{"hangup", "HUP", 129},
		{"quit", "QUIT", 131},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := start(t, Options{Command: []string{"sleep", "30"}, Env: os.Environ()})
			waitForState(t, p, "S")
			if err := p.Signal(tc.sig); err != nil {
				t.Fatalf("signal: %v", err)
			}
			if got := p.Wait().ExitCode; got != tc.want {
				t.Fatalf("exit code = %d, want %d", got, tc.want)
			}
		})
	}
}

// An ordinary non-zero exit is reported as itself, not as a signal status.
func TestExitCodePassesThroughOrdinaryStatus(t *testing.T) {
	p := start(t, Options{Command: []string{"sh", "-c", "exit 42"}, Env: os.Environ()})
	if got := p.Wait().ExitCode; got != 42 {
		t.Fatalf("exit code = %d, want 42", got)
	}
}

// The mapping that is silently wrong if written the obvious way: a process in a
// new session is in an orphaned process group, and the kernel discards SIGTSTP
// sent to one. Only SIGSTOP actually stops it.
func TestSuspendStopsAnOrphanedProcessGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process state is read from /proc")
	}
	p := start(t, Options{Command: []string{"sleep", "30"}, Env: os.Environ()})
	waitForState(t, p, "S")

	if err := p.Signal("TSTP"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	waitForState(t, p, "T")

	if err := p.Signal("CONT"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	waitForState(t, p, "S")
}

// Closing stdin is what lets a command that reads to EOF finish.
func TestCloseInputEndsAReaderCommand(t *testing.T) {
	p := start(t, Options{Command: []string{"cat"}, Env: os.Environ()})
	if _, err := p.WriteInput([]byte("piped")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	p.CloseInput()
	out, err := io.ReadAll(p.Stdout())
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if got := p.Wait(); got.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", got.ExitCode)
	}
	if string(out) != "piped" {
		t.Fatalf("stdout = %q, want piped", out)
	}
}

// A TTY's input side is the terminal itself; closing it would break the session
// rather than signal end-of-input.
func TestCloseInputIsANoOpForTTY(t *testing.T) {
	p := start(t, Options{
		Command: []string{"sh", "-c", "sleep 5"},
		Env:     os.Environ(),
		TTY:     true,
		Winsize: &pty.Winsize{Rows: 24, Cols: 80},
	})
	p.CloseInput()
	if _, err := p.WriteInput([]byte("still open\n")); err != nil {
		t.Fatalf("writing after CloseInput on a TTY failed: %v", err)
	}
}

// The size a caller asks for reaches the program.
func TestResizeAppliesToTheTTY(t *testing.T) {
	p := start(t, Options{
		Command: []string{"sh", "-c", "sleep 5"},
		Env:     os.Environ(),
		TTY:     true,
		Winsize: &pty.Winsize{Rows: 24, Cols: 80},
	})
	if err := p.Resize(33, 101); err != nil {
		t.Fatalf("resize: %v", err)
	}
	size, err := pty.GetsizeFull(p.TTY())
	if err != nil {
		t.Fatalf("get size: %v", err)
	}
	if size.Rows != 33 || size.Cols != 101 {
		t.Fatalf("size = %dx%d, want 33x101", size.Rows, size.Cols)
	}
}

// readAvailable reads until the reader goes quiet for d.
func readAvailable(r io.Reader, d time.Duration) []byte {
	out := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		out <- buf.Bytes()
	}()
	select {
	case b := <-out:
		return b
	case <-time.After(d):
		return nil
	}
}

// waitForState polls /proc until the process reaches want ("S" sleeping, "T"
// stopped).
func waitForState(t *testing.T, p *Process, want string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		time.Sleep(100 * time.Millisecond)
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", p.PID()))
		if err == nil {
			// The state field follows the parenthesized comm, which may itself
			// contain spaces or parentheses.
			if idx := strings.LastIndex(string(data), ")"); idx >= 0 && idx+2 < len(data) {
				got = string(data[idx+2 : idx+3])
				if got == want {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process state = %q, want %q", got, want)
}
