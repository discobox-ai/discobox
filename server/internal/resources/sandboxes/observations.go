package sandboxes

import (
	"context"
	"errors"
	"log/slog"

	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/store"
)

// ReportSandboxStates records what a pool agent observed about the sandboxes it
// hosts (ADR 0017 §10).
//
// This is the only path by which a sandbox's State changes to a runtime fact,
// and it writes nothing else: no generation bump, no desired state, no
// operation. What the agent saw is not a request.
//
// A state change is also an observation the reconciler may care about, so the
// sandboxes whose state actually moved are marked dirty. That is what re-arms
// the ADR 0016 §5 re-pin: a sandbox that has just left a live state is one whose
// container can be rebuilt on a newer image without disturbing anybody, and the
// reconciler is the thing that decides so.
func (s *Service) ReportSandboxStates(ctx context.Context, batch store.SandboxStateReportBatch) error {
	changed, err := s.store.ApplySandboxStateReports(ctx, batch)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil // pool is gone; its reports are about nothing we own
		}
		return err
	}
	if s.engine == nil {
		return nil
	}
	for i := range changed {
		observation := &changed[i]
		sandbox := &observation.Sandbox
		if !s.observationNeedsReconcile(sandbox) {
			continue
		}
		if err := s.engine.MarkDirty(ctx, SandboxResourceType, SandboxDirtyID(sandbox.ProjectID, sandbox.ID)); err != nil {
			slog.WarnContext(ctx, "could not mark sandbox dirty after a state report",
				"sandboxId", sandbox.ID, "state", sandbox.State, "runtimeMissing", observation.RuntimeMissing, "error", err)
		}
	}
	return nil
}

// observationNeedsReconcile reports whether an observation is one the
// reconciler should look at.
//
// A sandbox that just went quiet is worth a look: it may be re-pinnable, and if
// its container is gone entirely the reconciler is what rebuilds it. A sandbox
// that came up is not — nothing about the control plane's view of it changed by
// its starting, and marking every start dirty would re-drive the whole pool
// every time somebody used a sandbox.
func (s *Service) observationNeedsReconcile(sandbox *model.Sandbox) bool {
	if sandbox.DesiredState == model.DesiredStateDeleted {
		return true
	}
	return !model.SandboxIsLive(sandbox.State)
}

// Nothing here bumps a generation. A generation versions the spec, and an
// observation is news about the world, not a change to what was asked for —
// including "this sandbox's container is gone", which is drift from a spec that
// nobody edited. A dirty mark is the whole mechanism (ADR 0017 §1), and the
// reconciler's idempotent ensure is what makes acting on one cheap.
