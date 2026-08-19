package localpty

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/term"
)

// The command under test is this test binary, re-run in one of the helper modes
// below. Asking a program from inside the pty what it can see is the only
// portable way to ask whether it got a terminal at all — a shell script would
// test /bin/sh on one platform and PowerShell's startup on the other, and
// neither is what this package promises.
const helperEnv = "LOCALPTY_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "report":
		helperReport()
	case "sizes":
		helperSizes()
	case "print":
		fmt.Print("done\r\n")
	case "fail":
		fmt.Print("that did not work\r\n")
		os.Exit(3)
	case "wait":
		time.Sleep(time.Hour)
	}
	os.Exit(0)
}

// helperReport says what the command can see of its terminal, then reads a line
// the way a prompt does.
func helperReport() {
	out := int(os.Stdout.Fd())
	if term.IsTerminal(out) {
		fmt.Print("ON-A-TTY\r\n")
	}
	if cols, rows, err := term.GetSize(out); err == nil {
		fmt.Printf("SIZE %d %d\r\n", cols, rows)
	}
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	fmt.Printf("GOT:%s\r\n", strings.TrimSpace(line))
}

// helperSizes reports the terminal size until it is killed, which is how a
// resize is observed from the far side without a signal the two platforms do
// not share.
func helperSizes() {
	for {
		if cols, rows, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
			fmt.Printf("SIZE %d %d\r\n", cols, rows)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A command on a pty is a terminal: it prints, it believes it is on one, it is
// told the size of the pane, and it can be typed at.
func TestACommandRunsOnATerminal(t *testing.T) {
	p := helper(t, "report", 100, 30)
	out := readAll(p)

	// Typed straight away: the pty holds it until the command reads.
	if _, err := p.Write([]byte("typed\r")); err != nil {
		t.Fatalf("write: %v", err)
	}

	out.wait(t, "ON-A-TTY", "the command should see a terminal on stdout")
	out.wait(t, "SIZE 100 30", "the command should be told the pane's size")
	// And it takes input, which is how a prompt is answered in a pane.
	out.wait(t, "GOT:typed", "the command should have read what was typed at it")
}

// The pane is resized as the window is, and the command is told.
func TestResizeReachesTheCommand(t *testing.T) {
	p := helper(t, "sizes", 100, 30)
	out := readAll(p)
	out.wait(t, "SIZE 100 30", "the command should start at the size it was given")

	if err := p.Resize(120, 40); err != nil {
		t.Fatalf("resize: %v", err)
	}
	out.wait(t, "SIZE 120 40", "the command should be told the new size")
}

// A command that exits ends its terminal cleanly.
//
// Neither platform arrives here on its own: a Unix pty master reports EIO once
// the last slave descriptor closes, and a Windows pseudo-console holds the
// output pipe open until it is closed. Both are end of file to a pane, and
// anything else puts an error on screen every time a fast command finishes.
func TestAFinishedCommandReadsAsEndOfFile(t *testing.T) {
	p := helper(t, "print", 80, 24)

	var seen strings.Builder
	buf := make([]byte, 1024)
	for {
		n, err := p.Read(buf)
		seen.Write(buf[:n])
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("read after exit = %v, want end of file", err)
			}
			break
		}
	}
	if !strings.Contains(seen.String(), "done") {
		t.Fatalf("output = %q, want what the command printed", seen.String())
	}
}

// How a command ended is worth knowing, because a pane that says "finished"
// over a command that failed is a screen disagreeing with the output above it.
func TestExitStatusIsTheCommandsOwn(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want int
	}{
		{"print", 0},
		{"fail", 3},
	} {
		p := helper(t, tc.mode, 80, 24)
		// The status is asked for where the pane asks for it: at end of file,
		// with the output read out.
		_, _ = io.Copy(io.Discard, p)

		reporter, ok := p.(ExitReporter)
		if !ok {
			t.Fatal("a local command should be able to say how it ended")
		}
		code, done := reporter.ExitStatus()
		if !done {
			t.Fatalf("%s: the command has ended and the status says otherwise", tc.mode)
		}
		if code != tc.want {
			t.Fatalf("%s: exit = %d, want %d", tc.mode, code, tc.want)
		}
	}
}

// Closing ends the command: one waiting on a key must not outlive the pane it
// was drawn in. Closing twice is what a pane that already ended being dismissed
// amounts to, and is not an error.
func TestCloseEndsACommandThatIsStillRunning(t *testing.T) {
	p := helper(t, "wait", 80, 24)

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := p.Read(buf); err != nil {
				done <- err
				return
			}
		}
	}()

	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	select {
	case <-done:
		// The read ended, which is the command being gone.
	case <-time.After(10 * time.Second):
		t.Fatal("closing should have ended the read")
	}
}

// A size nobody has laid out yet is not an error: a pane sizes itself after it
// opens, and a terminal started at zero draws itself wrong before anything can
// correct it.
func TestAnUnsizedPaneGetsADefault(t *testing.T) {
	p := helper(t, "report", 0, 0)
	out := readAll(p)
	out.wait(t, fmt.Sprintf("SIZE %d %d", defaultCols, defaultRows), "an unsized pty should open at the default")

	if err := p.Resize(0, 0); err != nil {
		t.Fatalf("resize to nothing = %v, want it ignored", err)
	}
}

// helper starts this test binary on a pty, in one of the modes above.
func helper(t *testing.T, mode string, cols, rows int) PTY {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("find the test binary: %v", err)
	}
	p, err := Start(t.Context(), Command{
		Path: self,
		Env:  append(os.Environ(), helperEnv+"="+mode),
	}, cols, rows)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// tail is everything the command has printed so far. The pty is read by one
// reader — two would take turns at the same stream and each see half of it.
type tail struct {
	mu   sync.Mutex
	text strings.Builder
}

func readAll(p PTY) *tail {
	out := &tail{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := p.Read(buf)
			out.mu.Lock()
			out.text.Write(buf[:n])
			out.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return out
}

func (o *tail) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.String()
}

// wait blocks until the command has printed want, or fails saying what it did
// print instead.
func (o *tail) wait(t *testing.T, want, why string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(o.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s: waited for %q, got %q", why, want, o.String())
}
