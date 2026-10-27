package secrets

import (
	"context"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A grant whose lifetime is fitted to a delegation at an instant, and stamped
// from that same instant, ends when the delegation does at the latest — even
// with a whole number of seconds left at that instant, which is what a coarse
// clock gives when the fit falls in the tick the delegation's expiry was read
// in (issue #201, seen on Windows).
//
// This holds delegatedTTL and mintGrantAs to the instant they are given. That
// commitApproval gives them both one reading of the clock is not under test
// here: nothing it reads the clock through can be set by a test.
func TestAGrantFittedToADelegationEndsNoLaterThanIt(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	write := db.Write.WithContext(ctx)
	for _, row := range []any{
		&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"},
		&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "docker"},
		&model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: "provider-1"}},
		&model.Sandbox{ID: "sbx-lead", ProjectID: "project-1", Name: "lead", PoolID: "pool-1"},
		&model.Sandbox{ID: "sbx-worker", ProjectID: "project-1", Name: "worker", PoolID: "pool-1"},
	} {
		if err := write.Create(row).Error; err != nil {
			t.Fatalf("create %T: %v", row, err)
		}
	}
	st := store.New(db.Write, db.Read)
	svc := NewService(st)
	secret, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  "github",
		Type:  serverapi.CreateSecretBodyTypeToken,
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("ghp_realrealrealrealrealrealrealreal12")},
		// Long enough for the delegation's whole lifetime.
		MaxGrantTTLSeconds: serverapi.NewOptInt64(86400),
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	lapses := time.Now().UTC().Add(2 * time.Hour)
	delegation := &model.SecretGrant{
		ProjectID: "project-1", SecretID: secret.ID, Scope: model.SecretGrantScopeSandbox, ScopeKey: "sbx-lead",
		Hosts: []string{"github.com"}, GrantedBy: "user-1", Purpose: model.SecretGrantPurposeDelegate, ExpiresAt: &lapses,
	}
	if err := st.CreateSecretGrant(ctx, delegation); err != nil {
		t.Fatalf("create delegation grant: %v", err)
	}

	now := lapses.Add(-2 * time.Hour)
	ttl, err := delegatedTTL(ctx, st, "project-1", "sbx-lead", delegation, secret.ID, []string{"github.com"}, 86400, false, now)
	if err != nil {
		t.Fatalf("fit to the delegation: %v", err)
	}
	grant, err := svc.mintGrantAs(ctx, now, "project-1", secret, model.SecretGrantScopeSandbox, "sbx-worker", []string{"github.com"},
		ttl, model.SecretGrantPurposeUse, delegation.ID)
	if err != nil {
		t.Fatalf("mint the grant: %v", err)
	}
	if grant.ExpiresAt == nil || !grant.ExpiresAt.Equal(lapses) {
		t.Fatalf("grant expires %v, want %v: the delegation's whole lifetime, and not a moment past it", grant.ExpiresAt, lapses)
	}
}
