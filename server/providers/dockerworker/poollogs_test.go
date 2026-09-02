package dockerworker

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

func writeConsoleLog(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "console.log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write console log: %v", err)
	}
	return path
}

// A tail reads the last N lines whole: the trailing newline ending the file is
// not a separator, so -n 1 is the last line rather than an empty one.
func TestTailFileReadsLastLines(t *testing.T) {
	path := writeConsoleLog(t, "boot 1", "boot 2", "boot 3", "boot 4")
	for _, tc := range []struct {
		tail int
		want string
	}{
		{tail: 1, want: "boot 4\n"},
		{tail: 2, want: "boot 3\nboot 4\n"},
		{tail: 4, want: "boot 1\nboot 2\nboot 3\nboot 4\n"},
		{tail: 99, want: "boot 1\nboot 2\nboot 3\nboot 4\n"},
		{tail: 0, want: "boot 1\nboot 2\nboot 3\nboot 4\n"},
	} {
		stream, err := TailFile(context.Background(), path, sandbox.PoolLogOptions{Tail: tc.tail})
		if err != nil {
			t.Fatalf("tail %d: %v", tc.tail, err)
		}
		got, err := io.ReadAll(stream)
		_ = stream.Close()
		if err != nil {
			t.Fatalf("read tail %d: %v", tc.tail, err)
		}
		if string(got) != tc.want {
			t.Fatalf("tail %d = %q, want %q", tc.tail, got, tc.want)
		}
	}
}

// The backwards scan has to cross its own read boundary, or a tail of a console
// log longer than one chunk returns the wrong lines.
func TestTailFileScansPastChunkBoundary(t *testing.T) {
	lines := make([]string, 0, 5000)
	for i := 0; i < 5000; i++ {
		lines = append(lines, strings.Repeat("x", 40))
	}
	lines[4990] = "the line we want"
	path := writeConsoleLog(t, lines...)

	stream, err := TailFile(context.Background(), path, sandbox.PoolLogOptions{Tail: 10})
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	defer stream.Close()
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasPrefix(string(got), "the line we want\n") {
		t.Fatalf("tail started at %q", firstLineOf(string(got)))
	}
	if lineCount := strings.Count(string(got), "\n"); lineCount != 10 {
		t.Fatalf("tail returned %d lines, want 10", lineCount)
	}
}

// Following waits at the end of the file instead of reporting EOF, which is
// what makes a boot readable as it happens.
func TestTailFileFollowsAppends(t *testing.T) {
	path := writeConsoleLog(t, "booting")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := TailFile(ctx, path, sandbox.PoolLogOptions{Tail: 1, Follow: true})
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	defer stream.Close()

	buf := make([]byte, 64)
	n, err := stream.Read(buf)
	if err != nil || string(buf[:n]) != "booting\n" {
		t.Fatalf("first read = %q, %v", buf[:n], err)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := file.WriteString("docker started\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = file.Close()

	n, err = stream.Read(buf)
	if err != nil {
		t.Fatalf("follow read: %v", err)
	}
	if string(buf[:n]) != "docker started\n" {
		t.Fatalf("follow read = %q", buf[:n])
	}
}

// A followed read ends when its caller's context does; otherwise a client that
// disconnected would hold the file open for as long as the server runs.
func TestTailFileFollowEndsWithContext(t *testing.T) {
	path := writeConsoleLog(t, "booting")
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := TailFile(ctx, path, sandbox.PoolLogOptions{Follow: true})
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	defer stream.Close()
	if _, err := io.CopyN(io.Discard, stream, int64(len("booting\n"))); err != nil {
		t.Fatalf("read: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := stream.Read(make([]byte, 16))
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("read after cancel = %v, want io.EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a followed read did not end when its context did")
	}
}

// The journal invocation names every packaging of the daemon at once, and
// carries the caller's tail and follow through.
func TestJournalCommandCarriesOptions(t *testing.T) {
	command := strings.Join(JournalCommand(sandbox.PoolLogOptions{Tail: 50, Follow: true}), " ")
	for _, want := range []string{"journalctl", "--no-pager", "-u docker.service", "-u docker.socket", "-u snap.docker.dockerd.service", "-n 50", "-f"} {
		if !strings.Contains(command, want) {
			t.Fatalf("journal command %q is missing %q", command, want)
		}
	}
	whole := strings.Join(JournalCommand(sandbox.PoolLogOptions{}), " ")
	if strings.Contains(whole, "-n ") || strings.Contains(whole, " -f") {
		t.Fatalf("unbounded journal command = %q", whole)
	}
	if !strings.Contains(whole, "--no-tail") {
		t.Fatalf("unbounded journal command %q must ask for the whole log", whole)
	}
}

// A command's stderr is part of the log the operator reads: it is where the
// tools being run explain an empty result, and a non-zero exit after that ends
// the stream rather than discarding what was said.
func TestStreamCommandMergesStderrAndEndsOnExit(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "echo to-stdout; echo to-stderr >&2; exit 3")
	stream, err := StreamCommand(cmd, "test log")
	if err != nil {
		t.Fatalf("stream command: %v", err)
	}
	defer stream.Close()
	if stream.Source != "test log" {
		t.Fatalf("source = %q", stream.Source)
	}
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(out), "to-stdout") || !strings.Contains(string(out), "to-stderr") {
		t.Fatalf("output = %q, want both streams", out)
	}
}

// Closing kills the command, which is the only thing that ends a --follow read
// — and it has to kill what the command started, not just the command.
//
// The fixture backgrounds a sleep and waits on it, which is the shape of every
// scripted backend: the grandchild holds this stream's write end, so a close
// that killed only the shell would leave the command unreaped until the sleep
// ended on its own. That shows up here as a close that takes the full
// commandStopWait instead of returning at once.
func TestStreamCommandCloseStopsTheCommandAndItsChildren(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "echo started; sleep 300 & wait")
	stream, err := StreamCommand(cmd, "test log")
	if err != nil {
		t.Fatalf("stream command: %v", err)
	}
	if _, err := stream.Read(make([]byte, 64)); err != nil {
		t.Fatalf("read: %v", err)
	}

	closed := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_ = stream.Close()
		closed <- time.Since(start)
	}()
	select {
	case elapsed := <-closed:
		if elapsed >= commandStopWait {
			t.Fatalf("close took %s: the command outlived the kill", elapsed)
		}
	case <-time.After(commandStopWait + 10*time.Second):
		t.Fatal("closing the stream did not return at all")
	}
}

func firstLineOf(value string) string {
	line, _, _ := strings.Cut(value, "\n")
	return line
}
