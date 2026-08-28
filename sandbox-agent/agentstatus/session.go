package agentstatus

import (
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandbox-agent/terminal"
)

// ComputeSessionStatus reports one SessionStatus per terminal session.
//
// all is the manager's whole listing, and not all of it is a session: it
// filters to terminal-mode execs (every terminal carries a harnessId — shell
// is a harness too, ADR 0032). One-shot commands are not sessions and never
// appear; a terminal that has ended still does, with its exited or failed
// exec status, for as long as its record exists — deletion is what removes it.
// A terminal revives in place under its own exec id (ADR 0038), so the list
// holds one entry per terminal identity, showing its current run's status; an
// ended entry means "not running, revivable by attach", not a dead sibling.
//
// What a session is *doing* — thinking, idle, waiting on the user — is not
// reported. Deriving it meant reading each terminal's recorded harness hooks
// through a per-harness event mapping, which only claude-code ever had, and
// no client read the result. The hooks themselves are still recorded and
// still readable through `discobox hooks logs`; only the derivation is gone.
func ComputeSessionStatus(all []execs.Exec) []SessionStatus {
	var out []SessionStatus
	for _, exec := range all {
		if terminal.HarnessID(exec) == "" {
			continue
		}
		out = append(out, sessionStatusForExec(exec))
	}
	return out
}

func sessionStatusForExec(exec execs.Exec) SessionStatus {
	return SessionStatus{
		TerminalID:     exec.ID,
		HarnessID:      terminal.HarnessID(exec),
		Primary:        terminal.IsPrimary(exec),
		Title:          exec.Title,
		LastAccessedAt: exec.LastAccessedAt,
		AttacherCount:  exec.AttacherCount,
		ExecStatus:     string(exec.Status),
		StartedAt:      exec.StartedAt,
	}
}
