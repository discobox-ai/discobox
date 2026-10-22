package sandboxes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// This file is what using a sandbox does to it (ADR 26-10-12-711): an action
// has the pool start it before the request is forwarded, and an action that
// finds it with no container sets its rebuild in motion — a repair, once, for
// a sandbox that had failed.
//
// None of it is a second way to converge a sandbox. The start is the pool
// agent's own, asked for early; the repair is the intent RepairSandbox
// records, with a different author; and the rebuild of a sandbox that merely
// lost its container is the dirty mark an observation of that already makes.

// ensureRunning has the pool start the sandbox an action is about to be
// forwarded into, and returns once the agent inside it answers.
//
// It is asked before the lease is handed out so that "no container" is learned
// while nothing has been forwarded: the request, with its body, is still the
// caller's, and a repair can run under it.
//
// A healthy sandbox the row reads as running is not asked about. The HTTP port
// proxy acquires on every request, and a round trip on each buys nothing the
// pool agent does not already do: it starts the sandbox again by the lease's
// mark if a stop landed after this look (ADR 26-10-12-711 §2). A failed
// sandbox is always asked, whatever it was last seen doing: the observation is
// up to a complete sync old, and "no container" is the answer that repairs it.
func (s *Service) ensureRunning(ctx context.Context, sb *model.Sandbox) error {
	if sb.State == model.SandboxStateReady && sb.RuntimeState == model.SandboxRuntimeStateRunning {
		return nil
	}
	provider, err := s.resolveProvider(ctx, sb)
	if err != nil {
		return err
	}
	if provider == nil {
		return nil
	}
	err = provider.EnsureRunning(ctx, sandboxRefFromSandbox(sb), sb.ProviderState)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sandbox.ErrPoolAgentUnsupported):
		// A pool agent from before the operation. It still starts a stopped
		// sandbox when the request reaches it, by its own table, and says "no
		// container" only then, so nothing is repaired until its pool moves
		// onto the current agent.
		return nil
	case errors.Is(err, sandbox.ErrPoolNotReachable):
		return apperrors.StatusError{Status: http.StatusConflict, Message: err.Error(), Cause: err}
	case errors.Is(err, ErrNoContainer), errors.Is(err, ErrArchived):
		return runtimeRefusal(err, "use")
	case errors.Is(err, ErrNotFound):
		// The pool holds nothing of a sandbox whose row has settled. That is
		// an answer, unlike the acquire's own not-found for a sandbox no pool
		// has been given yet, so it does not carry the sentinel the wait reads
		// as "not yet": nothing is coming that would clear it.
		return apperrors.NewStatusError(http.StatusConflict, runtimeRefusal(err, "use").Error())
	}
	// The start itself failed, and the failure is what the caller can act on:
	// a missing bind mount, an image that will not run. git prints a refused
	// request's body to the person fetching, so this is also what `discobox
	// apply` says when the sandbox holding the work will not start.
	return fmt.Errorf("start sandbox %q: %w; nothing in it can be reached until it starts, so if this persists run `discobox admin box repair %s`", sb.ID, err, sb.ID)
}

// rebuildOnUse answers an action that found the sandbox with no container on
// its pool (runtimeHasNoContainer). It reports whether a rebuild is now coming,
// which the action waits for; when none is, the action is answered with the
// pool's refusal, or with the error returned here.
//
//   - A generation not yet observed is a rebuild already in flight: a repair,
//     an upgrade, or the automatic repair another request just recorded.
//   - A settled failure is repaired, once (recordAutomaticRepair). One that is
//     an automatic repair's own failure has had its one, and is refused with
//     both failures and the command that repairs it.
//   - A `ready` sandbox lost its container without failing, and the reconciler
//     rebuilds it from the spec as soon as it is marked dirty. The complete
//     sync would mark it within a minute; this does not wait for that.
//
// Anything else — archived, on its way out, never yet created — has no
// container for a reason a rebuild does not answer.
//
// It runs only under an action, never under a report: "container gone"
// arrives every sync, and acting on it there is the loop a settled failure
// exists to prevent (ADR 0017 §4).
func (s *Service) rebuildOnUse(ctx context.Context, sb *model.Sandbox, use services.SandboxUse, marked *bool) (bool, error) {
	switch {
	case sb.DesiredState != model.DesiredStatePresent:
		return false, nil
	case !sb.Converged():
		return true, nil
	case sb.State == model.SandboxStateFailed || sb.ErrorMessage != nil:
		if sb.AutomaticRepairSpent() {
			return false, automaticRepairFailed(sb)
		}
		// Recorded, or lost to a concurrent request or a newer intent: either
		// way the row has moved, and the next attempt reads what it is now.
		if _, err := s.recordAutomaticRepair(ctx, sb, use); err != nil {
			return false, err
		}
		return true, nil
	case sb.State == model.SandboxStateReady:
		if !*marked {
			if s.engine == nil {
				return false, errors.New("reconcile engine is required")
			}
			if err := s.engine.MarkDirty(ctx, SandboxResourceType, SandboxDirtyID(sb.ProjectID, sb.ID)); err != nil {
				return false, err
			}
			*marked = true
		}
		return true, nil
	}
	return false, nil
}

// automaticRepairFailed refuses a sandbox whose one automatic repair has
// already failed. It gives both failures — the one the repair answered, and
// the repair's own — because the second alone does not say the sandbox was
// broken before anything was tried.
func automaticRepairFailed(sb *model.Sandbox) error {
	repairFailure := "it left the sandbox with no container"
	if sb.ErrorMessage != nil {
		repairFailure = *sb.ErrorMessage
	}
	failure := "it had no container on its pool"
	if sb.AutomaticRepair != nil && sb.AutomaticRepair.Failure != "" {
		failure = sb.AutomaticRepair.Failure
	}
	return apperrors.StatusError{
		Status: http.StatusConflict,
		Message: fmt.Sprintf("the discobox failed (%s), and the one automatic repair of it failed too (%s); run `discobox admin box repair %s`",
			failure, repairFailure, sb.ID),
		Cause: ErrNoContainer,
	}
}

// recordAutomaticRepair records the repair intent RepairSandbox does, for a
// failed sandbox an action found with no container, and reports whether it
// did (ADR 26-10-12-711 §§3-5).
//
// The intent is the same one — the generation bump, RepairGeneration naming
// it, the dirty mark — so the reconciler tears down and rebuilds exactly as
// for a typed repair, and cannot tell the two apart. What differs is here:
//
//   - It names itself on the row (AutomaticRepair), and is not recorded for a
//     generation that already is one. That is the whole of what bounds it: no
//     counter and no timer, and any explicit intent moves the generation on.
//   - Whether it is still due is decided in the transaction, on the row as it
//     is then. Two requests that both saw the failure collapse to one repair:
//     the second finds a generation it did not judge.
//   - It re-pins only where the project's sandboxes follow their harness image
//     on their own. Nobody asked for this repair, so under a `manual` policy
//     it rebuilds on the pin the sandbox has.
//   - It is not driven inline and sends no start. The action that caused it is
//     waiting for the reconcile, and starts the sandbox itself when it lands.
func (s *Service) recordAutomaticRepair(ctx context.Context, observed *model.Sandbox, use services.SandboxUse) (bool, error) {
	if s.engine == nil {
		return false, errors.New("reconcile engine is required")
	}
	var repin sandboxRepin
	project, err := s.store.GetProject(ctx, observed.ProjectID)
	if err != nil {
		return false, err
	}
	if model.SandboxUpgradesAutomatically(project) {
		if repin, err = s.currentRepin(ctx, observed); err != nil {
			return false, err
		}
	}
	var record *model.AutomaticRepair
	err = s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		sb, err := txStore.GetSandbox(ctx, observed.ProjectID, observed.ID)
		if err != nil {
			return err
		}
		// The pool said "no container" of the generation this request read.
		// A row that has moved on since is somebody else's to judge.
		if sb.Generation != observed.Generation || !sb.AutomaticRepairAllowed() {
			return nil
		}
		failure := ""
		if sb.ErrorMessage != nil {
			failure = *sb.ErrorMessage
		}
		previousGeneration := sb.Generation
		sb.IncrementGeneration()
		sb.RecordIntent(model.DesiredStatePresent)
		sb.RepairGeneration = sb.Generation
		sb.AutomaticRepair = &model.AutomaticRepair{
			Generation: sb.Generation,
			StartedAt:  time.Now().UTC(),
			Trigger:    use.Trigger,
			Failure:    failure,
		}
		repin.apply(sb)
		if err := txStore.UpdateSandbox(ctx, sb, store.WithGeneration(previousGeneration)); err != nil {
			return err
		}
		record = sb.AutomaticRepair
		return s.engine.MarkDirtyTx(ctx, txDB, SandboxResourceType, SandboxDirtyID(sb.ProjectID, sb.ID))
	})
	if errors.Is(err, store.ErrGenerationConflict) {
		return false, nil
	}
	if err != nil || record == nil {
		return false, err
	}
	// The record of who used it. Success clears the failure from the row, and
	// the row keeps what was repaired and why, not for whom.
	attrs := []any{
		"sandbox", observed.ID,
		"project", observed.ProjectID,
		"generation", record.Generation,
		"trigger", record.Trigger,
		"failure", record.Failure,
	}
	if principal, ok := auth.PrincipalFromContext(ctx); ok {
		attrs = append(attrs, "principalType", principal.Type, "user", principal.UserID)
		if principal.SandboxID != "" {
			attrs = append(attrs, "bySandbox", principal.SandboxID)
		}
	}
	slog.InfoContext(ctx, "repairing a failed sandbox with no container because it was used", attrs...)
	return true, nil
}
