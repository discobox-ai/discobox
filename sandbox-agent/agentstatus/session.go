package agentstatus

import (
	"context"
	"strings"
	"time"

	"github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/harness/registry"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
	"github.com/obot-platform/discobox/sandbox-agent/store"
	"github.com/obot-platform/discobox/sandbox-agent/terminal"
)

// HookLister is the subset of sandbox-agent's store needed to derive session
// state from recorded harness lifecycle hooks.
type HookLister interface {
	ListHarnessHooks(ctx context.Context, terminalID string, limit int) ([]store.HarnessHookRecord, error)
}

// hookHistoryLimit is generous because state derivation only needs to scan
// backward to the most recent state-defining event.
const hookHistoryLimit = 200

// ComputeSessionStatus derives one SessionStatus per terminal exec.
// harnessTypeID selects the harness driver (claude-code, etc.) used to derive
// a fine-grained state from recorded hook events; execs without a matching
// harness.SessionStateDeriver, or with no hooks recorded yet, fall back to a
// generic state derived from the exec's own process liveness. prompt is the
// sandbox's resolved initial prompt, used as the primary terminal's session
// name — already exactly "the first prompt," with no transcript parsing
// needed.
func ComputeSessionStatus(ctx context.Context, terminals []execs.Exec, harnessTypeID string, prompt []string, hooks HookLister) []SessionStatus {
	if len(terminals) == 0 {
		return nil
	}
	deriver := sessionStateDeriverFor(harnessTypeID)
	name := strings.Join(prompt, " ")
	out := make([]SessionStatus, 0, len(terminals))
	for _, exec := range terminals {
		out = append(out, sessionStatusForExec(ctx, exec, name, deriver, hooks))
	}
	return out
}

func sessionStatusForExec(ctx context.Context, exec execs.Exec, primaryName string, deriver harness.SessionStateDeriver, hooks HookLister) SessionStatus {
	primary := terminal.IsPrimary(exec)
	status := SessionStatus{
		TerminalID:    exec.ID,
		HarnessID:     terminal.HarnessID(exec),
		Primary:       primary,
		AttacherCount: exec.AttacherCount,
		ExecStatus:    string(exec.Status),
		StartedAt:     exec.StartedAt,
	}
	if primary {
		status.Name = primaryName
	}

	state, lastEvent, lastEventAt := "", "", time.Time{}
	if deriver != nil && hooks != nil {
		if records, err := hooks.ListHarnessHooks(ctx, exec.ID, hookHistoryLimit); err == nil && len(records) > 0 {
			state, lastEvent, lastEventAt = deriver.DeriveSessionState(toHookRecords(records))
		}
	}
	if state == "" {
		state = genericState(exec)
	}
	status.State = state
	status.LastEvent = lastEvent
	if !lastEventAt.IsZero() {
		t := lastEventAt
		status.LastEventAt = &t
	}
	return status
}

// sessionStateDeriverFor returns the harness.SessionStateDeriver for
// harnessTypeID, if that harness's driver implements one. An empty type ID
// (e.g. a plain shell exec) never resolves to one, since
// registry.DriverForHarness falls back to every default driver for an
// unrecognized type, which would otherwise pick an unrelated harness's
// deriver by coincidence of registration order.
func sessionStateDeriverFor(harnessTypeID string) harness.SessionStateDeriver {
	if strings.TrimSpace(harnessTypeID) == "" {
		return nil
	}
	for _, driver := range registry.DriverForHarness(harness.Harness{TypeID: harnessTypeID}) {
		if deriver, ok := driver.(harness.SessionStateDeriver); ok {
			return deriver
		}
	}
	return nil
}

// genericState is the fallback for harnesses without a SessionStateDeriver
// (or before any hooks have been recorded): correct but coarse, derived only
// from the underlying exec process's liveness.
func genericState(exec execs.Exec) string {
	switch exec.Status {
	case execs.StatusStarting, execs.StatusRunning:
		return harness.SessionStateRunning
	case execs.StatusExited:
		return harness.SessionStateExited
	case execs.StatusFailed, execs.StatusLost:
		return harness.SessionStateFailed
	default:
		return harness.SessionStateUnknown
	}
}

func toHookRecords(records []store.HarnessHookRecord) []harness.HookRecord {
	out := make([]harness.HookRecord, len(records))
	for i, record := range records {
		out[i] = harness.HookRecord{Event: record.Event, Payload: record.Payload, CreatedAt: record.CreatedAt}
	}
	return out
}
