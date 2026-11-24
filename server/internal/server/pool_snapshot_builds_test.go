package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/discobox-ai/discobox/pkg/execstream/frame"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// A build's logs are read at the canonical pool path, with the feature,
// count and follow asked for, and a refusal keeps its status.
func TestPoolSnapshotBuildLogsRouteForwardsTheRead(t *testing.T) {
	stubs := newRouterTestServices()
	stubs.snapshotBuildLog = "==> docker.log <==\ninstalling\n"
	server := newPoolLogsTestServer(t, stubs)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	get := func(path string) *http.Response {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/projects/"+testDefaultProjectID+"/pools/pool-1/snapshot-builds/"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := get("abc123/logs?feature=docker&tail=20&follow=true")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != stubs.snapshotBuildLog {
		t.Fatalf("logs = %d %q", resp.StatusCode, body)
	}
	if stubs.snapshotBuildKey != "abc123" || stubs.snapshotBuildLogs != (sandbox.SnapshotBuildLogOptions{Feature: "docker", Tail: 20, Follow: true}) {
		t.Fatalf("asked %q %+v", stubs.snapshotBuildKey, stubs.snapshotBuildLogs)
	}
	stubs.snapshotBuildErr = apperrors.NewStatusError(http.StatusConflict, "the snapshot build is queued and has no box yet")
	resp = get("abc123/logs")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "queued") {
		t.Fatalf("a queued build's logs = %d %q", resp.StatusCode, body)
	}
}

// An exec into a build is a WebSocket bridged to the pool's, its frames
// passed through as they are, and a refusal is sent as an error frame.
func TestPoolSnapshotBuildExecRouteBridgesFrames(t *testing.T) {
	stubs := newRouterTestServices()
	server := newPoolLogsTestServer(t, stubs)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dial := func(query string) io.ReadWriter {
		socket, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/projects/"+testDefaultProjectID+"/pools/pool-1/snapshot-builds/abc123/exec"+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		t.Cleanup(func() { _ = socket.CloseNow() })
		return websocket.NetConn(ctx, socket, websocket.MessageBinary)
	}
	conn := dial("?tty=true&rows=30&cols=100")
	if err := frame.Write(conn, frame.Input, []byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	next, err := frame.Read(conn)
	if err != nil || next.Type != frame.Input || string(next.Payload) != "ls\n" {
		t.Fatalf("echoed frame %d %q, %v", next.Type, next.Payload, err)
	}
	if stubs.snapshotBuildKey != "abc123" || !stubs.snapshotBuildExec.TTY || stubs.snapshotBuildExec.Rows != 30 || stubs.snapshotBuildExec.Cols != 100 {
		t.Fatalf("asked %q %+v", stubs.snapshotBuildKey, stubs.snapshotBuildExec)
	}

	stubs.mu.Lock()
	stubs.snapshotBuildErr = apperrors.NewStatusError(http.StatusNotFound, "no snapshot build of that key is under way on this pool")
	stubs.mu.Unlock()
	next, err = frame.Read(dial("?command=ls"))
	if err != nil || next.Type != frame.Error || !strings.Contains(string(next.Payload), "no snapshot build") {
		t.Fatalf("a refused exec: %d %q, %v", next.Type, next.Payload, err)
	}
}
