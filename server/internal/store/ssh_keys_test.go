package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/store"
)

func TestSSHKeyCreateGetListDelete(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)

	if err := db.Write.WithContext(ctx).Create(&model.Project{
		ID: "project-2", OwnerUserID: "user-1", Name: "Project 2",
	}).Error; err != nil {
		t.Fatalf("create project-2: %v", err)
	}

	key := &model.SSHKey{
		ProjectID:   "project-1",
		Name:        "laptop",
		PublicKey:   "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBboEyGDIiA0m5NEPRKXBTvzqSFCosRkVUUxfoM6RB6i",
		Fingerprint: "SHA256:abc123",
		Comment:     "user@laptop",
	}
	if err := s.CreateSSHKey(ctx, key); err != nil {
		t.Fatalf("create: %v", err)
	}
	if key.ID == "" {
		t.Fatalf("expected generated ID")
	}

	// A second project's key with the same fingerprint is fine: the unique
	// index is scoped per project.
	otherProject := &model.SSHKey{
		ProjectID:   "project-2",
		PublicKey:   key.PublicKey,
		Fingerprint: key.Fingerprint,
	}
	if err := s.CreateSSHKey(ctx, otherProject); err != nil {
		t.Fatalf("create in other project: %v", err)
	}

	got, err := s.GetSSHKey(ctx, "project-1", key.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "laptop" || got.Fingerprint != "SHA256:abc123" {
		t.Fatalf("got %+v", got)
	}

	// Prefix lookup, matching the id package's short-ID resolution.
	if _, err := s.GetSSHKey(ctx, "project-1", key.ID[:len(key.ID)-4]); err != nil {
		t.Fatalf("get by prefix: %v", err)
	}

	// Wrong project scope must not find it.
	if _, err := s.GetSSHKey(ctx, "project-2", key.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get from wrong project: err = %v, want ErrNotFound", err)
	}

	list, err := s.ListSSHKeys(ctx, "project-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != key.ID {
		t.Fatalf("list = %+v", list)
	}

	if err := s.DeleteSSHKey(ctx, "project-1", key.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetSSHKey(ctx, "project-1", key.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: err = %v, want ErrNotFound", err)
	}
	// The other project's row survives.
	if _, err := s.GetSSHKey(ctx, "project-2", otherProject.ID); err != nil {
		t.Fatalf("other project's key should be unaffected: %v", err)
	}
}

func TestSSHKeyDuplicateFingerprintWithinProjectConflicts(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)

	first := &model.SSHKey{ProjectID: "project-1", PublicKey: "ssh-ed25519 AAAA", Fingerprint: "SHA256:dup"}
	if err := s.CreateSSHKey(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	second := &model.SSHKey{ProjectID: "project-1", PublicKey: "ssh-ed25519 AAAA", Fingerprint: "SHA256:dup"}
	if err := s.CreateSSHKey(ctx, second); err == nil {
		t.Fatalf("expected duplicate fingerprint within the same project to fail")
	}
}

func TestSSHKeyDeleteNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)

	if err := s.DeleteSSHKey(ctx, "project-1", "sshkey_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
