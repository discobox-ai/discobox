package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/obot-platform/discobox/execstream/frame"
	"github.com/obot-platform/discobox/execstream/host"
	"github.com/obot-platform/discobox/execstream/resume"
)

// This test crosses the actual CLI websocket boundary. It deliberately drops
// the first upgraded connection, accepts input while the replacement handshake
// is held off, and verifies the shared host applies and echoes that input once.
func TestOpenReconnectingSandboxExecAttachPreservesInputAcrossWebSocketReplacement(t *testing.T) {
	const execPath = "/api/projects/project-1/sandboxes/sandbox-1/execs/exec-1"

	hostDone := make(chan struct{})
	applied := make(chan []byte, 4)
	var stream *host.Stream
	stream = host.New(host.Options{
		Done: hostDone,
		OnFrame: func(next frame.Frame) error {
			if next.Type != frame.Input {
				return fmt.Errorf("unexpected applied frame type %d", next.Type)
			}
			payload := append([]byte(nil), next.Payload...)
			applied <- payload
			// Produce output before Apply returns. This exercises output racing
			// ahead of the acknowledgement during the reconnect handshake.
			stream.Broadcast(frame.Stdout, append([]byte("echo:"), payload...))
			return nil
		},
	})

	allowReplacement := make(chan struct{})
	accepted := make(chan net.Conn, 2)
	replayRequests := make(chan bool, 2)
	serverErrors := make(chan error, 4)
	var attachCount atomic.Int32
	var handlerWG sync.WaitGroup
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == execPath:
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(w, `{"id":"exec-1","status":"running","command":["cat"],"workdir":"/","tty":true,"createdAt":"2026-07-23T00:00:00Z"}`)
			if err != nil {
				serverErrors <- err
			}
		case r.Method == http.MethodGet && r.URL.Path == execPath+"/attach":
			replayRequests <- r.URL.Query().Get("replay") == "true"
			if attachCount.Add(1) > 1 {
				select {
				case <-allowReplacement:
				case <-r.Context().Done():
					return
				}
			}
			socket, err := websocket.Accept(w, r, nil)
			if err != nil {
				serverErrors <- err
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
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	app := &App{serverURL: server.URL, noStart: true}
	events := make(chan resume.Event, 4)
	conn, err := app.openReconnectingSandboxExecAttach(ctx, "project-1", "sandbox-1", "exec-1", true, func(event resume.Event) {
		events <- event
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = conn.Close()
		close(hostDone)
		handlerWG.Wait()
	}()

	firstPhysical := <-accepted
	readResult := make(chan frame.Frame, 1)
	readError := make(chan error, 1)
	go func() {
		next, err := conn.ReadFrame()
		if err != nil {
			readError <- err
			return
		}
		readResult <- next
	}()
	if err := firstPhysical.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case event := <-events:
		if event.State != resume.ConnectionReconnecting {
			t.Fatalf("first connection event = %q, want reconnecting", event.State)
		}
	case <-ctx.Done():
		t.Fatal("client did not notice websocket loss")
	}
	if err := conn.WriteFrame(frame.Input, []byte("during-outage")); err != nil {
		t.Fatalf("write while disconnected: %v", err)
	}
	close(allowReplacement)

	select {
	case next := <-readResult:
		if next.Type != frame.Stdout || string(next.Payload) != "echo:during-outage" {
			t.Fatalf("reconnected output = %#v", next)
		}
	case err := <-readError:
		t.Fatalf("read after reconnect: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for reconnected output")
	}
	if got := string(<-applied); got != "during-outage" {
		t.Fatalf("applied input = %q, want during-outage", got)
	}
	select {
	case duplicate := <-applied:
		t.Fatalf("input was applied more than once: %q", duplicate)
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case event := <-events:
		if event.State != resume.ConnectionReconnected {
			t.Fatalf("second connection event = %q, want reconnected", event.State)
		}
	case <-ctx.Done():
		t.Fatal("client did not report websocket reconnection")
	}
	for range 2 {
		if replay := <-replayRequests; !replay {
			t.Fatal("websocket replacement did not request terminal replay")
		}
	}
	select {
	case err := <-serverErrors:
		t.Fatalf("websocket test server: %v", err)
	default:
	}

}

func TestAttachWebSocketKeepaliveClosesUnresponsivePeerAndUnblocksRead(t *testing.T) {
	releaseServer := make(chan struct{})
	serverSocket := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		serverSocket <- socket
		// Deliberately never read: control frames are not processed and the
		// client's Ping cannot receive a Pong.
		<-releaseServer
		_ = socket.CloseNow()
	}))
	defer server.Close()
	defer close(releaseServer)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	socket, response, err := websocket.Dial(ctx, "ws"+server.URL[len("http"):], nil)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer socket.CloseNow()
	<-serverSocket
	conn := websocket.NetConn(ctx, socket, websocket.MessageBinary)

	readDone := make(chan error, 1)
	go func() {
		var payload [1]byte
		_, err := conn.Read(payload[:])
		readDone <- err
	}()
	pingDone := make(chan error, 1)
	go func() {
		err := pingAttachWebSocketWithIntervals(ctx, socket, 10*time.Millisecond, 30*time.Millisecond)
		if err != nil {
			_ = conn.Close()
		}
		pingDone <- err
	}()

	select {
	case err := <-pingDone:
		if err == nil {
			t.Fatal("keepalive returned nil for an unresponsive websocket peer")
		}
	case <-ctx.Done():
		t.Fatal("keepalive did not detect the unresponsive websocket peer")
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("blocked websocket read returned nil after keepalive failure")
		}
	case <-ctx.Done():
		t.Fatal("keepalive failure did not unblock the websocket read")
	}
}
