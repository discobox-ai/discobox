package store_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/encryption"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func TestPoolProxyCredentialsAreSealedAndBoundToTheirPool(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s := store.New(db.Write, db.Read, store.WithKeyring(localKeyring(t, db, newLocalKey(t))))
	pool := &model.Pool{ID: "pool-one", ProjectID: "project-one"}
	//nolint:gosec // Synthetic fixture verifies proxy credential handling.
	intent := &pooloutbound.Intent{Mode: "explicit", HTTP: "http://user:private@proxy:3128"}
	if err := s.SetPoolProxy(ctx, pool, intent); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(pool.ProxyIntent, []byte("private")) || !encryption.Sealed(pool.ProxyIntent) {
		t.Fatal("proxy credentials are unsealed")
	}
	got, err := s.OpenPoolProxy(ctx, pool)
	if err != nil || got.HTTP != intent.HTTP {
		t.Fatalf("read sealed override: %v", err)
	}
	pool.ID = "pool-other"
	if _, err := s.OpenPoolProxy(ctx, pool); err == nil {
		t.Fatal("sealed intent opened as another pool")
	}
}
