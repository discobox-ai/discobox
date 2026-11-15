//go:build !windows

package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/discobox-ai/discobox/pkg/execstream/frame"
	"github.com/discobox-ai/discobox/pkg/execstream/host"
)

// rawAttach is `discobox attach --raw` to a running discobox's primary
// terminal, with no terminal of its own: stdin is a pipe the test holds open,
// as `< <(sleep N)` does.
type rawAttach struct {
	stdin     *os.File
	done      chan error
	forwarded chan string
}

func startRawAttach(t *testing.T) *rawAttach {
	t.Helper()
	const sandboxID = "sbx_9qk5n25t2hh2rv00"
	execPath := "/api/projects/project-1/sandboxes/" + sandboxID + "/execs/primary"

	hostDone := make(chan struct{})
	t.Cleanup(func() { close(hostDone) })
	a := &rawAttach{done: make(chan error, 1), forwarded: make(chan string, 4)}
	stream := host.New(host.Options{
		Done: hostDone,
		OnFrame: func(next frame.Frame) error {
			if next.Type == frame.Signal {
				a.forwarded <- string(next.Payload)
			}
			return nil
		},
	})
	started := make(chan struct{}, 1)
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/sandboxes/"+sandboxID:
			_, _ = io.WriteString(w, runTestSandboxJSON(sandboxID, "running"))
		case r.Method == http.MethodGet && r.URL.Path == execPath+"/attach":
			socket, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			conn := websocket.NetConn(context.Background(), socket, websocket.MessageBinary)
			defer conn.Close()
			stream.Broadcast(frame.Stdout, []byte("screen"))
			_ = stream.Attach(context.Background(), &directAttachFrames{conn: conn}, host.AttachOptions{})
		case r.Method == http.MethodPost && r.URL.Path == execPath+"/start":
			_, _ = io.WriteString(w, `{"id":"ex_primaryterminal000","primary":true,"status":"running","command":["codex"],"workdir":"/workspace","tty":true,"createdAt":"2026-01-01T00:00:00Z"}`)
			started <- struct{}{}
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
		}
	}))
	t.Cleanup(server.Close)

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdinR.Close(); _ = stdinW.Close() })
	a.stdin = stdinW
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		a.done <- app.attachSandboxTerminal(ctx, "project-1", sandboxID, primaryExecID, execAttachOptions{detachAtInputEnd: true}, stdinR, io.Discard, io.Discard)
	}()
	select {
	case <-started:
	case err := <-a.done:
		t.Fatalf("attach ended before it attached: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("attach never reached the terminal")
	}
	return a
}

// ended waits for the attach to return, and returns what it returned.
func (a *rawAttach) ended(t *testing.T, after string) error {
	t.Helper()
	select {
	case err := <-a.done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatalf("attach still running 3s after %s", after)
		return nil
	}
}

// SIGTERM sent to a raw attach (`timeout`, a script cleaning up) is passed on
// to the discobox's terminal, as Ctrl-C and the rest are, where the
// sandbox-agent delivers it to the program in front.
func TestRawAttachForwardsSIGTERMToTheDiscobox(t *testing.T) {
	// The test's own subscription keeps a SIGTERM that lands before the
	// attach subscribes from killing the test binary.
	guard := make(chan os.Signal, 4)
	signal.Notify(guard, syscall.SIGTERM)
	defer signal.Stop(guard)

	a := startRawAttach(t)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(3 * time.Second)
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
wait:
	for {
		select {
		case name := <-a.forwarded:
			if name != "TERM" {
				t.Fatalf("forwarded %s, want TERM", name)
			}
			break wait
		case err := <-a.done:
			t.Fatalf("attach ended on SIGTERM (%v); it should pass it on", err)
		case <-tick.C:
			// The session subscribes to signals once it is running.
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		case <-deadline:
			t.Fatal("SIGTERM never reached the discobox")
		}
	}

	_ = a.stdin.Close()
	if err := a.ended(t, "stdin closed"); err != nil {
		t.Fatalf("attach = %v, want a detach", err)
	}
}

// A raw attach with no terminal has nothing more to send once its stdin is
// closed, and ends, sending the discobox nothing.
func TestRawAttachEndsWhenStdinCloses(t *testing.T) {
	a := startRawAttach(t)
	_ = a.stdin.Close()
	if err := a.ended(t, "stdin closed"); err != nil {
		t.Fatalf("attach = %v, want a detach", err)
	}
	select {
	case name := <-a.forwarded:
		t.Fatalf("forwarded %s to the discobox's terminal", name)
	default:
	}
}
