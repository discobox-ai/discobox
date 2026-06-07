package sandboxauth_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"

	"github.com/obot-platform/disco2/internal/model"
	"github.com/obot-platform/disco2/internal/sandboxauth"
	"github.com/obot-platform/disco2/internal/secrets"
	"github.com/obot-platform/disco2/internal/store"
)

func TestManagerEnsuresTrustKeyOnceAndCreatesVerifiableToken(t *testing.T) {
	ctx := context.Background()
	userStore := newMemoryUserStore()
	key, err := secrets.GenerateBase64Key()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealerFromBase64Key(key)
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}
	manager := sandboxauth.NewManager(userStore, sealer)

	trustKey, err := manager.EnsureTrustKey(ctx, "project-1", "user-1")
	if err != nil {
		t.Fatalf("ensure trust key: %v", err)
	}
	if trustKey == "" {
		t.Fatal("expected trust key")
	}
	again, err := manager.EnsureTrustKey(ctx, "project-1", "user-1")
	if err != nil {
		t.Fatalf("ensure existing trust key: %v", err)
	}
	if again != trustKey {
		t.Fatalf("trust key changed: %q != %q", again, trustKey)
	}
	if userStore.updates != 1 {
		t.Fatalf("updates = %d, want 1", userStore.updates)
	}

	otherProjectKey, err := manager.EnsureTrustKey(ctx, "project-2", "user-1")
	if err != nil {
		t.Fatalf("ensure other project trust key: %v", err)
	}
	if otherProjectKey == trustKey {
		t.Fatalf("trust key reused across projects")
	}

	token, err := manager.CreateToken(ctx, "project-1", "user-1")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if !strings.HasPrefix(token, "v4.public.") {
		t.Fatalf("token prefix = %q, want v4.public", token)
	}

	publicKeyBytes, err := base64.StdEncoding.DecodeString(trustKey)
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	if len(publicKeyBytes) != ed25519.PublicKeySize {
		t.Fatalf("public key length = %d, want %d", len(publicKeyBytes), ed25519.PublicKeySize)
	}
	publicKey, err := paseto.NewV4AsymmetricPublicKeyFromEd25519(ed25519.PublicKey(publicKeyBytes))
	if err != nil {
		t.Fatalf("paseto public key: %v", err)
	}
	parsed, err := paseto.NewParser().ParseV4Public(publicKey, token, nil)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	exp, err := parsed.GetExpiration()
	if err != nil {
		t.Fatalf("get expiration: %v", err)
	}
	if time.Until(exp) <= 0 {
		t.Fatalf("expiration = %s, want future", exp)
	}
}

type memoryUserStore struct {
	mu      sync.Mutex
	keys    map[string]*model.ProjectUserKey
	updates int
}

func newMemoryUserStore() *memoryUserStore {
	return &memoryUserStore{keys: map[string]*model.ProjectUserKey{}}
}

func (s *memoryUserStore) GetProjectUserKey(_ context.Context, projectID, userID string) (*model.ProjectUserKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.keys[projectID+"/"+userID]
	if key == nil {
		return nil, store.ErrNotFound
	}
	copied := *key
	copied.EncryptedSandboxPrivateKey = append([]byte(nil), key.EncryptedSandboxPrivateKey...)
	return &copied, nil
}

func (s *memoryUserStore) CreateProjectUserKeyIfMissing(_ context.Context, key *model.ProjectUserKey) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := key.ProjectID + "/" + key.UserID
	if s.keys[id] != nil {
		return false, nil
	}
	copied := *key
	copied.EncryptedSandboxPrivateKey = append([]byte(nil), key.EncryptedSandboxPrivateKey...)
	s.keys[id] = &copied
	s.updates++
	return true, nil
}
