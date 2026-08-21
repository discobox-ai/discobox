//go:build !windows

package client

import (
	"context"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/execstream/frame"
)

// Ctrl-Z has to do four things in one order: stop the remote job, hand the
// terminal back in the mode the shell expects, stop this process, then take the
// terminal again and resume the remote. Verifying this used to require a real
// PTY, a suspended process, and ps.
func TestSuspendOrdersRemoteStopTerminalHandoverAndResume(t *testing.T) {
	conn, remote := newPipe(t)
	console := newFakeConsole()
	// Resize is off here so the size watcher's own frames cannot interleave with
	// the two signals whose order is under test; TestResizeIsResentAfterResume
	// covers the resize.
	s := New(Options{
		Conn: conn, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
		Console: console, RawMode: true, Kind: "test",
	})
	go func() { _ = s.Run(context.Background()) }()

	console.deliver(t, testSuspendSignal)

	// The remote is told to stop before this process does.
	got := readFrames(t, remote, 1)
	if len(got) != 1 || got[0].Type != frame.Signal || string(got[0].Payload) != "TSTP" {
		t.Fatalf("first frame = %+v, want a TSTP signal", got)
	}
	waitFor(t, console, "suspend")
	if events := console.Events(); !ordered(events, "makeraw", "restore", "suspend") {
		t.Fatalf("events = %v, want the terminal restored before suspending", events)
	}

	console.resume <- struct{}{}

	after := readFrames(t, remote, 1)
	if len(after) != 1 || string(after[0].Payload) != "CONT" {
		t.Fatalf("frame after resume = %+v, want CONT", after)
	}
	waitFor(t, console, "resumed")
	if events := console.Events(); !ordered(events, "restore", "suspend", "resumed", "makeraw") {
		t.Fatalf("events = %v, want the terminal retaken after resuming", events)
	}
}

// The terminal may have been resized while this process was stopped, so the
// remote is told the current size once the session resumes.
func TestResizeIsResentAfterResume(t *testing.T) {
	conn, remote := newPipe(t)
	console := newFakeConsole()
	s := New(Options{
		Conn: conn, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
		Console: console, RawMode: true, Resize: true, Kind: "test",
	})
	go func() { _ = s.Run(context.Background()) }()

	// Read continuously from the start: writes share one mutex, so leaving the
	// size watcher's first frame unread would block the suspend's frames too.
	frames := make(chan frame.Frame, 16)
	go func() {
		for {
			f, err := frame.Read(remote)
			if err != nil {
				close(frames)
				return
			}
			frames <- f
		}
	}()

	console.deliver(t, testSuspendSignal)
	waitFor(t, console, "suspend")
	console.resume <- struct{}{}

	sawCont := false
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("stream ended before a resize followed the resume")
			}
			if f.Type == frame.Signal && string(f.Payload) == "CONT" {
				sawCont = true
				continue
			}
			if sawCont && f.Type == frame.Resize {
				return
			}
		case <-deadline:
			t.Fatal("no resize was sent after the session resumed")
		}
	}
}

// A signal the console does not name is dropped rather than sent as an empty
// frame the remote would have to interpret.
func TestUnnamedSignalsAreDropped(t *testing.T) {
	conn, remote := newPipe(t)
	console := newFakeConsole()
	s := New(Options{
		Conn: conn, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
		Console: console, Kind: "test",
	})
	go func() { _ = s.Run(context.Background()) }()

	console.deliver(t, testUnnamedSignal) // unnamed by the fake
	console.deliver(t, syscall.SIGTERM)

	got := readFrames(t, remote, 1)
	if len(got) != 1 || string(got[0].Payload) != "TERM" {
		t.Fatalf("frames = %+v, want only the named signal", got)
	}
}
