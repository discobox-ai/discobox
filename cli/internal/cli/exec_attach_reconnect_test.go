package cli

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/discobox-ai/discobox/execstream/frame"
	"github.com/discobox-ai/discobox/execstream/host"
)

// openExecAttachConn is what attachSandboxExec calls to get its connection. A
// TTY exec now gets the same reconnecting, replay transport a terminal attach
// already uses, so a dropped websocket is recovered instead of ending the
// attach: this drives the physical connection through a real host.Stream so
// the resumable session handshake actually completes, then drops it and
// checks for a second /attach request.
func TestOpenExecAttachConnReconnectsForTTY(t *testing.T) {
	const execPath = "/api/projects/project-1/sandboxes/sandbox-1/execs/exec-1"

	hostDone := make(chan struct{})
	stream := host.New(host.Options{
		Done:    hostDone,
		OnFrame: func(frame.Frame) error { return nil },
	})

	var attachCount atomic.Int32
	accepted := make(chan net.Conn, 2)
	allowReplacement := make(chan struct{})
	var handlerWG sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == execPath:
			// Consulted by the reconnect-or-stop decision between dials.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"exec-1","status":"running","command":["cat"],"workdir":"/","tty":true,"createdAt":"2026-07-23T00:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == execPath+"/attach":
			if attachCount.Add(1) > 1 {
				select {
				case <-allowReplacement:
				case <-r.Context().Done():
					return
				}
			}
			socket, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			conn := websocket.NetConn(context.Background(), socket, websocket.MessageBinary)
			accepted <- conn
			handlerWG.Add(1)
			defer handlerWG.Done()
			defer conn.Close()
			_ = stream.Attach(context.Background(), &directAttachFrames{conn: conn}, host.AttachOptions{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	conn, err := app.openExecAttachConn(ctx, "project-1", "sandbox-1", "exec-1", true)
	if err != nil {
		t.Fatalf("openExecAttachConn: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		close(hostDone)
		handlerWG.Wait()
	})

	first := <-accepted
	readErr := make(chan error, 1)
	go func() {
		_, err := conn.ReadFrame()
		readErr <- err
	}()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	close(allowReplacement)

	select {
	case <-accepted:
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
	const execPath = "/api/projects/project-1/sandboxes/sandbox-1/execs/exec-1"

	var attachCount atomic.Int32
	accepted := make(chan net.Conn, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != execPath+"/attach" {
			http.NotFound(w, r)
			return
		}
		attachCount.Add(1)
		socket, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn := websocket.NetConn(context.Background(), socket, websocket.MessageBinary)
		accepted <- conn
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
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

	first := <-accepted
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("ReadFrame = nil after the connection dropped, want an error")
		}
	case <-accepted:
		t.Fatal("a second /attach request arrived; non-TTY execs must not reconnect")
	case <-ctx.Done():
		t.Fatal("timed out waiting for the direct attach to fail")
	}
	if got := attachCount.Load(); got != 1 {
		t.Fatalf("attach requests = %d, want exactly 1", got)
	}
}
