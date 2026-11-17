package sandboxes

import (
	"context"
	"errors"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// DefaultArchiveRetention is how long an archived sandbox is kept when neither
// the project nor the server has chosen otherwise. It is a policy, not a
// mechanism: the pool agent's volume reaper has a same-length window for trees
// whose sandbox the control plane no longer holds, and the two are unrelated
// (ADR 0022 §4, ADR 26-10-01-876).
const DefaultArchiveRetention = 24 * time.Hour

// serverArchiveRetention is the default every project that has not set its own
// follows. A zero configured value means the server said nothing, which is the
// distinction that lets a development server shorten the window without
// touching any project's setting.
func (r *SandboxReconciler) serverArchiveRetention() time.Duration {
	if r.archiveRetention > 0 {
		return r.archiveRetention
	}
	return DefaultArchiveRetention
}

// projectArchiveRetention resolves the retention that applies to one sandbox. A
// project that has not set one follows the server default as it changes, rather
// than having been frozen to whatever the default was when it was created.
func (r *SandboxReconciler) projectArchiveRetention(ctx context.Context, projectID string) time.Duration {
	project, err := r.store.GetProject(ctx, projectID)
	if err != nil || project == nil || project.ArchiveRetentionSeconds <= 0 {
		return r.serverArchiveRetention()
	}
	return time.Duration(project.ArchiveRetentionSeconds) * time.Second
}

// archiveDeadline is when an archived sandbox stops being kept and is purged.
//
// Like the source-push deadline it is derived rather than stored: SetState
// stamps StateChangedAt once, when the sandbox first reaches `archived`, so
// every reconcile computes the same instant. Re-entering the state does not
// extend it, and a deadline that was never persisted cannot be lost.
func archiveDeadline(sb *model.Sandbox, retention time.Duration) time.Time {
	return sb.StateChangedAt.Add(retention)
}

// archive converges a sandbox toward existing as data: the provider drops the
// container and keeps the durable tree (ADR 0022 §1).
//
// It is also where retention is enforced, which is why an already-archived,
// already-converged sandbox does not simply return. Reaching this function
// again means a wake-up fired — the future-dated mark armed below, or the
// expiry backstop in ScanDirty — and the only question left is whether the
// sandbox has been archived long enough to purge. If it has, this records
// delete intent and lets the ordinary delete path do the work.
func (r *SandboxReconciler) archive(ctx context.Context, sandbox *model.Sandbox, generation int64) (reconcile.Result, error) {
	// A settled failure needs new intent, not another attempt (ADR 0017 §4).
	if sandbox.Converged() && sandbox.ErrorMessage != nil {
		return reconcile.Result{}, nil
	}

	retention := r.projectArchiveRetention(ctx, sandbox.ProjectID)

	if sandbox.State == model.SandboxStateArchived {
		if !time.Now().Before(archiveDeadline(sandbox, retention)) {
			return reconcile.Result{}, r.expireArchive(ctx, sandbox, generation)
		}
		if sandbox.Converged() {
			return r.armArchiveExpiry(sandbox, retention), nil
		}
	}

	if err := r.archiveSandbox(ctx, sandbox); err != nil {
		if teardownNotAnswered(ctx, err) {
			return reconcile.Result{}, err
		}
		sandbox.ObservedGeneration = generation
		sandbox.RecordFailure(model.SandboxStateFailed, err.Error(), failureReason(err))
		if updateErr := r.update(ctx, sandbox, generation); updateErr != nil {
			return reconcile.Result{}, updateErr
		}
		return reconcile.Result{}, err
	}

	sandbox.ObservedGeneration = generation
	sandbox.SetState(model.SandboxStateArchived)
	sandbox.ClearFailure()
	if err := r.update(ctx, sandbox, generation); err != nil {
		return reconcile.Result{}, err
	}
	return r.armArchiveExpiry(sandbox, retention), nil
}

// expireArchive turns an expired archive into a delete. It writes the intent
// the same way a user's purge would — generation bump, desired state, dirty
// mark, one transaction — so retention is not a second deletion path but the
// policy that submits the ordinary one on the user's behalf.
func (r *SandboxReconciler) expireArchive(ctx context.Context, sandbox *model.Sandbox, generation int64) error {
	updated, err := r.recordDeleteIntent(ctx, sandbox, generation)
	if err != nil {
		return err
	}
	return r.delete(ctx, updated, updated.Generation)
}

// armArchiveExpiry is the reconcile that enforces retention, expressed as the
// engine's timer. An archived sandbox has no other edge — its generations agree
// and nothing observes it — so without this it would wait on the 60s scan
// backstop rather than waking at the deadline itself.
//
// A zero StateChangedAt has no deadline to arm, and settles: the scan's
// ListArchivedSandboxRefsExpiredBefore skips those rows for the same reason.
func (r *SandboxReconciler) armArchiveExpiry(sb *model.Sandbox, retention time.Duration) reconcile.Result {
	if sb.StateChangedAt.IsZero() {
		return reconcile.Result{}
	}
	return reconcile.RequeueAt(archiveDeadline(sb, retention))
}

// teardownNotAnswered is whether an archive, or the archive a repair runs
// before it rebuilds, failed without the pool's answer to it: the pool was not
// reachable, its agent is older than this server (the window of an upgrade,
// which ends when the pool's reconcile replaces it), or the reconcile's context
// ended. None is the reconciler giving up on the intent, so none settles it
// (ADR 0017 §3): the generation stays unobserved and the engine retries with
// backoff. A pool is unreachable for a while after every server restart, until
// the pool reconciler brings its host back, and an archive accepted just
// before a shutdown is retried in exactly that window; settled there, it was
// left `failed` with nothing to drive it again (#1043). The pool's own refusal
// still settles, as any other failure does. A delete never settles at all
// (SandboxReconciler.delete).
func teardownNotAnswered(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, sandbox.ErrPoolNotReachable) || errors.Is(err, ErrPoolAgentUnsupported)
}

// archiveSandbox is the provider call. A sandbox with no provider — one that
// never reached a runtime — archives trivially: there is nothing to tear down,
// and the row is what is being retained.
func (r *SandboxReconciler) archiveSandbox(ctx context.Context, sb *model.Sandbox) error {
	provider, err := r.resolveProvider(ctx, sb)
	if err != nil {
		return err
	}
	if provider == nil {
		return nil
	}
	secretState, err := r.store.OpenSandboxSecretState(ctx, sb)
	if err != nil {
		return err
	}
	state, err := provider.Archive(ctx, sandboxRefFromSandbox(sb), secretState)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	sb.SecretState = state
	return nil
}

// recordDeleteIntent bumps the generation and asks for deletion, guarded by the
// generation the caller was working against. It returns the sandbox as stored,
// so the caller converges against the intent it just wrote rather than a stale
// copy.
func (r *SandboxReconciler) recordDeleteIntent(ctx context.Context, sandbox *model.Sandbox, generation int64) (*model.Sandbox, error) {
	sandbox.IncrementGeneration()
	sandbox.RecordIntent(model.DesiredStateDeleted)
	if err := r.update(ctx, sandbox, generation); err != nil {
		return nil, err
	}
	return sandbox, nil
}
