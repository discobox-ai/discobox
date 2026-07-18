package poolagentauth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"

	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/store"
)

func TestManagerCreatesTrustKeyAndScopedToken(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(newMemoryStore(), nil)

	publicKeyText, err := manager.EnsureTrustKey(ctx)
	if err != nil {
		t.Fatalf("ensure trust key: %v", err)
	}
	publicKeyBytes, err := base64.StdEncoding.DecodeString(publicKeyText)
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	if len(publicKeyBytes) != ed25519.PublicKeySize {
		t.Fatalf("public key length = %d, want %d", len(publicKeyBytes), ed25519.PublicKeySize)
	}

	signed, err := manager.CreateToken(ctx, TokenClaims{
		ProjectID: "project-1",
		PoolID:    "worker-1",
		SandboxID: "sandbox-1",
		Scopes:    []string{ScopeSandboxRead},
	})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	publicKey, err := paseto.NewV4AsymmetricPublicKeyFromEd25519(ed25519.PublicKey(publicKeyBytes))
	if err != nil {
		t.Fatalf("paseto public key: %v", err)
	}
	parser := paseto.NewParserForValidNow()
	parser.AddRule(paseto.ForAudience(Audience))
	parsed, err := parser.ParseV4Public(publicKey, signed, nil)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if projectID, _ := parsed.GetString("project_id"); projectID != "project-1" {
		t.Fatalf("project_id = %q", projectID)
	}
	if workerID, _ := parsed.GetString("pool_id"); workerID != "worker-1" {
		t.Fatalf("worker_id = %q", workerID)
	}
	if sandboxID, _ := parsed.GetString("sandbox_id"); sandboxID != "sandbox-1" {
		t.Fatalf("sandbox_id = %q", sandboxID)
	}
	var scopes []string
	if err := parsed.Get("scopes", &scopes); err != nil {
		t.Fatalf("scopes claim: %v", err)
	}
	if len(scopes) != 1 || scopes[0] != ScopeSandboxRead {
		t.Fatalf("scopes = %#v", scopes)
	}
	notBefore, err := parsed.GetTime("nbf")
	if err != nil {
		t.Fatalf("nbf claim: %v", err)
	}
	if time.Until(notBefore) > -4*time.Minute {
		t.Fatalf("not-before was not backdated enough: %v", notBefore)
	}
}

type memoryStore struct {
	state map[string]*model.ServerState
}

func newMemoryStore() *memoryStore {
	return &memoryStore{state: map[string]*model.ServerState{}}
}

func (s *memoryStore) GetServerState(_ context.Context, key string) (*model.ServerState, error) {
	state := s.state[key]
	if state == nil {
		return nil, store.ErrNotFound
	}
	copied := *state
	copied.Value = append(json.RawMessage(nil), state.Value...)
	return &copied, nil
}

func (s *memoryStore) CreateServerState(_ context.Context, state *model.ServerState) error {
	copied := *state
	copied.Value = append(json.RawMessage(nil), state.Value...)
	s.state[state.Key] = &copied
	return nil
}
