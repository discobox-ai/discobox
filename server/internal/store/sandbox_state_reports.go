package store

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/server/internal/model"
)

// SandboxStateReport is one runtime observation of one sandbox, as reported by
// the pool agent that hosts it (ADR 0017 §10).
//
// State is a runtime state — one of model.SandboxRuntimeStates. A report says
// nothing about whether the sandbox should exist, and carries no error: why an
// operation failed is the reconciler's verdict, recorded on ErrorMessage, and a
// second writer of that field is what ADR 0034 exists to remove.
type SandboxStateReport struct {
	SandboxID string
	State     string
}

// SandboxObservation is one sandbox the caller should look at after a batch.
//
// RuntimeMissing separates the two facts a complete sync can deliver, which the
// recorded state cannot tell apart: a sandbox whose container is gone and one
// that is merely stopped both read `stopped`, but only the first needs its
// container rebuilt.
type SandboxObservation struct {
	Sandbox        model.Sandbox
	RuntimeMissing bool
}

// SandboxStateReportBatch is a single delivery on the state channel.
//
// Complete marks the periodic full sync: the reports are every sandbox the
// agent hosts, so a sandbox the control plane believes is on this pool and that
// the batch omits no longer has a container. A batch that is not complete is a
// transition delta and says nothing about sandboxes it does not mention.
type SandboxStateReportBatch struct {
	PoolID     string
	BootID     string
	Sequence   int64
	ReportedAt time.Time
	Complete   bool
	Reports    []SandboxStateReport
}

// ApplySandboxStateReports records a batch of runtime observations and returns
// the sandboxes the reconciler should take a look at.
//
// Reports are observations, never intent: this writes RuntimeState and the
// report watermark, and touches neither DesiredState, Generation, State, nor
// ErrorMessage. It is the only writer of the runtime axis, and the only place
// that writes those columns at all (ADR 0034 §2).
//
// Two things earn a look. A state that actually changed is the obvious one. The
// other is a sandbox a complete sync omitted: its container is gone, which is a
// different fact from "stopped" even though both record the same state, and it
// is the only signal that existence reconciliation has work to do. Returning it
// only when the state string changed would leave a sandbox that was already
// stopped when its container was reaped invisible forever — reachable in
// listings, and 404 to everything that tried to use it.
func (s *Store) ApplySandboxStateReports(ctx context.Context, batch SandboxStateReportBatch) ([]SandboxObservation, error) {
	var changed []SandboxObservation
	err := s.Transaction(ctx, func(txStore *Store, tx *gorm.DB) error {
		changed = nil
		pool, err := txStore.GetPoolByID(ctx, batch.PoolID)
		if err != nil {
			return err
		}
		reported := make(map[string]SandboxStateReport, len(batch.Reports))
		for _, report := range batch.Reports {
			reported[report.SandboxID] = report
		}

		hosted, err := txStore.listSandboxesForPool(ctx, tx, pool.ProjectID, batch.PoolID)
		if err != nil {
			return err
		}
		for i := range hosted {
			sandbox := &hosted[i]
			// An archived sandbox has no container by intent, so every complete
			// sync omits it — the same signal a lost container sends. Left to the
			// logic below it would be recorded as `stopped` and handed to the
			// reconciler as drift to repair, which would rebuild the container the
			// archive just removed and put the sandbox beyond its retention
			// policy. Archived sandboxes are simply not the runtime's to report
			// on (ADR 0022 §5).
			if sandbox.DesiredState == model.DesiredStateArchived {
				continue
			}
			report, ok := reported[sandbox.ID]
			missing := false
			if !ok {
				if !batch.Complete {
					continue
				}
				missing = true
				// A complete sync that omits a sandbox is reporting that its
				// container is gone. That is a fact about the container, not
				// about whether the sandbox should exist: existence
				// reconciliation rebuilds it if it is still wanted, and it
				// stays stopped until something uses it (ADR 0017 §13).
				//
				// `stopped` rather than unobserved is deliberate: a container
				// that is gone is certainly not running, and reverting to
				// unobserved would report the sandbox as starting.
				report = SandboxStateReport{SandboxID: sandbox.ID, State: model.SandboxRuntimeStateStopped}
			}
			if !acceptSandboxStateReport(sandbox, batch) {
				continue
			}
			previous := sandbox.RuntimeState
			sandbox.SetRuntimeState(report.State)
			reportedAt := batch.ReportedAt
			sandbox.StateReportedAt = &reportedAt
			sandbox.StateReportBoot = batch.BootID
			sandbox.StateReportSeq = batch.Sequence
			if err := tx.Model(&model.Sandbox{}).
				Where("id = ?", sandbox.ID).
				Select(observedSandboxColumns).
				Updates(sandbox).Error; err != nil {
				return err
			}
			if missing || previous != sandbox.RuntimeState {
				changed = append(changed, SandboxObservation{Sandbox: *sandbox, RuntimeMissing: missing})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return changed, nil
}

// acceptSandboxStateReport rejects a report older than the one already
// recorded, so a delayed transition delta cannot overwrite a newer complete
// sync (ADR 0017 §10).
//
// Within one agent boot the sequence is authoritative. Across boots it means
// nothing — a restarted agent counts from zero again — so the report timestamp
// decides, and an agent that has just booted is reporting what it can see now.
func acceptSandboxStateReport(sandbox *model.Sandbox, batch SandboxStateReportBatch) bool {
	if sandbox.StateReportBoot == batch.BootID {
		return batch.Sequence > sandbox.StateReportSeq
	}
	if sandbox.StateReportedAt == nil {
		return true
	}
	return !batch.ReportedAt.Before(*sandbox.StateReportedAt)
}

func (s *Store) listSandboxesForPool(ctx context.Context, tx *gorm.DB, projectID, poolID string) ([]model.Sandbox, error) {
	var sandboxes []model.Sandbox
	err := tx.WithContext(ctx).
		Where("project_id = ? AND pool_id = ?", projectID, poolID).
		Find(&sandboxes).Error
	return sandboxes, err
}
