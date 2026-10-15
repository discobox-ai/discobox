package sandboxes

import (
	"errors"
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// A change to a secret's routes reaches a running sandbox through its
// sentinel delivery (ADR 26-10-02-924): every sandbox the secret is injected
// into is marked in the transaction that wrote them, and nothing else is.
func TestMarkSecretHoldersDirtyTxMarksTheSandboxesTheSecretIsInjectedInto(t *testing.T) {
	ctx, svc, st, _ := createIntentFixture(t)
	for _, sandboxID := range []string{"sb-1", "sb-2", "sb-3", "sb-4"} {
		if err := st.CreateSandbox(ctx, &model.Sandbox{ID: sandboxID, ProjectID: "project-1", CreatedByUserID: "user-1", Name: sandboxID, PoolID: "pool-1"}); err != nil {
			t.Fatalf("create sandbox %s: %v", sandboxID, err)
		}
	}
	for _, assignment := range []*model.SandboxSecret{
		{ProjectID: "project-1", SandboxID: "sb-1", SecretID: "sec-1", EnvName: "TOKEN", Sentinel: "sentinel-1"},
		{ProjectID: "project-1", SandboxID: "sb-2", SecretID: "sec-1", EnvName: "TOKEN", Sentinel: "sentinel-2"},
		// An agent's credential reaches its sandbox as an activation, not as a
		// registered sentinel, so its holder has nothing to re-register.
		{ProjectID: "project-1", SandboxID: "sb-3", SecretID: "sec-1", EnvName: "AGENT_TOKEN", Sentinel: "sentinel-3", AgentRequested: true},
		{ProjectID: "project-1", SandboxID: "sb-4", SecretID: "sec-2", EnvName: "TOKEN", Sentinel: "sentinel-4"},
	} {
		if err := st.CreateSandboxSecret(ctx, assignment); err != nil {
			t.Fatalf("create assignment: %v", err)
		}
	}

	if err := st.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		return svc.MarkSecretHoldersDirtyTx(ctx, txStore, txDB, "project-1", "sec-1")
	}); err != nil {
		t.Fatalf("mark holders: %v", err)
	}

	dirty, err := svc.engine.ListDirty(ctx, SentinelsResourceType)
	if err != nil {
		t.Fatalf("list dirty: %v", err)
	}
	got := make([]string, 0, len(dirty))
	for _, row := range dirty {
		got = append(got, row.ResourceID)
	}
	slices.Sort(got)
	want := []string{SandboxDirtyID("project-1", "sb-1"), SandboxDirtyID("project-1", "sb-2")}
	if !slices.Equal(got, want) {
		t.Fatalf("dirty = %v, want %v", got, want)
	}
}

// A mark that does not commit is not a mark: the routes and their delivery
// commit together or not at all.
func TestMarkSecretHoldersDirtyTxRollsBackWithItsTransaction(t *testing.T) {
	ctx, svc, st, _ := createIntentFixture(t)
	if err := st.CreateSandbox(ctx, &model.Sandbox{ID: "sb-1", ProjectID: "project-1", CreatedByUserID: "user-1", Name: "sb-1", PoolID: "pool-1"}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if err := st.CreateSandboxSecret(ctx, &model.SandboxSecret{ProjectID: "project-1", SandboxID: "sb-1", SecretID: "sec-1", EnvName: "TOKEN", Sentinel: "sentinel-1"}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}
	failed := gorm.ErrInvalidTransaction
	if err := st.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		if err := svc.MarkSecretHoldersDirtyTx(ctx, txStore, txDB, "project-1", "sec-1"); err != nil {
			return err
		}
		return failed
	}); !errors.Is(err, failed) {
		t.Fatalf("transaction = %v, want %v", err, failed)
	}
	dirty, err := svc.engine.ListDirty(ctx, SentinelsResourceType)
	if err != nil {
		t.Fatalf("list dirty: %v", err)
	}
	if len(dirty) != 0 {
		t.Fatalf("dirty = %v, want none after a rollback", dirty)
	}
}
