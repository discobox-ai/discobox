package agentstatus

import (
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// The session's title is the OSC title its own program set, carried on the
// exec record from the shim's emulator — and nothing else: a session that
// never titled itself reports no title, rather than borrowing the sandbox's
// name or prompt, which the record already carries elsewhere.
func TestComputeSessionStatusTitleIsTheOSCTitleOrNothing(t *testing.T) {
	terminals := []execs.Exec{
		{ID: "exec-1", Status: execs.StatusRunning, Title: "✳ fixing the reaper", Metadata: map[string]string{"harnessId": "claude-code", "primary": "true"}},
		{ID: "exec-2", Status: execs.StatusRunning, Metadata: map[string]string{"harnessId": "claude-code"}},
	}
	statuses := ComputeSessionStatus(terminals)
	if statuses[0].Title != "✳ fixing the reaper" {
		t.Fatalf("title = %q, want the OSC title", statuses[0].Title)
	}
	if !statuses[0].Primary {
		t.Fatal("primary = false, want true")
	}
	if statuses[1].Title != "" {
		t.Fatalf("title = %q, want empty for a session that never titled itself", statuses[1].Title)
	}
}

// Only terminals are sessions: one-shot commands have no harness metadata
// and never appear. A terminal that has ended still does, carrying the exec
// status it ended with, for as long as its record exists.
func TestComputeSessionStatusReportsOnlyTerminals(t *testing.T) {
	all := []execs.Exec{
		{ID: "primary", Status: execs.StatusRunning, Metadata: map[string]string{"harnessId": "claude-code", "primary": "true"}},
		{ID: "one-shot", Status: execs.StatusRunning},
		{ID: "old-shell", Status: execs.StatusExited, Metadata: map[string]string{"harnessId": "shell"}},
		{ID: "crashed", Status: execs.StatusFailed, Metadata: map[string]string{"harnessId": "claude-code"}},
	}
	statuses := ComputeSessionStatus(all)
	if len(statuses) != 3 {
		t.Fatalf("statuses = %+v, want the three terminals", statuses)
	}
	execStatus := map[string]string{}
	for _, st := range statuses {
		execStatus[st.TerminalID] = st.ExecStatus
	}
	if _, ok := execStatus["one-shot"]; ok {
		t.Fatal("one-shot exec should not be reported as a session")
	}
	if execStatus["old-shell"] != string(execs.StatusExited) || execStatus["crashed"] != string(execs.StatusFailed) {
		t.Fatalf("ended terminals should report the exec status they ended with, got %v", execStatus)
	}
}
