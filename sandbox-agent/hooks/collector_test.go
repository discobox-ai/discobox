package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/internal/shorttmp"
	"github.com/discobox-ai/discobox/sandbox-agent/store"
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
