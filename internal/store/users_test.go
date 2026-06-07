package store_test

import (
	"context"
	"testing"

	"github.com/obot-platform/disco2/internal/model"
)

func TestCreateProjectUserKeyIfMissing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	created, err := s.CreateProjectUserKeyIfMissing(ctx, &model.ProjectUserKey{
		ProjectID:                  "project-1",
		UserID:                     "user-1",
		SandboxPublicKey:           "public-1",
		EncryptedSandboxPrivateKey: []byte("private-1"),
	})
	if err != nil {
		t.Fatalf("create missing key: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}

	key, err := s.GetProjectUserKey(ctx, "project-1", "user-1")
	if err != nil {
		t.Fatalf("get project user key: %v", err)
	}
	if key.SandboxPublicKey != "public-1" || string(key.EncryptedSandboxPrivateKey) != "private-1" {
		t.Fatalf("keys = %q/%q", key.SandboxPublicKey, string(key.EncryptedSandboxPrivateKey))
	}

	created, err = s.CreateProjectUserKeyIfMissing(ctx, &model.ProjectUserKey{
		ProjectID:                  "project-1",
		UserID:                     "user-1",
		SandboxPublicKey:           "public-2",
		EncryptedSandboxPrivateKey: []byte("private-2"),
	})
	if err != nil {
		t.Fatalf("create existing key: %v", err)
	}
	if created {
		t.Fatal("created = true, want false")
	}

	key, err = s.GetProjectUserKey(ctx, "project-1", "user-1")
	if err != nil {
		t.Fatalf("get project user key after duplicate create: %v", err)
	}
	if key.SandboxPublicKey != "public-1" || string(key.EncryptedSandboxPrivateKey) != "private-1" {
		t.Fatalf("keys changed to %q/%q", key.SandboxPublicKey, string(key.EncryptedSandboxPrivateKey))
	}
}
