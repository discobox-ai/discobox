package service

import (
	"context"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
)

func TestStartRequiresNewPoolHealthReports(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, db := newProviderInstanceTestStore(ctx, t)
	// Disabled providers are still invalidated, without booting a real host.
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Name: "test", Type: "docker", Disabled: true}
	if err := st.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1", RegisteredAt: &now,
		PoolManifest:      model.PoolManifest{Name: "Default", ProviderInstanceID: provider.ID},
		ResourceLifecycle: model.ResourceLifecycle{State: model.PoolStateActive, DesiredState: model.DesiredStatePresent}}
	if err := st.CreatePool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdatePoolStatus(ctx, pool.ID, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	engine, err := reconcile.New(db.Write, reconcile.Options{SingleNode: true})
	if err != nil {
		t.Fatal(err)
	}
	svc := New(st, engine, Options{})
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := svc.Stop(ctx); err != nil {
			t.Error(err)
		}
	}()
	pool, err = st.GetPoolByID(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pool.Health(time.Now()) != model.PoolHealthUnknown || pool.IsReady() {
		t.Fatalf("service started trusting old health: %+v", pool)
	}
	if pool.RegisteredAt == nil || pool.State != model.PoolStateActive {
		t.Fatal("startup discarded lifecycle or registration")
	}
	pool, err = st.UpdatePoolStatus(ctx, pool.ID, true, true, false, 1, 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !pool.IsReady() {
		t.Fatal("fresh status did not recover health")
	}
}
