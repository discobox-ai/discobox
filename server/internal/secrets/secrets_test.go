package secrets_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/secrets"
)

func TestAESGCMSealerRoundTripAndAssociatedData(t *testing.T) {
	key, err := secrets.GenerateBase64Key()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealerFromBase64Key(key)
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}

	plaintext := []byte("secret state")
	ciphertext, err := sealer.Seal(context.Background(), "purpose", "resource-1", plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("ciphertext equals plaintext")
	}
	if !bytes.HasPrefix(ciphertext, []byte("discobox:v1:")) {
		t.Fatalf("ciphertext prefix = %q, want discobox:v1", string(ciphertext[:min(len(ciphertext), len("discobox:v1:"))]))
	}

	opened, err := sealer.Open(context.Background(), "purpose", "resource-1", ciphertext)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(opened) != string(plaintext) {
		t.Fatalf("opened = %q, want %q", string(opened), string(plaintext))
	}
	if _, err := sealer.Open(context.Background(), "purpose", "resource-2", ciphertext); err == nil {
		t.Fatal("open with wrong resource succeeded")
	}
	if _, err := sealer.Open(context.Background(), "other-purpose", "resource-1", ciphertext); err == nil {
		t.Fatal("open with wrong purpose succeeded")
	}
}

func TestNewAESGCMSealerFromBase64KeyRejectsInvalidInput(t *testing.T) {
	if _, err := secrets.NewAESGCMSealerFromBase64Key("not-base64"); err == nil {
		t.Fatal("expected invalid key error")
	}
	if _, err := secrets.NewAESGCMSealerFromBase64Key("c2hvcnQ="); err == nil {
		t.Fatal("expected short key error")
	}
}
