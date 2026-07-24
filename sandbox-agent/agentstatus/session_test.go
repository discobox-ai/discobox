package agentstatus

import (
	"context"
	"testing"
	"time"

	"github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
	"github.com/obot-platform/discobox/sandbox-agent/store"
)

// fakeHookLister returns a fixed set of hooks per terminal ID, and records
// whether it was ever called, so tests can assert a nil deriver skips the
// lookup entirely.
type fakeHookLister struct {
	byTerminal map[string][]store.HarnessHookRecord
	calls      int
}

func (f *fakeHookLister) ListHarnessHooks(_ context.Context, terminalID string, _ int) ([]store.HarnessHookRecord, error) {
	f.calls++
	return f.byTerminal[terminalID], nil
}

func hookRecord(event string, at time.Time) store.HarnessHookRecord {
	return store.HarnessHookRecord{Event: event, CreatedAt: at}
}

func TestComputeSessionStatusClaudeCodeDerivesFineGrainedState(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hooks := &fakeHookLister{byTerminal: map[string][]store.HarnessHookRecord{
		"exec-1": {
			hookRecord("SessionStart", base),
			hookRecord("UserPromptSubmit", base.Add(time.Second)),
			hookRecord("Stop", base.Add(2*time.Second)),
		},
	}}
	terminals := []execs.Exec{
		{ID: "exec-1", Status: execs.StatusRunning, Metadata: map[string]string{"harnessId": "claude-code", "primary": "true"}},
	}

	statuses := ComputeSessionStatus(context.Background(), terminals, "claude-code", []string{"fix", "the", "bug"}, hooks)
	if len(statuses) != 1 {
		t.Fatalf("statuses = %d, want 1", len(statuses))
	}
	got := statuses[0]
	if got.State != harness.SessionStateIdle {
		t.Fatalf("state = %q, want %q", got.State, harness.SessionStateIdle)
	}
	if got.LastEvent != "Stop" {
		t.Fatalf("lastEvent = %q, want Stop", got.LastEvent)
	}
	if !got.Primary {
		t.Fatal("primary = false, want true")
	}
	if got.Name != "fix the bug" {
		t.Fatalf("name = %q, want %q", got.Name, "fix the bug")
	}
	if hooks.calls != 1 {
		t.Fatalf("hook lookups = %d, want 1", hooks.calls)
	}
}

func TestComputeSessionStatusFallsBackWhenNoHooksYet(t *testing.T) {
	hooks := &fakeHookLister{byTerminal: map[string][]store.HarnessHookRecord{}}
	terminals := []execs.Exec{
		{ID: "exec-1", Status: execs.StatusRunning, Metadata: map[string]string{"harnessId": "claude-code", "primary": "true"}},
	}

	statuses := ComputeSessionStatus(context.Background(), terminals, "claude-code", []string{"hi"}, hooks)
	got := statuses[0]
	if got.State != harness.SessionStateRunning {
		t.Fatalf("state = %q, want %q (generic fallback)", got.State, harness.SessionStateRunning)
	}
}

func TestComputeSessionStatusNonPrimaryHasNoName(t *testing.T) {
	hooks := &fakeHookLister{}
	terminals := []execs.Exec{
		{ID: "exec-1", Status: execs.StatusRunning, Metadata: map[string]string{"harnessId": "claude-code"}},
	}
	statuses := ComputeSessionStatus(context.Background(), terminals, "claude-code", []string{"hi"}, hooks)
	if statuses[0].Name != "" {
		t.Fatalf("name = %q, want empty for a non-primary terminal", statuses[0].Name)
	}
}

func TestComputeSessionStatusUnknownHarnessSkipsHookLookupEntirely(t *testing.T) {
	hooks := &fakeHookLister{}
	terminals := []execs.Exec{
		{ID: "exec-1", Status: execs.StatusExited, Metadata: map[string]string{"primary": "true"}},
	}
	statuses := ComputeSessionStatus(context.Background(), terminals, "", []string{"hi"}, hooks)
	if statuses[0].State != harness.SessionStateExited {
		t.Fatalf("state = %q, want %q", statuses[0].State, harness.SessionStateExited)
	}
	if hooks.calls != 0 {
		t.Fatalf("hook lookups = %d, want 0 for an unrecognized harness", hooks.calls)
	}
}

func TestComputeSessionStatusFailedExec(t *testing.T) {
	hooks := &fakeHookLister{}
	terminals := []execs.Exec{
		{ID: "exec-1", Status: execs.StatusFailed},
	}
	statuses := ComputeSessionStatus(context.Background(), terminals, "codex-cli", nil, hooks)
	if statuses[0].State != harness.SessionStateFailed {
		t.Fatalf("state = %q, want %q", statuses[0].State, harness.SessionStateFailed)
	}
}
