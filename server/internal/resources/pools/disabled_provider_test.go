package pools

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A disabled provider takes no new pools (ADR 26-11-22-507 §1): the create
// is refused as a conflict, and nothing is stored.
func TestCreatePoolOnADisabledProviderIsRefused(t *testing.T) {
	svc := newSizeFixture(t)
	ctx := context.Background()
	if err := svc.store.CreateSandboxProviderInstance(ctx, &model.SandboxProviderInstance{ID: "prov-off", ProjectID: "project-1", Type: unsizedType, Name: "Off", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CreatePool(ctx, "project-1", services.CreatePoolBody{Name: "late", ProviderInstanceId: "prov-off"})
	var status apperrors.StatusError
	if !errors.As(err, &status) || status.Status != http.StatusConflict || !strings.Contains(err.Error(), "Off") {
		t.Fatalf("create on a disabled provider = %v, want a 409 naming the provider", err)
	}
	pools, err := svc.store.ListPools(ctx, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 0 {
		t.Fatalf("stored %d pools, want none", len(pools))
	}
}

// removingPoolProvider is a scripted pool provider that also records removes.
type removingPoolProvider struct {
	scriptedPoolProvider
}

func (p *removingPoolProvider) RemovePool(context.Context, sandbox.PoolManager, *model.Project, *model.SandboxProviderInstance, *model.Pool) error {
	p.calls = append(p.calls, "remove")
	return p.fail
}

type disabledProviderFixture struct {
	*bootstrapRetryFixture
	runtime *removingPoolProvider
}

// newDisabledProviderFixture is a never-registered pool on a provider of a
// type this server builds, disabled after the pool was made.
func newDisabledProviderFixture(t *testing.T) *disabledProviderFixture {
	t.Helper()
	f := newBootstrapRetryFixture(t)
	runtime := &removingPoolProvider{}
	manager := sandbox.NewProviderManager(f.store)
	manager.RegisterProvider("stub", runtime)
	f.reconciler = NewPoolReconciler(f.store, manager, NewControlPlane(f.store, manager, nil), nil)
	if err := f.db.Model(&model.SandboxProviderInstance{}).Where("id = ?", "provider-1").UpdateColumn("disabled", true).Error; err != nil {
		t.Fatal(err)
	}
	return &disabledProviderFixture{bootstrapRetryFixture: f, runtime: runtime}
}

func (f *disabledProviderFixture) deletePool() {
	f.t.Helper()
	f.column("desired_state", model.DesiredStateDeleted)
}

func (f *disabledProviderFixture) poolGone() bool {
	f.t.Helper()
	_, err := f.store.GetPool(context.Background(), "project-1", "pool-1")
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		f.t.Fatal(err)
	}
	return errors.Is(err, store.ErrNotFound)
}

// A pool on a disabled provider keeps working: its reconcile calls its
// runtime as on an enabled provider.
func TestPoolOnADisabledProviderReconciles(t *testing.T) {
	f := newDisabledProviderFixture(t)
	if err := f.reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !slices.Contains(f.runtime.calls, "reconcile") {
		t.Fatalf("calls = %v, want the pool's runtime converged", f.runtime.calls)
	}
}

// Deleting a pool on a disabled provider removes its runtime before its row:
// nothing else would ever find what it left running.
func TestDeletingAPoolOnADisabledProviderRemovesItsRuntime(t *testing.T) {
	f := newDisabledProviderFixture(t)
	f.deletePool()
	if err := f.reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !slices.Equal(f.runtime.calls, []string{"remove"}) {
		t.Fatalf("calls = %v, want the pool's runtime removed", f.runtime.calls)
	}
	if !f.poolGone() {
		t.Fatal("pool row kept after its runtime was removed")
	}
}

// A provider of a type this server cannot build has nothing of its pools to
// reach: deleting one drops the row alone (ADR 26-11-22-507 §2).
func TestDeletingAPoolOnAProviderThatCannotBeBuiltDropsTheRowAlone(t *testing.T) {
	f := newDisabledProviderFixture(t)
	if err := f.db.Model(&model.SandboxProviderInstance{}).Where("id = ?", "provider-1").UpdateColumn("type", "unsupported").Error; err != nil {
		t.Fatal(err)
	}
	f.deletePool()
	if err := f.reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.runtime.calls) != 0 {
		t.Fatalf("calls = %v, want none", f.runtime.calls)
	}
	if !f.poolGone() {
		t.Fatal("pool row kept, want it dropped")
	}
}

// A type this server builds whose build fails is a failure, not "cannot be
// built": the delete retries and the row stays, disabled or not.
func TestDeletingAPoolWhoseProviderFailsToBuildRetries(t *testing.T) {
	f := newDisabledProviderFixture(t)
	manager := sandbox.NewProviderManager(f.store)
	manager.RegisterFactory("built", func(context.Context, *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return nil, errors.New("the service refused the key")
	})
	f.reconciler = NewPoolReconciler(f.store, manager, NewControlPlane(f.store, manager, nil), nil)
	if err := f.db.Model(&model.SandboxProviderInstance{}).Where("id = ?", "provider-1").UpdateColumn("type", "built").Error; err != nil {
		t.Fatal(err)
	}
	f.deletePool()
	if err := f.reconcile(); err == nil || !strings.Contains(err.Error(), "refused the key") {
		t.Fatalf("reconcile = %v, want the build's error", err)
	}
	if f.poolGone() {
		t.Fatal("pool row dropped on a build error, want it kept for the retry")
	}
}
