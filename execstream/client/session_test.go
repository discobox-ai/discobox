package client

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/execstream/frame"
)

type pipeConn struct {
	conn net.Conn
	mu   sync.Mutex
}

func (c *pipeConn) ReadFrame() (frame.Frame, error) { return frame.Read(c.conn) }
func (c *pipeConn) WriteFrame(typ byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return frame.Write(c.conn, typ, payload)
}
func (c *pipeConn) Close() error { return c.conn.Close() }

func newPipe(t *testing.T) (*pipeConn, net.Conn) {
	t.Helper()
	client, remote := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = remote.Close() })
	return &pipeConn{conn: client}, remote
}

// fakeConsole records what a session does to the terminal, in order, and lets a
// test deliver signals without involving the process.
type fakeConsole struct {
	mu      sync.Mutex
	events  []string
	signals chan<- os.Signal
	ready   chan struct{}
	cols    int
	rows    int
	// suspended is closed each time Suspend is entered; resume releases it.
	resume chan struct{}
}

func newFakeConsole() *fakeConsole {
	return &fakeConsole{ready: make(chan struct{}), cols: 80, rows: 24, resume: make(chan struct{}, 1)}
}

func (c *fakeConsole) record(event string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *fakeConsole) Events() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

func (c *fakeConsole) MakeRaw() (func(), bool, error) {
	c.record("makeraw")
	return func() { c.record("restore") }, true, nil
}

func (c *fakeConsole) Size() (int, int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cols, c.rows, true
}

func (c *fakeConsole) Suspend() {
	c.record("suspend")
	<-c.resume
	c.record("resumed")
}

func (c *fakeConsole) NotifySignals(ch chan<- os.Signal) {
	c.mu.Lock()
	c.signals = ch
	c.mu.Unlock()
	close(c.ready)
}

func (c *fakeConsole) StopSignals(chan<- os.Signal) {}

func (c *fakeConsole) IsSuspendSignal(sig os.Signal) bool {
	return testSuspendSignal != nil && sig == testSuspendSignal
}

func (c *fakeConsole) SignalName(sig os.Signal) (string, bool) {
	switch {
	case sig == os.Interrupt:
		return "INT", true
	case sig == syscall.SIGTERM:
		return "TERM", true
	case testSuspendSignal != nil && sig == testSuspendSignal:
		return "TSTP", true
	}
	return "", false
}

// deliver sends a signal to the session once it is listening.
func (c *fakeConsole) deliver(t *testing.T, sig os.Signal) {
	t.Helper()
	select {
	case <-c.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("session never subscribed to signals")
	}
	c.mu.Lock()
	ch := c.signals
	c.mu.Unlock()
	ch <- sig
}

// readFrames collects frames from the remote end.
func readFrames(t *testing.T, remote net.Conn, n int) []frame.Frame {
	t.Helper()
	out := make([]frame.Frame, 0, n)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for len(out) < n {
			f, err := frame.Read(remote)
			if err != nil {
				return
			}
			out = append(out, f)
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out reading %d frames, got %d", n, len(out))
	}
	return out
}

// Ordinary signals are forwarded, not acted on locally.
func TestSignalsAreForwardedAsFrames(t *testing.T) {
	conn, remote := newPipe(t)
	console := newFakeConsole()
	s := New(Options{
		Conn: conn, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
		Console: console, Kind: "test",
	})
	go func() { _ = s.Run(context.Background()) }()

	console.deliver(t, os.Interrupt)
	got := readFrames(t, remote, 1)
	if len(got) != 1 || got[0].Type != frame.Signal || string(got[0].Payload) != "INT" {
		t.Fatalf("frame = %+v, want an INT signal", got)
	}
}

// stdout and stderr frames reach the caller's own streams, so a redirect on
// either side behaves as it would for a local command.
func TestOutputFramesRouteToTheMatchingStream(t *testing.T) {
	conn, remote := newPipe(t)
	var stdout, stderr strings.Builder
	s := New(Options{Conn: conn, Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, Kind: "test"})
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()

	for _, f := range []struct {
		typ     byte
		payload string
	}{{frame.Stdout, "out"}, {frame.Stderr, "err"}} {
		if err := frame.Write(remote, f.typ, []byte(f.payload)); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	exit, _ := frame.EncodeExit("exited", ptr(int64(0)), "")
	_ = frame.Write(remote, frame.Exit, exit)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session did not finish")
	}
	if stdout.String() != "out" || stderr.String() != "err" {
		t.Fatalf("stdout = %q, stderr = %q, want out and err", stdout.String(), stderr.String())
	}
}

// A non-zero remote status must surface as an ExitError carrying the code, so
// the caller can exit with it.
func TestExitFrameBecomesExitError(t *testing.T) {
	conn, remote := newPipe(t)
	s := New(Options{Conn: conn, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard, Kind: "test"})
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()

	payload, _ := frame.EncodeExit("exited", ptr(int64(42)), "")
	_ = frame.Write(remote, frame.Exit, payload)

	select {
	case err := <-done:
		var exit ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 42 {
			t.Fatalf("Run err = %v, want ExitError 42", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session did not finish")
	}
}

// A signal death arrives as 128+signum and must survive the clamp intact.
func TestSignalExitCodesSurvive(t *testing.T) {
	for _, code := range []int64{130, 143, 129} {
		payload, _ := frame.EncodeExit("failed", &code, "signal")
		err := ExitErrorFromPayload("test", payload)
		var exit ExitError
		if !errors.As(err, &exit) || int64(exit.ExitCode()) != code {
			t.Fatalf("exit code = %v, want %d", err, code)
		}
	}
}

// Raw mode is entered for the session and always handed back, including when
// the remote ends the stream.
func TestRawModeIsRestoredWhenTheSessionEnds(t *testing.T) {
	conn, remote := newPipe(t)
	console := newFakeConsole()
	s := New(Options{
		Conn: conn, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
		Console: console, RawMode: true, Kind: "test",
	})
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()

	payload, _ := frame.EncodeExit("exited", ptr(int64(0)), "")
	_ = frame.Write(remote, frame.Exit, payload)
	<-done

	if events := console.Events(); !ordered(events, "makeraw", "restore") {
		t.Fatalf("events = %v, want raw mode entered and restored", events)
	}
}

func waitFor(t *testing.T, c *fakeConsole, event string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range c.Events() {
			if e == event {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("console never recorded %q, got %v", event, c.Events())
}

// ordered reports whether want appears in events in that relative order.
func ordered(events []string, want ...string) bool {
	i := 0
	for _, e := range events {
		if i < len(want) && e == want[i] {
			i++
		}
	}
	return i == len(want)
}

func ptr[T any](v T) *T { return &v }
