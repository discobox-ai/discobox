package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/obot-platform/discobox/sandbox-agent/terminal"
)

func TestRecordAndListEvents(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.RecordEvent(ctx, "agt_1", "terminal.created", "created", map[string]any{"agentId": "codex"}); err != nil {
		t.Fatalf("record event: %v", err)
	}
	events, err := st.ListEvents(ctx, "agt_1", 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 || events[0].Type != "terminal.created" || events[0].Details["agentId"] != "codex" {
		t.Fatalf("events = %#v", events)
	}
}

func TestObserveTerminalRecordsTransitions(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	createdAt := time.Now().UTC()
	status := terminal.Terminal{ID: "agt_1", Status: terminal.StatusRunning, CreatedAt: createdAt}
	if err := st.ObserveTerminal(ctx, status); err != nil {
		t.Fatalf("observe running: %v", err)
	}
	exitedAt := time.Now().UTC()
	code := int64(7)
	status.Status = terminal.StatusFailed
	status.ExitedAt = &exitedAt
	status.ExitCode = &code
	if err := st.ObserveTerminal(ctx, status); err != nil {
		t.Fatalf("observe failed: %v", err)
	}
	events, err := st.ListEvents(ctx, "agt_1", 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	seen := map[string]bool{}
	for _, event := range events {
		seen[event.Type] = true
	}
	for _, typ := range []string{"terminal.observed", "terminal.status.changed", "terminal.exited"} {
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
