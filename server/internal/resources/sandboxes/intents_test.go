package sandboxes

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// The dirty mark wakes the engine on commit, so by the time recordSandboxIntent
// returns the engine may already have converged a delete and dropped the row.
// The intent was accepted all the same, and a purge whose row the engine
// removed first has done what it was asked: it is not a 404 (issue #204).
func TestPurgeSandboxSucceedsWhenTheEngineRemovesTheRowFirst(t *testing.T) {
	ctx, svc, st, db := createIntentFixture(t)
	sandbox := &model.Sandbox{ID: "sb-1", ProjectID: "project-1", CreatedByUserID: "user-1", Name: "sb-1", PoolID: "pool-1"}
	if err := st.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	removeOnFirstReadAfterCommit(ctx, t, db, st, sandbox)

	if err := svc.PurgeSandbox(ctx, sandbox.ProjectID, sandbox.ID); err != nil {
		t.Fatalf("purge sandbox: %v, want success for a purge the engine finished first", err)
	}
	if _, err := st.GetSandbox(ctx, sandbox.ProjectID, sandbox.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get purged sandbox = %v, want not found", err)
	}
}

// removeOnFirstReadAfterCommit deletes the sandbox's row just before the first
// read of it made outside a transaction — the earliest moment after an intent
// commits that the engine's reconcile could have finished. It makes the
// interleaving certain instead of depending on how busy the machine is.
func removeOnFirstReadAfterCommit(ctx context.Context, t *testing.T, db *database.DB, st *store.Store, sandbox *model.Sandbox) {
	t.Helper()
	var armed atomic.Bool
	armed.Store(true)
	remove := func(tx *gorm.DB) {
		if tx.Statement.Table != "sandboxes" {
			return
		}
		if _, inTransaction := tx.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
			return
		}
		if !armed.CompareAndSwap(true, false) {
			return
		}
		if err := st.DeleteSandbox(ctx, sandbox.ProjectID, sandbox.ID); err != nil {
			_ = tx.AddError(err)
		}
	}
	conns := []*gorm.DB{db.Write}
	if db.Read.Config != db.Write.Config {
		conns = append(conns, db.Read)
	}
	for _, conn := range conns {
		if err := conn.Callback().Query().Before("gorm:query").Register("test:remove_sandbox_first", remove); err != nil {
			t.Fatalf("register query callback: %v", err)
		}
	}
}
