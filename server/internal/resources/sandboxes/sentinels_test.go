package sandboxes

import (
	"context"
	"errors"
	"testing"

	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// sentinelFixture is a placed sandbox holding one injected secret whose
// routes send github.com unjudged.
func sentinelFixture(t *testing.T) (*Service, *recordingProvider, *model.Secret) {
	t.Helper()
	ctx := context.Background()
	svc, rec := newAssignFixture(t)
	st := svc.store
	sec := bearerSecret(t, st, "github", "")
	if _, err := sec.ReplaceRoutes([]model.SecretRoute{{Host: "github.com"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSecret(ctx, sec); err != nil {
		t.Fatal(err)
	}
	deliver(t, st, "sb-1", sec.ID, "GH_TOKEN", "SENT-1")
	sb, err := st.GetSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	sb.ProviderState = []byte(`{"poolId":"pool-1"}`)
	if err := st.UpdateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	return svc, rec, sec
}

// A delivery the pool does not take is returned, so the engine retries it,
// and it leaves the sandbox as it was: a pool unreachable for a moment is not
// a failed sandbox. The retry delivers the sentinel with its routes.
func TestSentinelDeliveryRetriesWithoutFailingTheSandbox(t *testing.T) {
	ctx := context.Background()
	svc, rec, _ := sentinelFixture(t)
	rec.updateErrs = []error{errors.New("pool unreachable")}
	r := svc.NewSentinelReconciler()
	id := SandboxDirtyID("project-1", "sb-1")

	if _, err := r.Reconcile(ctx, id); err == nil {
		t.Fatal("Reconcile succeeded with the pool refusing, want its error returned for a retry")
	}
	sb, err := svc.store.GetSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	if sb.ErrorMessage != nil || sb.State == model.SandboxStateFailed {
		t.Fatalf("sandbox state = %q, error = %v, want it untouched by a failed delivery", sb.State, sb.ErrorMessage)
	}

	if _, err := r.Reconcile(ctx, id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(rec.updated) != 1 || len(rec.updated[0]) != 1 || rec.updated[0][0] != "SENT-1" {
		t.Fatalf("updated = %v, want the sentinel set delivered on the retry", rec.updated)
	}
	if got := rec.routes[0]["SENT-1"]; len(got) != 1 || len(got[0]) != 1 || got[0][0].Host != "github.com" {
		t.Fatalf("routes = %v, want the secret's routes delivered with it", rec.routes)
	}
}

// Nothing is sent for a sandbox going away, or for one no pool holds yet:
// the create that places it sends its sentinels and checks them after.
func TestSentinelDeliverySkipsWhatHasNoPoolToSendTo(t *testing.T) {
	ctx := context.Background()
	svc, rec, _ := sentinelFixture(t)
	r := svc.NewSentinelReconciler()
	id := SandboxDirtyID("project-1", "sb-1")
	sb, err := svc.store.GetSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}

	sb.DesiredState = model.DesiredStateArchived
	if err := svc.store.UpdateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if result, err := r.Reconcile(ctx, id); err != nil || !result.RequeueAt.IsZero() {
		t.Fatalf("archived: result = %+v, err = %v, want settled", result, err)
	}

	sb.DesiredState = model.DesiredStatePresent
	sb.ProviderState = nil
	if err := svc.store.UpdateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if result, err := r.Reconcile(ctx, id); err != nil || !result.RequeueAt.IsZero() {
		t.Fatalf("never placed: result = %+v, err = %v, want settled", result, err)
	}
	if len(rec.updated) != 0 {
		t.Fatalf("updated = %v, want nothing sent", rec.updated)
	}
}

// A pool that holds no container for the sandbox answers not found, and
// nothing there holds its sentinels: the create that builds one carries them,
// so the delivery settles rather than retrying until the rebuild.
func TestSentinelDeliverySettlesWhenThePoolHasNoContainer(t *testing.T) {
	svc, rec, _ := sentinelFixture(t)
	rec.updateErrs = []error{ErrNotFound}

	result, err := svc.NewSentinelReconciler().Reconcile(context.Background(), SandboxDirtyID("project-1", "sb-1"))
	if err != nil || !result.RequeueAt.IsZero() {
		t.Fatalf("result = %+v, err = %v, want settled", result, err)
	}
}

// A send that carried routes a change has since replaced marks the delivery
// again, so a create that read the routes before the change and landed after
// its delivery cannot leave the pool holding the old ones. One that carried
// the routes as they are marks nothing.
func TestASendThatCarriedStaleRoutesMarksTheDeliveryAgain(t *testing.T) {
	ctx, svc, st, _ := createIntentFixture(t)
	if err := st.CreateSandbox(ctx, &model.Sandbox{ID: "sb-1", ProjectID: "project-1", CreatedByUserID: "user-1", Name: "sb-1", PoolID: "pool-1"}); err != nil {
		t.Fatal(err)
	}
	sec := bearerSecret(t, st, "github", "")
	if _, err := sec.ReplaceRoutes([]model.SecretRoute{{Host: "github.com"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSecret(ctx, sec); err != nil {
		t.Fatal(err)
	}
	deliver(t, st, "sb-1", sec.ID, "GH_TOKEN", "SENT-1")
	sb, err := st.GetSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	current := map[string][][]secretroute.Route{"SENT-1": {{{Host: "github.com"}}}}
	if err := markSentinelsIfStale(ctx, st, svc.engine, sb, current); err != nil {
		t.Fatal(err)
	}
	if dirty, _ := svc.engine.ListDirty(ctx, SentinelsResourceType); len(dirty) != 0 {
		t.Fatalf("dirty = %v, want nothing for a send that carried the routes as they are", dirty)
	}
	if err := markSentinelsIfStale(ctx, st, svc.engine, sb, nil); err != nil {
		t.Fatal(err)
	}
	dirty, err := svc.engine.ListDirty(ctx, SentinelsResourceType)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 1 || dirty[0].ResourceID != SandboxDirtyID("project-1", "sb-1") {
		t.Fatalf("dirty = %v, want the sandbox's delivery marked again", dirty)
	}
}
