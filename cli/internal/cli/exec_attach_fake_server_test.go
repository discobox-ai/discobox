package cli

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/discobox-ai/discobox/pkg/execstream/frame"
	"github.com/discobox-ai/discobox/pkg/execstream/host"
)

// fakeExecPath is the exec a fakeExecAttachServer serves.
const fakeExecPath = "/api/projects/project-1/sandboxes/sandbox-1/execs/exec-1"

// fakeExecAttachServer is a running exec's API as the CLI's attach sees it:
// the exec's status and its /attach websocket, served by a real host.Stream
// so a resumable attach's session handshake completes, and a direct one is
// served as the sandbox-agent serves it. The status says tty, which only the
// reconnecting attach reads. The first
// /attach is accepted at once; every later one, a replacement after a drop,
// waits for allowReplacements.
//
// It owns its handlers' lifetime: its cleanup stops the gated ones, closes the
// server (which waits for every request not yet hijacked), ends the host
// stream (which ends the attached ones), and waits for those, so no handler
// outlives the test: one still running after that fails the test. A test's
// own cleanup that closes its client, registered
// after this server is made, runs first.
type fakeExecAttachServer struct {
	url string
	// accepted receives each attach's server side once it is upgraded, so a
	// test can drop it.
	accepted chan net.Conn

	allowReplacement chan struct{}
	allowOnce        sync.Once
	errs             chan error

	mu      sync.Mutex
	replays []bool
}

// newFakeExecAttachServer starts the server. onFrame applies a frame the
// client sends, as the exec's process would; nil discards them.
func newFakeExecAttachServer(t *testing.T, onFrame func(stream *host.Stream, next frame.Frame) error) *fakeExecAttachServer {
	t.Helper()
	s := &fakeExecAttachServer{
		accepted:         make(chan net.Conn, 4),
		allowReplacement: make(chan struct{}),
		errs:             make(chan error, 8),
	}
	hostDone := make(chan struct{})
	stopping := make(chan struct{})
	var stream *host.Stream
	stream = host.New(host.Options{
		Done: hostDone,
		OnFrame: func(next frame.Frame) error {
			if onFrame == nil {
				return nil
			}
			return onFrame(stream, next)
		},
	})

	var handlerWG sync.WaitGroup
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == fakeExecPath:
			// Consulted by the reconnect-or-stop decision between dials.
			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, `{"id":"exec-1","status":"running","command":["cat"],"workdir":"/","tty":true,"createdAt":"2026-07-23T00:00:00Z"}`); err != nil {
				s.fail(err)
			}
		case r.Method == http.MethodGet && r.URL.Path == fakeExecPath+"/attach":
			if s.recordAttach(r) > 1 {
				select {
				case <-s.allowReplacement:
				case <-stopping:
					return
				case <-r.Context().Done():
					return
				}
			}
			// Counted before the websocket hijacks the connection: until then
			// server.Close waits for this request, and after it this Add has
			// already happened, so the cleanup's Wait never races an Add.
			handlerWG.Add(1)
			defer handlerWG.Done()
			socket, err := websocket.Accept(w, r, nil)
			if err != nil {
				s.fail(err)
				return
			}
			conn := websocket.NetConn(context.Background(), socket, websocket.MessageBinary)
			defer conn.Close()
			select {
			case s.accepted <- conn:
			default:
			}
			_ = stream.Attach(context.Background(), &directAttachFrames{conn: conn}, host.AttachOptions{})
		default:
			http.NotFound(w, r)
		}
	}))
	s.url = server.URL
	t.Cleanup(func() {
		close(stopping)
		server.Close()
		close(hostDone)
		handlersDone := make(chan struct{})
		go func() {
			handlerWG.Wait()
			close(handlersDone)
		}()
		select {
		case <-handlersDone:
		case <-time.After(5 * time.Second):
			t.Error("an /attach handler is still running 5s after the host stream ended")
		}
	})
	return s
}

// allowReplacements lets every /attach after the first one through.
func (s *fakeExecAttachServer) allowReplacements() {
	s.allowOnce.Do(func() { close(s.allowReplacement) })
}

// attachReplays reports, for each /attach so far in order, whether it asked
// for the terminal to be replayed.
func (s *fakeExecAttachServer) attachReplays() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.replays...)
}

// err returns an error the server met, if any.
func (s *fakeExecAttachServer) err() error {
	select {
	case err := <-s.errs:
		return err
	default:
		return nil
	}
}

func (s *fakeExecAttachServer) recordAttach(r *http.Request) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replays = append(s.replays, r.URL.Query().Get("replay") == "true")
	return len(s.replays)
}

func (s *fakeExecAttachServer) fail(err error) {
	select {
	case s.errs <- err:
	default:
	}
}
