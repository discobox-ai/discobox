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

// ended waits for the attach to return, failing with what it did instead,
// and returns what it returned. resend, when set, is repeated until then: the
// session subscribes to signals only once it is running, and a signal that
// lands before that is the test's own guard's.
func (a *rawAttach) ended(t *testing.T, after string, resend func()) error {
	t.Helper()
	var err error
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	if resend != nil {
		resend()
	}
wait:
	for {
		select {
		case err = <-a.done:
			break wait
		case <-tick.C:
			if resend != nil {
				resend()
			}
			continue
		case <-deadline:
		}
		select {
		case name := <-a.forwarded:
			t.Fatalf("attach still running 3s after %s; it forwarded %s to the discobox's terminal instead of ending", after, name)
		default:
			t.Fatalf("attach still running 3s after %s", after)
		}
	}
	select {
	case name := <-a.forwarded:
		t.Fatalf("attach forwarded %s to the discobox's terminal; it was addressed to the attach", name)
	default:
	}
	return err
}

// SIGTERM is addressed to the attach (`timeout`, a script cleaning up), not
// to the program in the discobox: it ends the attach and leaves the discobox's
// terminal running.
func TestRawAttachEndsOnSIGTERMWithoutSignallingTheDiscobox(t *testing.T) {
	// The test's own subscription keeps a SIGTERM the attach does not catch
	// from killing the test binary.
	guard := make(chan os.Signal, 4)
	signal.Notify(guard, syscall.SIGTERM)
	defer signal.Stop(guard)

	a := startRawAttach(t)
	err := a.ended(t, "SIGTERM", func() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) })
	// It exits as a shell reports a command SIGTERM killed, and silently.
	if code, ok := ExitCode(err); !ok || code != 143 {
		t.Fatalf("attach = %v, want a silent exit 143", err)
	}
}

// A raw attach with no terminal has nothing more to send once its stdin is
// closed, and ends.
func TestRawAttachEndsWhenStdinCloses(t *testing.T) {
	a := startRawAttach(t)
	_ = a.stdin.Close()
	if err := a.ended(t, "stdin closed", nil); err != nil {
		t.Fatalf("attach = %v, want a detach", err)
	}
}
