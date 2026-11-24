package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/execstream/frame"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// snapshotBuildRuntime is a pool with one build under way, keyed "k1", that
// answers logs with what it was asked and runs an exec as an echo.
type snapshotBuildRuntime struct {
	*sandboxruntime.MemorySandboxRuntime
	logs  sandboxruntime.SnapshotLogsRequest
	exec  machine.ExecRequest
	ended context.Context
	end   context.CancelFunc
}

func (r *snapshotBuildRuntime) SnapshotBuildLogs(_ context.Context, key string, req sandboxruntime.SnapshotLogsRequest, w io.Writer) error {
	if key != "k1" {
		return sandboxruntime.ErrSnapshotBuildNotFound
	}
	r.logs = req
	_, err := fmt.Fprintf(w, "log of %s\n", req.Feature)
	return err
}

func (r *snapshotBuildRuntime) SnapshotBuildExec(_ context.Context, key string, req machine.ExecRequest) (machine.Process, context.Context, error) {
	if key != "k1" {
		return nil, nil, sandboxruntime.ErrSnapshotBuildQueued
	}
	r.exec = req
	reader, writer := io.Pipe()
	return &echoProcess{in: writer, out: reader}, r.ended, nil
}

// echoProcess writes back what it reads, and exits 3 at the end of its input.
type echoProcess struct {
	in  *io.PipeWriter
	out *io.PipeReader
}

func (p *echoProcess) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *echoProcess) CloseStdin() error           { return p.in.Close() }
func (p *echoProcess) Resize(uint16, uint16) error { return nil }
func (p *echoProcess) Close() error {
	_ = p.in.CloseWithError(io.ErrClosedPipe)
	return p.out.Close()
}

func (p *echoProcess) Wait(stdout, _ io.Writer) (int, error) {
	if _, err := io.Copy(stdout, p.out); err != nil {
		return -1, err
	}
	return 3, nil
}

func newSnapshotBuildRouter(t *testing.T) (*snapshotBuildRuntime, http.Handler, func(projectID, poolID, sandboxID string, scopes ...string) string) {
	t.Helper()
	publicKey, sign := testPoolTokenSigner(t)
	ended, end := context.WithCancel(context.Background())
	t.Cleanup(end)
	runtime := &snapshotBuildRuntime{MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(), ended: ended, end: end}
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: "project-1", PoolID: "pool-1"},
		Runtime:               runtime,
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime, router, sign
}

// A build's logs need snapshot-build:read, and stream as text what the
// runtime reads, with the feature, count and follow asked for.
func TestASnapshotBuildsLogsAreReadWithTheirScope(t *testing.T) {
	runtime, router, sign := newSnapshotBuildRouter(t)
	get := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/project/project-1/pool/pool-1/snapshot-builds/"+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		return resp
	}
	if resp := get("k1/logs", sign("project-1", "pool-1", "", ScopeSnapshotBuildExec)); resp.Code != http.StatusForbidden {
		t.Fatalf("without snapshot-build:read: %d %s", resp.Code, resp.Body)
	}
	read := sign("project-1", "pool-1", "", ScopeSnapshotBuildRead)
	resp := get("k1/logs?feature=docker&tail=5&follow=true", read)
	if resp.Code != http.StatusOK || resp.Body.String() != "log of docker\n" || !strings.HasPrefix(resp.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("logs = %d %q %q", resp.Code, resp.Body, resp.Header().Get("Content-Type"))
	}
	if runtime.logs != (sandboxruntime.SnapshotLogsRequest{Feature: "docker", Lines: 5, Follow: true}) {
		t.Fatalf("the runtime was asked %+v", runtime.logs)
	}
	if resp := get("k2/logs", read); resp.Code != http.StatusNotFound || !strings.Contains(resp.Body.String(), "no snapshot build") {
		t.Fatalf("logs of no build: %d %s", resp.Code, resp.Body)
	}
	if resp := get("k1/logs?tail=lots", read); resp.Code != http.StatusBadRequest {
		t.Fatalf("a tail that is no count: %d %s", resp.Code, resp.Body)
	}
}

// An exec into a build needs snapshot-build:exec, carries execstream frames
// both ways, and ends with the command's exit; one into a build with no box
// is refused on the stream.
func TestAnExecIntoASnapshotBuildCarriesFramesWithItsScope(t *testing.T) {
	runtime, router, sign := newSnapshotBuildRouter(t)
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// dial opens path, and is the socket and the status the upgrade got.
	dial := func(path, token string) (*websocket.Conn, int, error) {
		socket, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/project/project-1/pool/pool-1/snapshot-builds/"+path,
			&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
		status := 0
		if resp != nil {
			status = resp.StatusCode
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
		}
		return socket, status, err
	}
	if _, status, err := dial("k1/exec", sign("project-1", "pool-1", "", ScopeSnapshotBuildRead)); err == nil || status != http.StatusForbidden {
		t.Fatalf("without snapshot-build:exec: %d %v", status, err)
	}
	execToken := sign("project-1", "pool-1", "", ScopeSnapshotBuildExec)
	socket, _, err := dial("k1/exec?command=cat&command=-u", execToken)
	if err != nil {
		t.Fatal(err)
	}
	conn := websocket.NetConn(ctx, socket, websocket.MessageBinary)
	if err := frame.Write(conn, frame.Input, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := frame.Write(conn, frame.CloseInput, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	var exit frame.ExitPayload
	for {
		next, err := frame.Read(conn)
		if err != nil {
			t.Fatalf("read: %v (output %q)", err, out.String())
		}
		if next.Type == frame.Stdout {
			out.Write(next.Payload)
			continue
		}
		if next.Type != frame.Exit {
			t.Fatalf("frame %d %q", next.Type, next.Payload)
		}
		if exit, err = frame.DecodeExit(next.Payload); err != nil {
			t.Fatal(err)
		}
		break
	}
	if out.String() != "hello" || exit.Status != "exited" || exit.ExitCode == nil || *exit.ExitCode != 3 {
		t.Fatalf("output %q, exit %+v", out.String(), exit)
	}
	if strings.Join(runtime.exec.Argv, " ") != "cat -u" || runtime.exec.TTY {
		t.Fatalf("the runtime ran %+v", runtime.exec)
	}

	socket, _, err = dial("k2/exec", execToken)
	if err != nil {
		t.Fatal(err)
	}
	next, err := frame.Read(websocket.NetConn(ctx, socket, websocket.MessageBinary))
	if err != nil || next.Type != frame.Error || !strings.Contains(string(next.Payload), "queued") {
		t.Fatalf("exec into a queued build: %d %q %v", next.Type, next.Payload, err)
	}
}

// A shell in a build ends when the build does, saying so.
func TestAnExecIntoASnapshotBuildEndsWithTheBuild(t *testing.T) {
	runtime, router, sign := newSnapshotBuildRouter(t)
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	socket, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/project/project-1/pool/pool-1/snapshot-builds/k1/exec?tty=true&rows=24&cols=80",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + sign("project-1", "pool-1", "", ScopeSnapshotBuildExec)}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	conn := websocket.NetConn(ctx, socket, websocket.MessageBinary)
	if err := frame.Write(conn, frame.Input, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if next, err := frame.Read(conn); err != nil || next.Type != frame.Stdout {
		t.Fatalf("first frame %d %v", next.Type, err)
	}
	if !runtime.exec.TTY || runtime.exec.Rows != 24 || runtime.exec.Cols != 80 || len(runtime.exec.Argv) == 0 {
		t.Fatalf("the runtime ran %+v, want a shell on a 24x80 terminal", runtime.exec)
	}
	runtime.end()
	for {
		next, err := frame.Read(conn)
		if err != nil {
			t.Fatal(err)
		}
		if next.Type == frame.Exit {
			exit, _ := frame.DecodeExit(next.Payload)
			if exit.Status != "failed" || !strings.Contains(exit.Error, "build ended") {
				t.Fatalf("exit = %+v", exit)
			}
			return
		}
	}
}
