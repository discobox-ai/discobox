package sandboxes_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/resources/sandboxes"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// unreachablePoolArchiveProvider answers the first archive the way a pool
// whose host is not back yet does after a server restart, and archives
// normally once the host is up.
type unreachablePoolArchiveProvider struct {
	archiveTestProvider
	unreachable int
}

func (p *unreachablePoolArchiveProvider) Archive(ctx context.Context, ref sandboxes.SandboxRef, state []byte) ([]byte, error) {
	if p.unreachable > 0 {
		p.unreachable--
		p.archiveCalls++
		return nil, fmt.Errorf("%w: pool pool-1 has no host running: libkrun VM pool-1: sandbox not found", sandbox.ErrPoolNotReachable)
	}
	return p.archiveTestProvider.Archive(ctx, ref, state)
}

// An archive whose pool is not reachable yet is not finished (ADR 0017 §3):
// it must not settle as a failure, or nothing re-drives it once the pool's
// host is back and the sandbox stays in error with desired=archived.
func TestReconcileArchiveRetriesWhilePoolIsNotReachable(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := archivableSandbox(ctx, t, appStore, model.SandboxStateReady, 0)

	provider := &unreachablePoolArchiveProvider{unreachable: 1}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))
	if _, err := reconciler.ReconcileSandbox(ctx, sb); !errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("reconcile with an unreachable pool = %v, want an ErrPoolNotReachable the engine retries", err)
	}

	afterFirst, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if afterFirst.Converged() && afterFirst.ErrorMessage != nil {
		t.Fatalf("unreachable pool settled the archive as a failure (state %q, error %q); nothing will retry it",
			afterFirst.State, *afterFirst.ErrorMessage)
	}

	// Not settled is not enough: an error recorded on the row reads as `error`,
	// which is what the person saw.
	if got := afterFirst.DisplayState(); got != "archiving" {
		t.Fatalf("display state while waiting for the pool = %q, want archiving", got)
	}

	// The pool's host is back: the engine's retry reconciles the stored row.
	if _, err := reconciler.ReconcileSandbox(ctx, afterFirst); err != nil {
		t.Fatalf("retry archive: %v", err)
	}
	updated, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox after retry: %v", err)
	}
	if updated.State != model.SandboxStateArchived {
		t.Fatalf("state = %q, want archived; archive calls = %d", updated.State, provider.archiveCalls)
	}
}

// unreachablePoolRemoveProvider answers the first purge as a pool whose host
// is not back yet, and removes normally once it is.
type unreachablePoolRemoveProvider struct {
	archiveTestProvider
	unreachable int
}

func (p *unreachablePoolRemoveProvider) Remove(ctx context.Context, ref sandboxes.SandboxRef, state []byte) ([]byte, error) {
	if p.unreachable > 0 {
		p.unreachable--
		p.removeCalls++
		return nil, fmt.Errorf("%w: pool pool-1 has no host running", sandbox.ErrPoolNotReachable)
	}
	return p.archiveTestProvider.Remove(ctx, ref, state)
}

// A purge interrupted the same way is not finished either: it stays a delete
// in progress, and the retry once the pool is back removes the row.
func TestReconcileDeleteRetriesWhilePoolIsNotReachable(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := createSandboxForReconcile(t, appStore, model.ResourceLifecycle{
		DesiredState:       model.DesiredStateDeleted,
		State:              model.SandboxStateReady,
		Generation:         2,
		ObservedGeneration: 1,
	})

	provider := &unreachablePoolRemoveProvider{unreachable: 1}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))
	if _, err := reconciler.ReconcileSandbox(ctx, sb); !errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("reconcile with an unreachable pool = %v, want an ErrPoolNotReachable the engine retries", err)
	}
	afterFirst, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if afterFirst.Converged() || afterFirst.ErrorMessage != nil || afterFirst.State == model.SandboxStateFailed {
		t.Fatalf("unreachable pool settled the delete (state %q, observed %d of %d, error %v)",
			afterFirst.State, afterFirst.ObservedGeneration, afterFirst.Generation, afterFirst.ErrorMessage)
	}
	if got := afterFirst.DisplayState(); got != "deleting" {
		t.Fatalf("display state while waiting for the pool = %q, want deleting", got)
	}

	if _, err := reconciler.ReconcileSandbox(ctx, afterFirst); err != nil {
		t.Fatalf("retry delete: %v", err)
	}
	if _, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get sandbox after retry = %v, want not found", err)
	}
}

// endedContextArchiveProvider fails the archive the way a server shutting
// down does: its context ends under the call.
type endedContextArchiveProvider struct {
	archiveTestProvider
	cancel context.CancelFunc
}

func (p *endedContextArchiveProvider) Archive(ctx context.Context, _ sandboxes.SandboxRef, _ []byte) ([]byte, error) {
	p.archiveCalls++
	p.cancel()
	return nil, ctx.Err()
}

// An archive cut off by the server's own shutdown is not the reconciler giving
// up either: the row is left as it was, for the next server run to finish.
func TestReconcileArchiveInterruptedByShutdownIsNotSettled(t *testing.T) {
	appStore := newExecutorTestStore(t)
	sb := archivableSandbox(context.Background(), t, appStore, model.SandboxStateReady, 0)

	ctx, cancel := context.WithCancel(context.Background())
	provider := &endedContextArchiveProvider{cancel: cancel}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))
	if _, err := reconciler.ReconcileSandbox(ctx, sb); !errors.Is(err, context.Canceled) {
		t.Fatalf("reconcile under an ended context = %v, want context.Canceled", err)
	}
	stored, err := appStore.GetSandbox(context.Background(), sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if stored.Converged() || stored.ErrorMessage != nil || stored.DisplayState() != "archiving" {
		t.Fatalf("interrupted archive settled: state %q, observed %d of %d, error %v, display %q",
			stored.State, stored.ObservedGeneration, stored.Generation, stored.ErrorMessage, stored.DisplayState())
	}
}

// A repair tears down with the same archive call, and its teardown waits for
// the pool the same way: settled, it would leave the sandbox failed with the
// repair it asked for spent.
func TestReconcileRepairTeardownRetriesWhilePoolIsNotReachable(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := repairableSandbox(ctx, t, appStore, model.SandboxStateReady)
	recordRepairIntent(ctx, t, appStore, sb)

	provider := &unreachablePoolArchiveProvider{unreachable: 1}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))
	if _, err := reconciler.ReconcileSandbox(ctx, sb); !errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("reconcile repair with an unreachable pool = %v, want ErrPoolNotReachable", err)
	}
	afterFirst, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if afterFirst.Converged() || afterFirst.ErrorMessage != nil || afterFirst.DisplayState() != "repairing" {
		t.Fatalf("unreachable pool settled the repair: state %q, observed %d of %d, error %v, display %q",
			afterFirst.State, afterFirst.ObservedGeneration, afterFirst.Generation, afterFirst.ErrorMessage, afterFirst.DisplayState())
	}
	if provider.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0: the rebuild must wait for its teardown", provider.createCalls)
	}

	if _, err := reconciler.ReconcileSandbox(ctx, afterFirst); err != nil {
		t.Fatalf("retry repair: %v", err)
	}
	if provider.archiveCalls != 2 || provider.createCalls != 1 {
		t.Fatalf("repair after the pool is back: archive=%d create=%d, want 2 and 1", provider.archiveCalls, provider.createCalls)
	}
	repaired, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox after retry: %v", err)
	}
	if !repaired.Converged() || repaired.ErrorMessage != nil {
		t.Fatalf("repair did not converge: observed %d of %d, error %v", repaired.ObservedGeneration, repaired.Generation, repaired.ErrorMessage)
	}
}
