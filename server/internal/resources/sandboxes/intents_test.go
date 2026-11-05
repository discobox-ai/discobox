package sandboxes

import (
	"context"
	"errors"
	"net/http"
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

// An archive does not take back a delete still converging — a purge that
// failed and retries, or the retention's — whoever asks for it: it is accepted
// and records nothing, so a discobox archiving one it created may not keep a
// sandbox past a person's purge (ADR 26-11-03-724 §1). Any other intent over
// the delete is refused.
func TestNoIntentTakesBackASandboxBeingDeleted(t *testing.T) {
	ctx, svc, st, _ := createIntentFixture(t)
	sandbox := &model.Sandbox{ID: "sb-1", ProjectID: "project-1", CreatedByUserID: "user-1", Name: "sb-1", PoolID: "pool-1"}
	sandbox.DesiredState = model.DesiredStateDeleted
	if err := st.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	if err := svc.DeleteSandbox(ctx, sandbox.ProjectID, sandbox.ID); err != nil {
		t.Fatalf("archive of a sandbox being deleted = %v, want it accepted", err)
	}
	_, err := svc.recordSandboxIntent(ctx, sandbox.ProjectID, sandbox.ID, model.DesiredStatePresent, false)
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusConflict {
		t.Fatalf("present over a sandbox being deleted = %v, want 409", err)
	}
	stored, err := st.GetSandbox(ctx, sandbox.ProjectID, sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if stored.DesiredState != model.DesiredStateDeleted || stored.Generation != sandbox.Generation {
		t.Fatalf("desired state = %s at generation %d, want the delete left as it was at %d", stored.DesiredState, stored.Generation, sandbox.Generation)
	}
}
