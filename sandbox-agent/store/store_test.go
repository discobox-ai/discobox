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
	st, err := Open(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.RecordExecEvent(ctx, "ex_1", "exec.created", "created", map[string]any{"agentId": "codex"}); err != nil {
		t.Fatalf("record event: %v", err)
	}
	events, err := st.ListEvents(ctx, "ex_1", 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 || events[0].Type != "exec.created" || events[0].Details["agentId"] != "codex" {
		t.Fatalf("events = %#v", events)
	}
}

func TestPrimaryTerminalLaunchedMarker(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "agent.db")
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

func TestRecordAndListAgentHooks(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_, err = st.RecordAgentHook(ctx, AgentHookRecord{
		TerminalID: "agt_1",
		Provider:   "codex",
		Event:      "PreToolUse",
		Payload:    json.RawMessage(`{"tool_name":"Bash"}`),
	})
	if err != nil {
		t.Fatalf("record hook: %v", err)
	}
	hooks, err := st.ListAgentHooks(ctx, "agt_1", 10)
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

func TestObserveExecRecordsTransitions(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "agent.db"))
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
	st, err := Open(ctx, filepath.Join(t.TempDir(), "agent.db"))
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
