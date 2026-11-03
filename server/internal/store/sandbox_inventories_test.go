package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/encryption"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func inventoryTestSandbox(t *testing.T, s *store.Store, id string) {
	t.Helper()
	if err := s.CreateSandbox(context.Background(), &model.Sandbox{
		ID: id, ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: id,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
}

// One inventory is held per sandbox: a new one replaces it, and an unchanged
// one only moves its observation time, forward.
func TestSandboxInventoryIsReplacedAndConfirmedByDigest(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	inventoryTestSandbox(t, s, "sandbox-1")
	at := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)

	if _, err := s.GetSandboxInventory(ctx, "project-1", "sandbox-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get before any was posted: %v, want ErrNotFound", err)
	}
	if held, err := s.ConfirmSandboxInventory(ctx, "project-1", "sandbox-1", "sha256:one", at); err != nil || held {
		t.Fatalf("confirm before any was posted: held = %v, err = %v; want not held", held, err)
	}

	if err := s.PutSandboxInventory(ctx, "project-1", "pool-1", "sandbox-1", "sha256:one", []byte(`{"execs":[]}`), at); err != nil {
		t.Fatalf("put: %v", err)
	}
	if held, err := s.ConfirmSandboxInventory(ctx, "project-1", "sandbox-1", "sha256:one", at.Add(15*time.Second)); err != nil || !held {
		t.Fatalf("confirm the held digest: held = %v, err = %v", held, err)
	}
	// A late report must not walk the observation back.
	if held, err := s.ConfirmSandboxInventory(ctx, "project-1", "sandbox-1", "sha256:one", at.Add(5*time.Second)); err != nil || !held {
		t.Fatalf("late confirm: held = %v, err = %v", held, err)
	}
	got, err := s.GetSandboxInventory(ctx, "project-1", "sandbox-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Digest != "sha256:one" || string(got.Document) != `{"execs":[]}` || !got.ObservedAt.Equal(at.Add(15*time.Second)) {
		t.Fatalf("inventory = %+v, want the first document observed at +15s", got)
	}

	if held, err := s.ConfirmSandboxInventory(ctx, "project-1", "sandbox-1", "sha256:two", at.Add(30*time.Second)); err != nil || held {
		t.Fatalf("confirm a digest that is not held: held = %v, err = %v", held, err)
	}
	if err := s.PutSandboxInventory(ctx, "project-1", "pool-1", "sandbox-1", "sha256:two", []byte(`{"execs":[{"id":"e"}]}`), at.Add(30*time.Second)); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err = s.GetSandboxInventory(ctx, "project-1", "sandbox-1")
	if err != nil || got.Digest != "sha256:two" || string(got.Document) != `{"execs":[{"id":"e"}]}` {
		t.Fatalf("after replace: %+v, %v", got, err)
	}
	// Another project's caller reads nothing.
	if _, err := s.GetSandboxInventory(ctx, "project-2", "sandbox-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get from another project: %v, want ErrNotFound", err)
	}
}

// The inventory holds terminal content, so it is sealed at rest like a secret
// (ADR 26-10-11-657 §3), bound to its sandbox.
func TestSandboxInventoryIsSealedAtRest(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, true)
	createTestPool(t, s, "project-1", "pool-1")
	inventoryTestSandbox(t, s, "sandbox-1")
	inventoryTestSandbox(t, s, "sandbox-2")

	document := []byte(`{"screens":{"exec-1":{"lines":["export TOKEN=hunter2"]}}}`)
	if err := s.PutSandboxInventory(ctx, "project-1", "pool-1", "sandbox-1", "sha256:one", document, time.Now().UTC()); err != nil {
		t.Fatalf("put: %v", err)
	}
	var row model.SandboxInventory
	if err := db.Read.WithContext(ctx).Where("sandbox_id = ?", "sandbox-1").First(&row).Error; err != nil {
		t.Fatalf("read row: %v", err)
	}
	if !encryption.Sealed(row.Document) || bytes.Contains(row.Document, []byte("hunter2")) {
		t.Fatalf("stored document is not sealed: %q", row.Document)
	}
	got, err := s.GetSandboxInventory(ctx, "project-1", "sandbox-1")
	if err != nil || !bytes.Equal(got.Document, document) {
		t.Fatalf("get = %q, %v; want the document opened", got.Document, err)
	}
	// One sandbox's sealed inventory does not open as another's.
	if err := db.Write.WithContext(ctx).Create(&model.SandboxInventory{
		SandboxID: "sandbox-2", ProjectID: "project-1", Digest: "sha256:one", Document: row.Document, ObservedAt: time.Now().UTC(),
	}).Error; err != nil {
		t.Fatalf("copy row: %v", err)
	}
	if _, err := s.GetSandboxInventory(ctx, "project-1", "sandbox-2"); err == nil {
		t.Fatal("an inventory sealed for one sandbox opened as another's")
	}
}

// Deleting a sandbox deletes what it posted: terminal content must not
// outlive the sandbox it came from.
func TestDeletingASandboxDeletesItsInventory(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, false)
	createTestPool(t, s, "project-1", "pool-1")
	inventoryTestSandbox(t, s, "sandbox-1")
	inventoryTestSandbox(t, s, "sandbox-2")
	for _, id := range []string{"sandbox-1", "sandbox-2"} {
		if err := s.PutSandboxInventory(ctx, "project-1", "pool-1", id, "sha256:one", []byte(`{}`), time.Now().UTC()); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	if err := s.DeleteSandbox(ctx, "project-1", "sandbox-1"); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	var remaining []string
	if err := db.Read.WithContext(ctx).Model(&model.SandboxInventory{}).Pluck("sandbox_id", &remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0] != "sandbox-2" {
		t.Fatalf("inventories left = %v, want only sandbox-2's", remaining)
	}
}

// An inventory is written only for a sandbox its pool still hosts, decided
// where it is written: a report that races a delete leaves nothing behind.
func TestSandboxInventoryIsNotWrittenForASandboxThePoolDoesNotHost(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, false)
	createTestPool(t, s, "project-1", "pool-1")
	inventoryTestSandbox(t, s, "sandbox-1")
	at := time.Now().UTC()

	for name, put := range map[string]func() error{
		"a sandbox that is gone": func() error {
			return s.PutSandboxInventory(ctx, "project-1", "pool-1", "sandbox-gone", "sha256:one", []byte(`{}`), at)
		},
		"a sandbox on another pool": func() error {
			return s.PutSandboxInventory(ctx, "project-1", "pool-2", "sandbox-1", "sha256:one", []byte(`{}`), at)
		},
	} {
		if err := put(); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
	var rows int64
	if err := db.Read.WithContext(ctx).Model(&model.SandboxInventory{}).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("%d inventory rows written for sandboxes the pool does not host", rows)
	}
}
