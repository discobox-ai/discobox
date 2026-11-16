package cli

import (
	"context"
	"testing"
	"time"
)

// openExecAttachConn is what attachSandboxExec calls to get its connection. A
// TTY exec gets the same reconnecting, replay transport a terminal attach
// uses, so a dropped websocket is recovered instead of ending the
// attach: this drives the physical connection through a real host.Stream so
// the resumable session handshake actually completes, then drops it and
// checks for a second /attach request.
func TestOpenExecAttachConnReconnectsForTTY(t *testing.T) {
	server := newFakeExecAttachServer(t, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	app := &App{serverURL: server.url, autoStart: autoStartServerFalse}
	conn, err := app.openExecAttachConn(ctx, "project-1", "sandbox-1", "exec-1", true)
	if err != nil {
		t.Fatalf("openExecAttachConn: %v", err)
	}
	// Registered after the server's cleanup, so it runs first: the client
	// stops reconnecting before the server is closed and waited for.
	t.Cleanup(func() { _ = conn.Close() })

	first := <-server.accepted
	readErr := make(chan error, 1)
	go func() {
		_, err := conn.ReadFrame()
		readErr <- err
	}()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	server.allowReplacements()

	select {
	case <-server.accepted:
		// A second /attach request proves the dropped connection was recovered
		// instead of ending the attach.
	case err := <-readErr:
		t.Fatalf("attach ended instead of reconnecting: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for a reconnect attempt")
	}
}

// A non-TTY exec has no screen to replay, so it keeps the direct,
// fail-on-disconnect transport: no session handshake, and a dropped
// connection ends the attach rather than triggering a reconnect.
func TestOpenExecAttachConnDoesNotReconnectForNonTTY(t *testing.T) {
	server := newFakeExecAttachServer(t, nil)
	// A second /attach is let straight through, so one would show up on
	// accepted rather than wait unseen.
	server.allowReplacements()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	app := &App{serverURL: server.url, autoStart: autoStartServerFalse}
	conn, err := app.openExecAttachConn(ctx, "project-1", "sandbox-1", "exec-1", false)
	if err != nil {
		t.Fatalf("openExecAttachConn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	readErr := make(chan error, 1)
	go func() {
		_, err := conn.ReadFrame()
		readErr <- err
	}()

	first := <-server.accepted
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("ReadFrame = nil after the connection dropped, want an error")
		}
	case <-server.accepted:
		t.Fatal("a second /attach request arrived; non-TTY execs must not reconnect")
	case <-ctx.Done():
		t.Fatal("timed out waiting for the direct attach to fail")
	}
	if got := len(server.attachReplays()); got != 1 {
		t.Fatalf("attach requests = %d, want exactly 1", got)
	}
}
