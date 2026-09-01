package client

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/execstream/frame"
)

// compressInterruptStall makes the escape's patience test-sized. The rule it
// enforces is "the interrupt before this one has gone unanswered", not a
// particular number of seconds.
func compressInterruptStall(t *testing.T) {
	t.Helper()
	previous := interruptStall
	interruptStall = 10 * time.Millisecond
	t.Cleanup(func() { interruptStall = previous })
}

// deliveryPipe is a transport that acknowledges, like the resumable stream the
// CLI attaches terminals over. Positions are counted per frame written, which
// is all the escape needs: a position it recorded, and whether the host has
// caught up to it.
type deliveryPipe struct {
	*pipeConn
	mu           sync.Mutex
	accepted     uint64
	acknowledged uint64
}

func (c *deliveryPipe) WriteFrame(typ byte, payload []byte) error {
	c.mu.Lock()
	c.accepted++
	c.mu.Unlock()
	return c.pipeConn.WriteFrame(typ, payload)
}

func (c *deliveryPipe) Positions() (uint64, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accepted, c.acknowledged
}

// applyAll is the host acknowledging everything written so far.
func (c *deliveryPipe) applyAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acknowledged = c.accepted
}

// A remote that never answers leaves the caller nothing that ends the attach,
// so repeated interrupts end it here instead — after warning that they will.
func TestUnansweredInterruptsEndTheSession(t *testing.T) {
	compressInterruptStall(t)
	conn, remote := newPipe(t)
	console := newFakeConsole()
	notices := make(chan struct{}, 4)
	s := New(Options{
		Conn: conn, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
		Console: console, Kind: "test",
		InterruptNotice: func() { notices <- struct{}{} },
	})
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()

	// The first two are forwarded like any other signal; only the third, with
	// both of them still unanswered, escapes.
	for range 2 {
		console.deliver(t, os.Interrupt)
		if got := readFrames(t, remote, 1); len(got) != 1 || string(got[0].Payload) != "INT" {
			t.Fatalf("frame = %+v, want an INT signal", got)
		}
		time.Sleep(2 * interruptStall)
	}
	console.deliver(t, os.Interrupt)

	select {
	case err := <-done:
		if !errors.Is(err, ErrInterrupted) {
			t.Fatalf("Run err = %v, want ErrInterrupted", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session did not end on repeated interrupts")
	}
	if len(notices) != 1 {
		t.Fatalf("notices = %d, want exactly one warning before the escape", len(notices))
	}
}

// A remote that is answering is not stalled, whatever it does about the
// interrupt itself, so the run starts over and the session stays up.
func TestAnsweredInterruptsNeverEscape(t *testing.T) {
	compressInterruptStall(t)
	conn, remote := newPipe(t)
	console := newFakeConsole()
	output := make(chan string, 4)
	s := New(Options{
		Conn: conn, Stdin: strings.NewReader(""), Stdout: writerFunc(func(p []byte) { output <- string(p) }),
		Stderr: io.Discard, Console: console, Kind: "test",
		InterruptNotice: func() { t.Error("warned about a remote that was answering") },
	})
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()

	for range 3 {
		console.deliver(t, os.Interrupt)
		readFrames(t, remote, 1)
		// The remote reacts, which is the only evidence a transport without
		// acknowledgements offers. Wait for the session to have taken it in
		// before the next interrupt asks whether it arrived.
		if err := frame.Write(remote, frame.Stdout, []byte("^C")); err != nil {
			t.Fatalf("write frame: %v", err)
		}
		select {
		case <-output:
		case <-time.After(3 * time.Second):
			t.Fatal("session never read the remote's answer")
		}
		time.Sleep(2 * interruptStall)
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
}

// Ctrl-C typed into a raw terminal is opaque data, so it counts only where the
// transport can prove the host never applied it.
func TestRawInterruptsEscapeWhenUnacknowledged(t *testing.T) {
	compressInterruptStall(t)
	pipe, remote := newPipe(t)
	conn := &deliveryPipe{pipeConn: pipe}
	console := newFakeConsole()
	copied := make(chan struct{})
	s := New(Options{
		Conn: conn, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
		Console: console, RawMode: true, Kind: "test",
		CopyInput: typedInterrupts(3, nil, copied),
	})
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()

	readFrames(t, remote, 2)
	select {
	case err := <-done:
		if !errors.Is(err, ErrInterrupted) {
			t.Fatalf("Run err = %v, want ErrInterrupted", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session did not end on repeated raw interrupts")
	}
	waitForClose(t, copied, "typed interrupts to finish")
}

// The same keystrokes against a host that is applying them are an impatient
// caller in a healthy session, and must reach the remote untouched.
func TestAcknowledgedRawInterruptsStayForwarded(t *testing.T) {
	compressInterruptStall(t)
	pipe, remote := newPipe(t)
	conn := &deliveryPipe{pipeConn: pipe}
	console := newFakeConsole()
	copied := make(chan struct{})
	s := New(Options{
		Conn: conn, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
		Console: console, RawMode: true, Kind: "test",
		InterruptNotice: func() { t.Error("warned about a host that was applying input") },
		CopyInput:       typedInterrupts(3, conn.applyAll, copied),
	})
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()

	readFrames(t, remote, 3)
	waitForClose(t, copied, "typed interrupts to finish")
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
}

// Interrupts typed faster than any answer could arrive are one keypress as far
// as the escape is concerned: nothing has had time to go unanswered.
func TestBurstedInterruptsDoNotEscape(t *testing.T) {
	// Not compressed: the point is that presses closer together than the stall
	// never count, so the window has to be wider than the test's own timing.
	previous := interruptStall
	interruptStall = time.Minute
	t.Cleanup(func() { interruptStall = previous })
	conn, remote := newPipe(t)
	console := newFakeConsole()
	s := New(Options{
		Conn: conn, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
		Console: console, Kind: "test",
		InterruptNotice: func() { t.Error("warned on a burst that could not have been answered yet") },
	})
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()

	for range 5 {
		console.deliver(t, os.Interrupt)
		readFrames(t, remote, 1)
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
}

// writerFunc reports each write to a test without the buffering a builder adds.
type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) {
	f(p)
	return len(p), nil
}

// typedInterrupts is a CopyInput that types count Ctrl-C keystrokes at the
// cadence the escape measures, running applied (when set) after each one as the
// host would. The stall is read here, on the test's own goroutine, so nothing
// the session runs races the cleanup that restores it; closing done lets a test
// wait for the goroutine it started rather than leaving it running past its
// cleanup.
func typedInterrupts(count int, applied func(), done chan struct{}) func(context.Context, *Session) error {
	stall := interruptStall
	return func(_ context.Context, s *Session) error {
		defer close(done)
		for i := range count {
			if i > 0 {
				time.Sleep(2 * stall)
			}
			if err := s.WriteFrame(frame.Input, []byte{interruptByte}); err != nil {
				return err
			}
			if applied != nil {
				applied()
			}
		}
		return nil
	}
}

// waitForClose blocks until done is closed, so a test never leaves a goroutine of
// its own running into cleanup.
func waitForClose(t *testing.T, done chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
