package hooks

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/store"
	"github.com/discobox-ai/x/shorttmp"
)

func TestPublishRecordsHarnessHook(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := &recordingRecorder{done: make(chan struct{})}
	socketPath := filepath.Join(shorttmp.Dir(t), "hooks.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(ctx, socketPath, recorder)
	}()
	waitForSocket(t, socketPath)
	assertPermissions(t, filepath.Dir(socketPath), socketDirMode)
	assertPermissions(t, socketPath, socketMode)

	err := Publish(context.Background(), socketPath, Message{
		TerminalID: "agt_1",
		Provider:   "codex",
		Event:      "Stop",
		Payload:    json.RawMessage(`{"stop":true}`),
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-recorder.done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for record")
	}
	if recorder.record.TerminalID != "agt_1" || recorder.record.Provider != "codex" || recorder.record.Event != "Stop" {
		t.Fatalf("record = %#v", recorder.record)
	}
	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("serve: %v", err)
	}
}

// The reply is the line the collector writes, not everything up to its close:
// on Windows a read pending while its peer closes a Unix socket can stall
// instead of returning EOF (go.dev/issue/49352), which timed Publish out with
// "ok" already sent (#475). A collector that answers and holds the connection
// open makes that reliance fail on every platform.
func TestPublishReadsTheReplyLineWithoutWaitingForClose(t *testing.T) {
	socketPath := filepath.Join(shorttmp.Dir(t), "hooks.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	release := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			served <- err
			return
		}
		defer conn.Close()
		if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
			served <- err
			return
		}
		_, err = io.WriteString(conn, replyOK+"\n")
		served <- err
		<-release
	}()
	defer close(release)

	start := time.Now()
	if err := Publish(context.Background(), socketPath, Message{Provider: "codex", Event: "Stop"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("publish took %s: it waited for the collector to close", elapsed)
	}
	if err := <-served; err != nil {
		t.Fatalf("collector: %v", err)
	}
}

func TestPublishReturnsTheCollectorsWholeError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socketPath := filepath.Join(shorttmp.Dir(t), "hooks.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(ctx, socketPath, failingRecorder{err: errors.New("store closed\nretry later")})
	}()
	waitForSocket(t, socketPath)

	err := Publish(context.Background(), socketPath, Message{Provider: "codex", Event: "Stop"})
	if err == nil || err.Error() != "publish hook: store closed retry later" {
		t.Fatalf("publish error = %v, want the recorder's error on one line", err)
	}
	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("serve: %v", err)
	}
}

type failingRecorder struct{ err error }

func (r failingRecorder) RecordHarnessHook(context.Context, store.HarnessHookRecord) (store.HarnessHookRecord, error) {
	return store.HarnessHookRecord{}, r.err
}

func TestSocketPathUsesDedicatedPublisherDirectory(t *testing.T) {
	want := "/run/discobox/harness-hooks/hooks.sock"
	if got := SocketPath("/run/discobox/agent-terminals"); got != want {
		t.Fatalf("SocketPath() = %q, want %q", got, want)
	}
}

func assertPermissions(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	// Windows reports 0777 for everything it hands out, so there is no mode
	// here to check. The socket these guard is only ever bound in the sandbox,
	// which is Linux; the rest of this test still exercises the protocol.
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode(%s) = %o, want %o", path, got, want)
	}
}

type recordingRecorder struct {
	record store.HarnessHookRecord
	done   chan struct{}
}

func (r *recordingRecorder) RecordHarnessHook(_ context.Context, record store.HarnessHookRecord) (store.HarnessHookRecord, error) {
	r.record = record
	close(r.done)
	return record, nil
}

func waitForSocket(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{}).DialContext(context.Background(), "unix", socketPath)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket %s did not become ready", socketPath)
}
