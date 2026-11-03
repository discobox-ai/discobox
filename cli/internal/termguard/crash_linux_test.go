package termguard

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// helperEnv makes the test binary one of the two processes a crash involves:
// "program" takes the terminal over the way the console does and then panics
// on a goroutine Bubble Tea would not have recovered, and "guard" is Serve.
const helperEnv = "TERMGUARD_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "guard":
		if err := Serve(os.Stdin, os.Stdout, os.Args[len(os.Args)-1]); err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	case "program":
		crashHoldingTheTerminal()
	case "finish":
		finishProperly()
	case "hangup":
		hangUpHoldingTheTerminal()
	}
	os.Exit(m.Run())
}

func crashHoldingTheTerminal() {
	self, _ := os.Executable()
	_ = os.Setenv(helperEnv, "guard")
	if _, err := Start([]string{self}); err != nil {
		os.Stderr.WriteString("no guard: " + err.Error())
		os.Exit(3)
	}
	if _, err := term.MakeRaw(int(os.Stdin.Fd())); err != nil {
		os.Exit(4)
	}
	os.Stdout.WriteString("\x1b[?1049h\x1b[?1003h\x1b[?1006hTHE CONSOLE")
	go func() { panic("boom on a goroutine nobody recovers") }()
	select {}
}

func hangUpHoldingTheTerminal() {
	self, _ := os.Executable()
	_ = os.Setenv(helperEnv, "guard")
	if _, err := Start([]string{self}); err != nil {
		os.Stderr.WriteString("no guard: " + err.Error())
		os.Exit(3)
	}
	if _, err := term.MakeRaw(int(os.Stdin.Fd())); err != nil {
		os.Exit(4)
	}
	os.Stdout.WriteString("\x1b[?1049h\x1b[?1003hTHE CONSOLE")
	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(10 * time.Second)
	os.Exit(5)
}

func finishProperly() {
	self, _ := os.Executable()
	_ = os.Setenv(helperEnv, "guard")
	guard, err := Start([]string{self})
	if err != nil {
		os.Stderr.WriteString("no guard: " + err.Error())
		os.Exit(3)
	}
	os.Stdout.WriteString("THE CONSOLE")
	guard.Release()
	os.Stdout.WriteString("GONE")
	os.Exit(0)
}

func TestAProperExitIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	got, _ := runOnTerminal(t, "finish", dir, "GONE")
	if strings.Contains(got, resetSequence) || strings.Contains(got, "discobox:") {
		t.Fatalf("the guard spoke after a proper exit: %q", got)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("a proper exit left a report behind: %v", left)
	}
}

func TestACrashLeavesTheTerminalAsItWasFound(t *testing.T) {
	dir := t.TempDir()
	got, changed := runOnTerminal(t, "program", dir, "The crash report is in ")
	console := strings.Index(got, "THE CONSOLE")
	reset := strings.LastIndex(got, resetSequence)
	if console < 0 || reset < console {
		t.Fatalf("want the reset after the console's frame, got %q", got)
	}
	if strings.Contains(got[reset:], "boom") {
		t.Errorf("the report was printed to the terminal rather than only named: %q", got[reset:])
	}
	reports, _ := filepath.Glob(filepath.Join(dir, "discobox-console-crash-*.log"))
	if len(reports) != 1 || !strings.Contains(got[reset:], reports[0]) {
		t.Fatalf("want the one report named on the terminal, have %v and %q", reports, got[reset:])
	}
	if report, _ := os.ReadFile(reports[0]); !strings.Contains(string(report), "boom on a goroutine nobody recovers") {
		t.Errorf("the report does not hold the panic: %q", report)
	}
	if changed != "" {
		t.Error(changed)
	}
}

func TestAHangupIsNamed(t *testing.T) {
	if signal.Ignored(syscall.SIGHUP) {
		t.Skip("SIGHUP is ignored here, and the program would inherit that")
	}
	dir := t.TempDir()
	got, changed := runOnTerminal(t, "hangup", dir, "the terminal has been restored")
	reset := strings.LastIndex(got, resetSequence)
	if reset < 0 || !strings.Contains(got[reset:], "was ended by SIGHUP;") {
		t.Fatalf("want the hangup named after the reset, got %q", got)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("a hangup left a report behind: %v", left)
	}
	if changed != "" {
		t.Error(changed)
	}
}

// runOnTerminal runs the test binary as helper on a fresh pty, as the leader of
// its own session, and returns what the terminal was sent once until appears,
// and what about the terminal's mode differs from before, if anything.
func runOnTerminal(t *testing.T, helper, dir, until string) (string, string) {
	t.Helper()
	master, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer master.Close()
	defer tty.Close()
	before, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}

	//nolint:gosec // the test binary, re-run as one of its helpers
	cmd := exec.CommandContext(t.Context(), os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"="+helper, "TMPDIR="+dir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var out bytes.Buffer
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	_ = cmd.Wait()

	var got string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		got = out.String()
		mu.Unlock()
		if strings.Contains(got, until) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the terminal never got %q; it got %q", until, got)
		}
	}

	// The guard is a process of its own, not waited for above, and it restores
	// the mode only after its message is written: the message arriving says the
	// restore is about to happen, not that it has. So the mode is polled until
	// it is the one from before, and only a mode still changed at the deadline
	// is one the guard left behind.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		after, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		if after.Lflag == before.Lflag && after.Iflag == before.Iflag && after.Oflag == before.Oflag {
			return got, ""
		}
		if time.Now().After(deadline) {
			return got, fmt.Sprintf("the terminal was left in another mode: lflag %#x -> %#x", before.Lflag, after.Lflag)
		}
	}
}
