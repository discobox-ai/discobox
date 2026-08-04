package store

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/server/internal/model"
)

// SandboxStateReport is one runtime observation of one sandbox, as reported by
// the pool agent that hosts it (ADR 0017 §10).
type SandboxStateReport struct {
	SandboxID string
	State     string
	// Error is the reason a sandbox reached a failed state, empty otherwise.
	Error string
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
// Reports are observations, never intent: this writes State, ErrorMessage, and
// the report watermark, and touches neither DesiredState nor Generation.
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
				report = SandboxStateReport{SandboxID: sandbox.ID, State: model.SandboxStateStopped}
			}
			if !acceptSandboxStateReport(sandbox, batch) {
				continue
			}
			previous := sandbox.State
			sandbox.SetState(report.State)
			switch {
			case report.Error != "":
				message := report.Error
				sandbox.ErrorMessage = &message
			case report.State == model.SandboxStateRunning:
				// The sandbox is up, so whatever went wrong last time is no
				// longer what is true of it. Only a live state clears the
				// error: a stopped sandbox that failed to build still owes its
				// owner the reason.
				sandbox.ErrorMessage = nil
			}
			reportedAt := batch.ReportedAt
			sandbox.StateReportedAt = &reportedAt
			sandbox.StateReportBoot = batch.BootID
			sandbox.StateReportSeq = batch.Sequence
			if err := tx.Model(&model.Sandbox{}).
				Where("id = ?", sandbox.ID).
				Select("state", "state_changed_at", "error_message", "state_reported_at", "state_report_boot", "state_report_seq").
				Updates(sandbox).Error; err != nil {
				return err
			}
			if missing || previous != sandbox.State {
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
