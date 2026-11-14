package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A rejection is judged against the secret as it was read, and the renewal it
// forces reads the row again under the singleflight. A person saving a new
// login between those two reads has replaced the value that was refused: the
// forced renewal must not spend the new login's refresh token on a refusal it
// never drew, nor be remembered against the old value (ADR 26-11-11-539 §2).
// Nothing is learned, and the new login's own first refusal is judged afresh.
func TestAForcedRenewalOfAValueReplacedSinceItWasRefusedRenewsNothing(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{
		ID: "project-1", OwnerUserID: "user-1", Name: "Project",
	}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	st := store.New(db.Write, db.Read)
	svc := NewService(st)

	var minted atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		minted.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "rotated", "refresh_token": "refresh-rotated",
			"expires_in": 3600, "token_type": "Bearer",
		})
	}))
	t.Cleanup(endpoint.Close)

	value := func(access, refresh string) []byte {
		t.Helper()
		//nolint:gosec // Test marshals a secret value before store encryption.
		b, err := json.Marshal(model.SecretValue{
			Token: access, RefreshToken: refresh, TokenURL: endpoint.URL, ClientID: "client-1",
			AccessTokenExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	sec := &model.Secret{
		ProjectID: "project-1", Name: "claude", Type: model.SecretTypeOAuth,
		Routes:        []model.SecretRoute{{Hosts: []string{"*"}}},
		MaxBindingTTL: 3600, EncryptedValue: value("old-access", "old-refresh"),
	}
	if err := st.CreateSecret(ctx, sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	refused, err := st.GetSecret(ctx, "project-1", sec.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The new login is saved after the refusal was read and before its
	// renewal runs.
	replacement, err := st.GetSecret(ctx, "project-1", sec.ID)
	if err != nil {
		t.Fatal(err)
	}
	replacement.EncryptedValue = value("new-access", "new-refresh")
	if err := st.UpdateSecret(ctx, replacement); err != nil {
		t.Fatalf("replace value: %v", err)
	}
	stored, err := st.GetSecret(ctx, "project-1", sec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ValueGeneration() == refused.ValueGeneration() {
		t.Fatalf("generation = %q after a replacement, want it moved", stored.ValueGeneration())
	}

	if got := svc.forceRenew(ctx, refused); got != renewalUnavailable {
		t.Fatalf("forceRenew = %v, want renewalUnavailable: the refused value has been replaced", got)
	}
	if n := minted.Load(); n != 0 {
		t.Fatalf("renewals = %d, want none: the new login's refresh token was spent on the old one's refusal", n)
	}
	if svc.renewedRecently(stored) {
		t.Fatal("the new login is remembered as renewed, but no renewal was spent on it")
	}
}
