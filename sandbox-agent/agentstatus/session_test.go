package agentstatus

import (
	"context"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandbox-agent/store"
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

	statuses := ComputeSessionStatus(context.Background(), terminals, "claude-code", hooks)
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
	if hooks.calls != 1 {
		t.Fatalf("hook lookups = %d, want 1", hooks.calls)
	}
}

func TestComputeSessionStatusFallsBackWhenNoHooksYet(t *testing.T) {
	hooks := &fakeHookLister{byTerminal: map[string][]store.HarnessHookRecord{}}
	terminals := []execs.Exec{
		{ID: "exec-1", Status: execs.StatusRunning, Metadata: map[string]string{"harnessId": "claude-code", "primary": "true"}},
	}

	statuses := ComputeSessionStatus(context.Background(), terminals, "claude-code", hooks)
	got := statuses[0]
	if got.State != harness.SessionStateRunning {
		t.Fatalf("state = %q, want %q (generic fallback)", got.State, harness.SessionStateRunning)
	}
}

// The session's title is the OSC title its own program set, carried on the
// exec record from the shim's emulator — and nothing else: a session that
// never titled itself reports no title, rather than borrowing the sandbox's
// name or prompt, which the record already carries elsewhere.
func TestComputeSessionStatusTitleIsTheOSCTitleOrNothing(t *testing.T) {
	hooks := &fakeHookLister{}
	terminals := []execs.Exec{
		{ID: "exec-1", Status: execs.StatusRunning, Title: "✳ fixing the reaper", Metadata: map[string]string{"harnessId": "claude-code", "primary": "true"}},
		{ID: "exec-2", Status: execs.StatusRunning, Metadata: map[string]string{"harnessId": "claude-code"}},
	}
	statuses := ComputeSessionStatus(context.Background(), terminals, "claude-code", hooks)
	if statuses[0].Title != "✳ fixing the reaper" {
		t.Fatalf("title = %q, want the OSC title", statuses[0].Title)
	}
	if statuses[1].Title != "" {
		t.Fatalf("title = %q, want empty for a session that never titled itself", statuses[1].Title)
	}
}

func TestComputeSessionStatusUnknownHarnessSkipsHookLookupEntirely(t *testing.T) {
	hooks := &fakeHookLister{}
	terminals := []execs.Exec{
		{ID: "exec-1", Status: execs.StatusRunning, Metadata: map[string]string{"harnessId": "shell", "primary": "true"}},
	}
	statuses := ComputeSessionStatus(context.Background(), terminals, "", hooks)
	if statuses[0].State != harness.SessionStateRunning {
		t.Fatalf("state = %q, want %q", statuses[0].State, harness.SessionStateRunning)
	}
	if hooks.calls != 0 {
		t.Fatalf("hook lookups = %d, want 0 for an unrecognized harness", hooks.calls)
	}
}

// Only terminals are sessions: one-shot commands have no harness metadata
// and never appear. A terminal that has ended still does, with its ended
// state, for as long as its record exists.
func TestComputeSessionStatusReportsOnlyTerminals(t *testing.T) {
	hooks := &fakeHookLister{}
	all := []execs.Exec{
		{ID: "primary", Status: execs.StatusRunning, Metadata: map[string]string{"harnessId": "claude-code", "primary": "true"}},
		{ID: "one-shot", Status: execs.StatusRunning},
		{ID: "old-shell", Status: execs.StatusExited, Metadata: map[string]string{"harnessId": "shell"}},
		{ID: "crashed", Status: execs.StatusFailed, Metadata: map[string]string{"harnessId": "claude-code"}},
	}
	statuses := ComputeSessionStatus(context.Background(), all, "claude-code", hooks)
	if len(statuses) != 3 {
		t.Fatalf("statuses = %+v, want the three terminals", statuses)
	}
	states := map[string]string{}
	for _, st := range statuses {
		states[st.TerminalID] = st.State
	}
	if states["one-shot"] != "" {
		t.Fatal("one-shot exec should not be reported as a session")
	}
	if states["old-shell"] != harness.SessionStateExited || states["crashed"] != harness.SessionStateFailed {
		t.Fatalf("ended terminals should report their ended state, got %v", states)
	}
}
