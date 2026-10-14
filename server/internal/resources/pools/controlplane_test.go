package pools

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// The dirty mark wakes the engine on commit, and the pool reconciler's delete
// drops the row. A delete whose row is gone by the time the request reads it
// again has done what it was asked: it is not a 404 (issue #204).
func TestDeletePoolSucceedsWhenTheEngineRemovesTheRowFirst(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	appStore := store.New(db.Write, db.Read)
	engine, err := reconcile.New(db.Write, reconcile.Options{SingleNode: true})
	if err != nil {
		t.Fatalf("create reconcile engine: %v", err)
	}
	if err := appStore.UpsertProject(ctx, &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "project-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "test", Name: "Test"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID}}
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	disarm := removePoolOnFirstReadAfterIntent(ctx, t, db, appStore, pool)

	svc := NewService(appStore, nil, NewControlPlane(appStore, engine))
	if err := svc.DeletePool(ctx, pool.ProjectID, pool.ID); err != nil {
		t.Fatalf("delete pool: %v, want success for a delete the engine finished first", err)
	}
	// DeletePool read nothing after its commit, so the row is still there, and
	// it carries the intent: nothing here runs the engine to converge it.
	if !disarm() {
		t.Fatal("the row was removed by a read DeletePool made after its intent committed")
	}
	deleted, err := appStore.GetPool(ctx, pool.ProjectID, pool.ID)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if deleted.DesiredState != model.DesiredStateDeleted || deleted.Generation != pool.Generation+1 {
		t.Fatalf("pool intent = %q at generation %d, want %q at %d",
			deleted.DesiredState, deleted.Generation, model.DesiredStateDeleted, pool.Generation+1)
	}
}

// removePoolOnFirstReadAfterIntent deletes the pool's row just before the
// first read of it made outside a transaction once delete intent has been
// written — the earliest moment after the intent commits that the engine's
// reconcile could have finished. It makes the interleaving certain instead of
// depending on how busy the machine is. The returned disarm stops it, and
// reports whether it was still armed: false means some read already hit it.
func removePoolOnFirstReadAfterIntent(ctx context.Context, t *testing.T, db *database.DB, appStore *store.Store, pool *model.Pool) (disarm func() bool) {
	t.Helper()
	var intentWritten, armed atomic.Bool
	armed.Store(true)
	noteIntent := func(tx *gorm.DB) {
		if tx.Statement.Table == "pools" {
			intentWritten.Store(true)
		}
	}
	remove := func(tx *gorm.DB) {
		if tx.Statement.Table != "pools" || !intentWritten.Load() {
			return
		}
		if _, inTransaction := tx.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
			return
		}
		if !armed.CompareAndSwap(true, false) {
			return
		}
		if err := appStore.DeletePool(ctx, pool.ProjectID, pool.ID); err != nil {
			_ = tx.AddError(err)
		}
	}
	conns := []*gorm.DB{db.Write}
	if db.Read.Config != db.Write.Config {
		conns = append(conns, db.Read)
	}
	for _, conn := range conns {
		if err := conn.Callback().Update().After("gorm:update").Register("test:note_pool_intent", noteIntent); err != nil {
			t.Fatalf("register update callback: %v", err)
		}
		if err := conn.Callback().Query().Before("gorm:query").Register("test:remove_pool_first", remove); err != nil {
			t.Fatalf("register query callback: %v", err)
		}
	}
	return func() bool { return armed.CompareAndSwap(true, false) }
}
