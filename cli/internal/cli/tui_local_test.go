package cli

import (
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A command on a pty is a terminal: it prints, it takes input, it can be
// resized, and closing it ends the process rather than leaving it holding a
// terminal nobody is drawing.
func TestLocalCommandIsATerminal(t *testing.T) {
	// It believes it is on a terminal, and it is told the size of the pane.
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", `[ -t 1 ] && echo ON-A-TTY; stty size < /dev/tty; read line; echo "GOT:$line"`)
	term, err := startOnPTY(command, 100, 30)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer term.Close()

	// Typed straight away: the pty holds it until the command reads, and one
	// reader takes the whole conversation. Two would race each other for it.
	if _, err := term.Write([]byte("typed\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := readFor(t.Context(), term, 3*time.Second)

	if !strings.Contains(out, "ON-A-TTY") {
		t.Fatalf("the command should see a terminal on stdout: %q", out)
	}
	if !strings.Contains(out, "30 100") {
		t.Fatalf("the command should be told the pane's size, got %q", out)
	}
	// And it takes input, which is how a pager is driven in a pane.
	if !strings.Contains(out, "GOT:typed") {
		t.Fatalf("the command should have read what was typed at it: %q", out)
	}
}

// Closing kills the command: a pager waiting on a key must not outlive the pane
// it was drawn in.
func TestLocalCommandCloseEndsIt(t *testing.T) {
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", "while true; do sleep 1; done")
	term, err := startOnPTY(command, 80, 24)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := term.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Safe twice, which is what a pane closing on its own then being closed
	// again amounts to.
	if err := term.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if command.ProcessState == nil {
		t.Fatal("the command should have been waited on")
	}
}

// The flags a child is given are the ones this invocation is using, so it talks
// to the same server, project and directory — and the token is not among them,
// since every process on the machine can read an argument list.
func TestGlobalFlagsCarryTheSessionButNotTheToken(t *testing.T) {
	app := &App{serverURL: "unix:///run/x.sock", projectID: "obot", source: "/src/disco2", token: "secret", noStart: true}
	flags := strings.Join(app.globalFlags(), " ")

	for _, want := range []string{"--server unix:///run/x.sock", "--project obot", "--chdir /src/disco2", "--no-start"} {
		if !strings.Contains(flags, want) {
			t.Errorf("flags %q missing %q", flags, want)
		}
	}
	if strings.Contains(flags, "secret") {
		t.Errorf("the token should not be in the argument list: %q", flags)
	}
}

// A command that exits ends its terminal cleanly.
//
// Reading a pty master after the last slave descriptor closes fails with EIO on
// Linux rather than returning end of file, and that is exactly what a command
// finishing looks like from this side. Left alone it puts "read /dev/ptmx:
// input/output error" on screen every time a fast command completes.
func TestLocalCommandExitReadsAsEndOfFile(t *testing.T) {
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", "echo done")
	term, err := startOnPTY(command, 80, 24)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer term.Close()

	var seen strings.Builder
	buf := make([]byte, 1024)
	for {
		n, err := term.Read(buf)
		seen.Write(buf[:n])
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("read after exit = %v, want EOF", err)
			}
			break
		}
	}
	if !strings.Contains(seen.String(), "done") {
		t.Fatalf("output = %q", seen.String())
	}
}
