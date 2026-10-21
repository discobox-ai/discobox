package sandboxes

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// ReportSandboxStates records what a pool agent observed about the sandboxes it
// hosts (ADR 0017 §10).
//
// This is the only path that writes a sandbox's RuntimeState, and it writes
// nothing else: no generation bump, no desired state, no existence state. What
// the agent saw is not a request.
//
// A state change is also an observation the reconciler may care about, so the
// sandboxes whose state actually moved are marked dirty. The case that matters
// is a container that is gone: the reconciler is what rebuilds it, and nothing
// else would tell it to.
//
// A sandbox that came up has its proxy record delivered again (cameUp), marked
// in the transaction that records it: nothing else notices a delivery that is
// owed, so a lost mark would wait for the next start.
func (s *Service) ReportSandboxStates(ctx context.Context, batch store.SandboxStateReportBatch) error {
	var changed []store.SandboxObservation
	err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		var err error
		if changed, err = txStore.ApplySandboxStateReports(ctx, batch); err != nil || s.engine == nil {
			return err
		}
		for i := range changed {
			sandbox := &changed[i].Sandbox
			if !cameUp(sandbox) {
				continue
			}
			id := SandboxDirtyID(sandbox.ProjectID, sandbox.ID)
			if err := s.engine.MarkDirtyDriftTx(ctx, txDB, SentinelsResourceType, id); err != nil {
				return err
			}
			if err := s.engine.MarkDirtyDriftTx(ctx, txDB, SandboxProxyPolicyResourceType, id); err != nil {
				return err
			}
		}
		return nil
	})
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
				"sandboxId", sandbox.ID, "runtimeState", sandbox.RuntimeState, "runtimeMissing", observation.RuntimeMissing, "error", err)
		}
	}
	return nil
}

// cameUp reports whether an observation is a sandbox meant to exist coming up,
// however it was started: a person's start, a wake, or the pool's own start on
// use. Each one delivers its whole proxy record again — its sentinel set
// (SentinelsResourceType) and its egress policy and gateways
// (SandboxProxyPolicyResourceType). A pool keeps that record nowhere else, a
// start never carries it, and a sandbox without it runs with no sentinels
// swapped and, worse, unrestricted; so one that lost it is whole again at its
// next start rather than at its next rebuild (#314). The marks are drift, the
// sandbox's own observation, so a delivery that keeps failing keeps its
// backoff however often the sandbox starts. They are one update each to one
// sandbox, not a reconcile of it.
func cameUp(sandbox *model.Sandbox) bool {
	return sandbox.DesiredState == model.DesiredStatePresent && sandbox.RuntimeState == model.SandboxRuntimeStateRunning
}

// observationNeedsReconcile reports whether an observation is one the
// reconciler should look at.
//
// A sandbox that just went quiet is worth a look: if its container is gone
// entirely the reconciler is what rebuilds it. A sandbox that came up is not —
// nothing about the control plane's view of it changed by its starting, and
// marking every start dirty would re-drive the whole pool every time somebody
// used a sandbox.
func (s *Service) observationNeedsReconcile(sandbox *model.Sandbox) bool {
	if sandbox.DesiredState == model.DesiredStateDeleted {
		return true
	}
	return !model.SandboxIsLive(sandbox)
}

// Nothing here bumps a generation. A generation versions the spec, and an
// observation is news about the world, not a change to what was asked for —
// including "this sandbox's container is gone", which is drift from a spec that
// nobody edited. A dirty mark is the whole mechanism (ADR 0017 §1), and the
// reconciler's idempotent ensure is what makes acting on one cheap.

// ReportSandboxProgress records provisioning progress a pool agent observed on
// the sandboxes it hosts (ADR 0060).
//
// Nothing is marked dirty. Progress means work is proceeding — an image pull in
// flight — which is the opposite of drift for the reconciler to repair; the
// state report that follows it is what carries any change worth acting on.
func (s *Service) ReportSandboxProgress(ctx context.Context, poolID string, reportedAt time.Time, reports []store.SandboxProgressReport) error {
	if err := s.store.ApplySandboxProgressReports(ctx, poolID, reportedAt, reports); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil // pool is gone; its reports are about nothing we own
		}
		return err
	}
	return nil
}
