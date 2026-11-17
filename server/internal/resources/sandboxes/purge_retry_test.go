package sandboxes_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/resources/sandboxes"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A purge a server before #928 settled as failed is still a row meant to be
// deleted: the scan re-drives it, and the removal the pool takes purges it.
func TestScanRedrivesAPurgeASettledFailureStranded(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	failure := "pool agent is not reachable: pool pool-1 has no host running"
	sb := createSandboxForReconcile(t, appStore, model.ResourceLifecycle{
		DesiredState:       model.DesiredStateDeleted,
		State:              model.SandboxStateFailed,
		Generation:         2,
		ObservedGeneration: 2,
		ErrorMessage:       &failure,
	})
	id := sandboxes.SandboxDirtyID(sb.ProjectID, sb.ID)

	r := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(failingSandboxProvider{}))
	ids, err := r.ScanDirty(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ids, id) {
		t.Fatalf("scan = %v, want the stranded purge %s", ids, id)
	}
	if _, err := r.Reconcile(ctx, id); err != nil {
		t.Fatalf("reconcile the stranded purge: %v", err)
	}
	if _, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get purged sandbox = %v, want not found", err)
	}
}

// An archive whose pool's agent is older than this server is the window of an
// upgrade, not the pool's answer: it stays archiving for the engine to retry.
func TestReconcileArchiveWaitsOnAnOlderPoolAgent(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := createSandboxForReconcile(t, appStore, model.ResourceLifecycle{
		DesiredState:       model.DesiredStateArchived,
		State:              model.SandboxStateReady,
		Generation:         2,
		ObservedGeneration: 1,
	})
	id := sandboxes.SandboxDirtyID(sb.ProjectID, sb.ID)
	older := fmt.Errorf("archive sandbox: %w", sandboxes.ErrPoolAgentUnsupported)

	r := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(failingSandboxProvider{archiveErr: older}))
	if _, err := r.Reconcile(ctx, id); !errors.Is(err, sandboxes.ErrPoolAgentUnsupported) {
		t.Fatalf("reconcile on an older agent = %v, want ErrPoolAgentUnsupported for the engine to retry", err)
	}
	waiting, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if waiting.Converged() || waiting.ErrorMessage != nil || waiting.DisplayState() != "archiving" {
		t.Fatalf("archive on an older agent: state %q, observed %d of %d, error %v, display %q; want archiving, unsettled",
			waiting.State, waiting.ObservedGeneration, waiting.Generation, waiting.ErrorMessage, waiting.DisplayState())
	}
}
