package sandboxes_test

import (
	"context"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/resources/sandboxes"
)

// A push-delivered sandbox parks at `awaiting_source` and resumes from there
// once the client reports the push. The resume has to leave that state: the
// reconcile that materializes the pushed source used to re-park the sandbox it
// had just filled, because it asked what state the sandbox was in rather than
// whether the push was still outstanding. The sandbox then ran, with its source
// in place, while reporting itself as still provisioning forever — and anything
// waiting for it to become usable waited for good.
func TestReconcileLeavesAwaitingSourceOnceTheSourceIsDelivered(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		delivered bool
		wantState string
	}{
		{name: "push still outstanding", delivered: false, wantState: model.SandboxStateAwaitingSource},
		{name: "push reported complete", delivered: true, wantState: model.SandboxStateReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			appStore := newExecutorTestStore(t)
			sb := createSandboxForReconcile(t, appStore, model.ResourceLifecycle{
				DesiredState:       model.DesiredStatePresent,
				State:              model.SandboxStateAwaitingSource,
				Generation:         2,
				ObservedGeneration: 1,
			})
			sb.Source = &model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryPush}
			// The park deadline is derived from this anchor, so a zero one reads
			// as a sandbox that has been waiting since the epoch.
			sb.StateChangedAt = time.Now().UTC()
			if tc.delivered {
				now := time.Now().UTC()
				sb.SourceDeliveredAt = &now
			}
			if err := appStore.UpdateSandbox(ctx, sb); err != nil {
				t.Fatalf("update sandbox: %v", err)
			}

			reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(&recordingCreateProvider{}))
			if _, err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			updated, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
			if err != nil {
				t.Fatalf("get sandbox: %v", err)
			}
			if updated.State != tc.wantState {
				t.Fatalf("state = %q, want %q", updated.State, tc.wantState)
			}
			if !updated.Converged() {
				t.Fatalf("generations = %d/%d, want converged either way", updated.ObservedGeneration, updated.Generation)
			}
		})
	}
}
