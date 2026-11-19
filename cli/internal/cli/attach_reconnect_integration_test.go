package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/discobox-ai/discobox/pkg/execstream/frame"
	"github.com/discobox-ai/discobox/pkg/execstream/host"
	"github.com/discobox-ai/discobox/pkg/execstream/resume"
)

// This test crosses the actual CLI websocket boundary. It deliberately drops
// the first upgraded connection, accepts input while the replacement handshake
// is held off, and verifies the shared host applies and echoes that input once.
func TestOpenReconnectingSandboxExecAttachPreservesInputAcrossWebSocketReplacement(t *testing.T) {
	applied := make(chan []byte, 4)
	server := newFakeExecAttachServer(t, func(stream *host.Stream, next frame.Frame) error {
		if next.Type != frame.Input {
			return fmt.Errorf("unexpected applied frame type %d", next.Type)
		}
		payload := append([]byte(nil), next.Payload...)
		applied <- payload
		// Produce output before Apply returns. This exercises output racing
		// ahead of the acknowledgement during the reconnect handshake.
		stream.Broadcast(frame.Stdout, append([]byte("echo:"), payload...))
		return nil
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	app := &App{serverURL: server.url, autoStart: autoStartServerFalse}
	events := make(chan resume.Event, 4)
	timings := make(chan resume.TimingEvent, 16)
	conn, err := app.openReconnectingSandboxExecAttach(ctx, "project-1", "sandbox-1", "exec-1", execAttachOptions{replay: true, event: func(event resume.Event) {
		events <- event
	}, timing: resume.TimingOptions{
		Observe:           func(event resume.TimingEvent) { timings <- event },
		HeartbeatInterval: 10 * time.Millisecond,
		HeartbeatTimeout:  time.Second,
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Registered after the server's cleanup, so it runs first: the client
	// stops reconnecting before the server is closed and waited for.
	t.Cleanup(func() { _ = conn.Close() })

	firstPhysical := <-server.accepted
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
	server.allowReplacements()

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
	replays := server.attachReplays()
	if len(replays) < 2 {
		t.Fatalf("attach requests = %d, want the first and its replacement", len(replays))
	}
	for _, replay := range replays[:2] {
		if !replay {
			t.Fatal("websocket replacement did not request terminal replay")
		}
	}
	if err := server.err(); err != nil {
		t.Fatalf("websocket test server: %v", err)
	}

	// Keep the websocket reader active while Ping waits for its Pong. Real
	// attach sessions continuously read output; this extra read recreates that
	// steady state after readResult consumed the first output frame.
	heartbeatReadDone := make(chan error, 1)
	go func() {
		_, err := conn.ReadFrame()
		heartbeatReadDone <- err
	}()
	var gotHeartbeat, gotInputAcknowledgement bool
	timingDeadline := time.After(time.Second)
	for !gotHeartbeat || !gotInputAcknowledgement {
		select {
		case event := <-timings:
			switch event.Source {
			case resume.TimingHeartbeat:
				gotHeartbeat = event.Err == nil && event.RoundTrip > 0
			case resume.TimingActionAcknowledgement:
				gotInputAcknowledgement = event.Input() && event.RoundTrip > 0
			}
		case <-timingDeadline:
			t.Fatalf("timing events: heartbeat=%t input acknowledgement=%t", gotHeartbeat, gotInputAcknowledgement)
		}
	}
}

func TestAttachWebSocketKeepaliveClosesUnresponsivePeerAndUnblocksRead(t *testing.T) {
	releaseServer := make(chan struct{})
	serverSocket := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
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
	frames := newWebsocketAttachFrames(ctx, socket)

	readDone := make(chan error, 1)
	go func() {
		_, err := frames.ReadFrame()
		readDone <- err
	}()
	pingDone := make(chan error, 1)
	go func() {
		err := frames.keepalive(ctx, 10*time.Millisecond, 30*time.Millisecond)
		if err != nil {
			_ = frames.Close()
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

// A paused stdout consumer stops the attach session from reading frames. The
// server is alive and answering pings the whole time, so the keepalive must not
// call it dead just because the client is not reading.
func TestAttachWebSocketKeepaliveSurvivesPausedReader(t *testing.T) {
	releaseServer := make(chan struct{})
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		socket, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// The server reads continuously, so it answers every ping.
		ctx := socket.CloseRead(r.Context())
		// Output is waiting for the client, as an exec's would be.
		_ = socket.Write(ctx, websocket.MessageBinary, []byte("output"))
		select {
		case <-releaseServer:
		case <-ctx.Done():
		}
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
	frames := newWebsocketAttachFrames(ctx, socket)

	// No one reads the client's socket: the output consumer is paused.
	pingCtx, stopPing := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stopPing()
	if err := frames.keepalive(pingCtx, 10*time.Millisecond, 30*time.Millisecond); err != nil {
		t.Fatalf("keepalive declared a live peer dead while output delivery was paused: %v", err)
	}
}
