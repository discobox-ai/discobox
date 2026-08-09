package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/obot-platform/discobox/sandbox-agent/execs"
)

func TestRecordAndListEvents(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.RecordExecEvent(ctx, "ex_1", "exec.created", "created", map[string]any{"harnessId": "codex"}); err != nil {
		t.Fatalf("record event: %v", err)
	}
	events, err := st.ListEvents(ctx, "ex_1", 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 || events[0].Type != "exec.created" || events[0].Details["harnessId"] != "codex" {
		t.Fatalf("events = %#v", events)
	}
}

func TestPrimaryTerminalLaunchedMarker(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "harness.db")
	st, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	launched, err := st.PrimaryTerminalLaunched(ctx)
	if err != nil {
		t.Fatalf("primary launched: %v", err)
	}
	if launched {
		t.Fatalf("expected primary not launched initially")
	}
	if err := st.MarkPrimaryTerminalLaunched(ctx); err != nil {
		t.Fatalf("mark primary launched: %v", err)
	}
	// Marking twice must be idempotent (upsert).
	if err := st.MarkPrimaryTerminalLaunched(ctx); err != nil {
		t.Fatalf("mark primary launched again: %v", err)
	}
	// A freshly reopened store (simulating a sandbox restart) must observe the
	// durable marker.
	reopened, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	launched, err = reopened.PrimaryTerminalLaunched(ctx)
	if err != nil {
		t.Fatalf("primary launched after reopen: %v", err)
	}
	if !launched {
		t.Fatalf("expected primary launched after mark")
	}
}

func TestRecordAndListHarnessHooks(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_, err = st.RecordHarnessHook(ctx, HarnessHookRecord{
		TerminalID: "agt_1",
		Provider:   "codex",
		Event:      "PreToolUse",
		Payload:    json.RawMessage(`{"tool_name":"Bash"}`),
	})
	if err != nil {
		t.Fatalf("record hook: %v", err)
	}
	hooks, err := st.ListHarnessHooks(ctx, "agt_1", 10)
	if err != nil {
		t.Fatalf("list hooks: %v", err)
	}
	if len(hooks) != 1 || hooks[0].Provider != "codex" || hooks[0].Event != "PreToolUse" {
		t.Fatalf("hooks = %#v", hooks)
	}
	if string(hooks[0].Payload) != `{"tool_name":"Bash"}` {
		t.Fatalf("payload = %s", hooks[0].Payload)
	}
}

func TestExecRecordIsDurableAndImmutable(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	created := time.Now().UTC()
	rec := execs.Exec{
		ID:        "ex_1",
		Command:   []string{"codex", "say hello"},
		Workdir:   "/workspace",
		TTY:       true,
		CreatedAt: created,
		Metadata:  map[string]string{"harnessId": "codex", "primary": "true"},
	}
	if err := st.SaveExecRecord(ctx, rec); err != nil {
		t.Fatalf("save record: %v", err)
	}
	// Observing status must not touch the immutable record.
	exited := time.Now().UTC()
	code := int64(0)
	if err := st.ObserveExec(ctx, execs.Exec{ID: "ex_1", Status: execs.StatusExited, ExitedAt: &exited, ExitCode: &code}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	// A second save with different metadata must be ignored (immutable).
	if err := st.SaveExecRecord(ctx, execs.Exec{ID: "ex_1", Command: []string{"other"}, Metadata: map[string]string{"harnessId": "changed"}}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	records, err := st.LoadExecRecords(ctx)
	if err != nil {
		t.Fatalf("load records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	got := records[0]
	if got.Metadata["harnessId"] != "codex" || got.Metadata["primary"] != "true" {
		t.Fatalf("metadata not durable/immutable: %v", got.Metadata)
	}
	if len(got.Command) != 2 || got.Command[0] != "codex" {
		t.Fatalf("command = %v", got.Command)
	}
	// Status is joined from the latest ExecState observation.
	if got.Status != execs.StatusExited {
		t.Fatalf("status = %q, want exited (joined from ExecState)", got.Status)
	}
}

func TestObserveExecRecordsTransitions(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	createdAt := time.Now().UTC()
	status := execs.Exec{ID: "ex_1", Status: execs.StatusRunning, CreatedAt: createdAt}
	if err := st.ObserveExec(ctx, status); err != nil {
		t.Fatalf("observe running: %v", err)
	}
	exitedAt := time.Now().UTC()
	code := int64(7)
	status.Status = execs.StatusFailed
	status.ExitedAt = &exitedAt
	status.ExitCode = &code
	if err := st.ObserveExec(ctx, status); err != nil {
		t.Fatalf("observe failed: %v", err)
	}
	events, err := st.ListEvents(ctx, "ex_1", 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	seen := map[string]bool{}
	for _, event := range events {
		seen[event.Type] = true
	}
	for _, typ := range []string{"exec.observed", "exec.status.changed", "exec.exited"} {
		if !seen[typ] {
			t.Fatalf("missing event %s in %#v", typ, events)
		}
	}
}

func TestResourceSamplesRespectRetention(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for i := range 3 {
		_, err := st.RecordResourceSample(ctx, ResourceSample{
			TerminalID: "agt_1",
			SampledAt:  time.Unix(int64(i), 0).UTC(),
			Source:     "test",
			Data:       []byte(`{"index":` + string(rune('0'+i)) + `}`),
		}, 2)
		if err != nil {
			t.Fatalf("record sample %d: %v", i, err)
		}
	}
	samples, err := st.ListResourceSamples(ctx, "agt_1", 10)
	if err != nil {
		t.Fatalf("list samples: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("len(samples) = %d, want 2: %#v", len(samples), samples)
	}
	if samples[0].SampledAt.Unix() != 1 || samples[1].SampledAt.Unix() != 2 {
		t.Fatalf("samples not oldest-to-newest retained tail: %#v", samples)
	}
}

func TestExecLogChunksAreOrderedAndDeletable(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	base := time.Now().UTC()
	for i := range 3 {
		if err := st.AppendExecLogChunk(ctx, "exec_1", base.Add(time.Duration(i)*time.Minute), "zstd", []byte{byte(i)}, 10); err != nil {
			t.Fatalf("append chunk %d: %v", i, err)
		}
	}
	// A second exec's chunks must not leak into the first exec's read.
	if err := st.AppendExecLogChunk(ctx, "exec_2", base, "zstd", []byte{9}, 1); err != nil {
		t.Fatalf("append other exec chunk: %v", err)
	}
	chunks, err := st.ListExecLogChunks(ctx, "exec_1")
	if err != nil {
		t.Fatalf("list chunks: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("len(chunks) = %d, want 3: %#v", len(chunks), chunks)
	}
	for i, chunk := range chunks {
		if chunk.Data[0] != byte(i) {
			t.Fatalf("chunks[%d] = %#v, want oldest-first order", i, chunk)
		}
	}
	if err := st.DeleteExecLog(ctx, "exec_1"); err != nil {
		t.Fatalf("delete log: %v", err)
	}
	chunks, err = st.ListExecLogChunks(ctx, "exec_1")
	if err != nil {
		t.Fatalf("list chunks after delete: %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("len(chunks) after delete = %d, want 0", len(chunks))
	}
	remaining, err := st.ListExecLogChunks(ctx, "exec_2")
	if err != nil {
		t.Fatalf("list other exec chunks: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("deleting exec_1 must not remove exec_2's chunks: %#v", remaining)
	}
}

func TestExecLogChunksPruneBeyondRetention(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Now().UTC()
	if err := st.AppendExecLogChunk(ctx, "exec_1", now.Add(-defaultLogRetention-time.Hour), "zstd", []byte{1}, 1); err != nil {
		t.Fatalf("append stale chunk: %v", err)
	}
	if err := st.AppendExecLogChunk(ctx, "exec_1", now, "zstd", []byte{2}, 1); err != nil {
		t.Fatalf("append fresh chunk: %v", err)
	}
	chunks, err := st.ListExecLogChunks(ctx, "exec_1")
	if err != nil {
		t.Fatalf("list chunks: %v", err)
	}
	if len(chunks) != 1 || chunks[0].Data[0] != 2 {
		t.Fatalf("chunks = %#v, want only the fresh chunk retained", chunks)
	}
}
