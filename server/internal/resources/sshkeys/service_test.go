package sshkeys_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	resourcesshkeys "github.com/obot-platform/discobox/server/internal/resources/sshkeys"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

const testPublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBboEyGDIiA0m5NEPRKXBTvzqSFCosRkVUUxfoM6RB6i user@laptop"

func TestCreateSSHKeyNormalizesAndFingerprints(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	key, err := svc.CreateSSHKey(ctx, "project-1", services.CreateSSHKeyBody{
		PublicKey: testPublicKey,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if key.Fingerprint == "" {
		t.Fatalf("expected a computed fingerprint")
	}
	if key.Comment != "user@laptop" {
		t.Fatalf("comment = %q, want %q", key.Comment, "user@laptop")
	}
	// The stored public key is normalized to just "type base64", no comment.
	if key.PublicKey == testPublicKey {
		t.Fatalf("expected normalized public key, got the raw input line")
	}

	list, err := svc.ListSSHKeys(ctx, "project-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != key.ID {
		t.Fatalf("list = %+v", list)
	}
}

func TestCreateSSHKeyRejectsInvalidKey(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	if _, err := svc.CreateSSHKey(ctx, "project-1", services.CreateSSHKeyBody{PublicKey: "not a key"}); err == nil {
		t.Fatalf("expected an error for an invalid public key")
	}
}

func TestCreateSSHKeyRejectsUnknownProject(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	_, err := svc.CreateSSHKey(ctx, "project-missing", services.CreateSSHKeyBody{PublicKey: testPublicKey})
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusNotFound {
		t.Fatalf("err = %v, want 404", err)
	}
}

func TestDeleteSSHKey(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	key, err := svc.CreateSSHKey(ctx, "project-1", services.CreateSSHKeyBody{PublicKey: testPublicKey})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.DeleteSSHKey(ctx, "project-1", key.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, err := svc.ListSSHKeys(ctx, "project-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("list after delete = %+v, want empty", list)
	}
}

func newTestService(t *testing.T) *resourcesshkeys.Service {
	t.Helper()

	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{
		ID:          "project-1",
		OwnerUserID: "user-1",
		Name:        "Project",
		Slug:        "project",
	}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}

	return resourcesshkeys.NewService(store.New(db.Write, db.Read))
}
