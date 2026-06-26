// Package workeragentauth manages control-plane signed requests to worker agents.
package workeragentauth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"aidanwoods.dev/go-paseto"

	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/secrets"
	"github.com/obot-platform/discobox/server/internal/store"
)

const (
	StateKey = "worker_agent_request_issuer"
	purpose  = "worker_agent_request_issuer.private_key"

	Audience             = "worker-agent"
	SandboxAgentAudience = "sandbox-agent"
	ScopeSandboxRead     = "sandbox:read"
	ScopeSandboxWrite    = "sandbox:write"
	ScopeSandboxHTTP     = "sandbox:http"
	ScopeTerminalRead    = "terminal:read"
	ScopeTerminalWrite   = "terminal:write"
	ScopeExecRead        = "exec:read"
	ScopeExecWrite       = "exec:write"

	TokenTTL  = 15 * time.Minute
	ClockSkew = 5 * time.Minute
)

type Store interface {
	GetServerState(ctx context.Context, key string) (*model.ServerState, error)
	CreateServerState(ctx context.Context, state *model.ServerState) error
}

type Manager struct {
	store  Store
	sealer secrets.Sealer
}

type TokenClaims struct {
	ProjectID string
	WorkerID  string
	SandboxID string
	Scopes    []string
}

type issuerState struct {
	PublicKey           string `json:"publicKey"`
	EncryptedPrivateKey []byte `json:"encryptedPrivateKey"`
	KeyType             string `json:"keyType"`
}

func NewManager(store Store, sealer secrets.Sealer) *Manager {
	return &Manager{store: store, sealer: sealer}
}

func (m *Manager) EnsureTrustKey(ctx context.Context) (string, error) {
	issuer, err := m.loadIssuer(ctx)
	if err == nil {
		return issuer.PublicKey, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	publicKey, privateKey, err := GenerateKeyPair()
	if err != nil {
		return "", err
	}
	encryptedPrivateKey, err := secrets.SealIfUnsealed(ctx, m.sealer, purpose, StateKey, []byte(privateKey))
	if err != nil {
		return "", err
	}
	value, err := json.Marshal(issuerState{
		PublicKey:           publicKey,
		EncryptedPrivateKey: encryptedPrivateKey,
		KeyType:             "ed25519",
	})
	if err != nil {
		return "", err
	}
	if err := m.store.CreateServerState(ctx, &model.ServerState{Key: StateKey, Value: value}); err != nil {
		issuer, reloadErr := m.loadIssuer(ctx)
		if reloadErr == nil {
			return issuer.PublicKey, nil
		}
		return "", err
	}
	return publicKey, nil
}

func (m *Manager) CreateToken(ctx context.Context, claims TokenClaims) (string, error) {
	return m.createToken(ctx, Audience, claims)
}

func (m *Manager) CreateSandboxAgentToken(ctx context.Context, claims TokenClaims) (string, error) {
	if claims.SandboxID == "" {
		return "", fmt.Errorf("sandboxID is required")
	}
	return m.createToken(ctx, SandboxAgentAudience, claims)
}

func (m *Manager) createToken(ctx context.Context, audience string, claims TokenClaims) (string, error) {
	if claims.ProjectID == "" || claims.WorkerID == "" {
		return "", fmt.Errorf("projectID and workerID are required")
	}
	issuer, err := m.loadIssuer(ctx)
	if errors.Is(err, store.ErrNotFound) {
		if _, ensureErr := m.EnsureTrustKey(ctx); ensureErr != nil {
			return "", ensureErr
		}
		issuer, err = m.loadIssuer(ctx)
	}
	if err != nil {
		return "", err
	}
	privateKeyText, err := secrets.Open(ctx, m.sealer, purpose, StateKey, issuer.EncryptedPrivateKey)
	if err != nil {
		return "", err
	}
	privateKey, err := DecodePrivateKey(string(privateKeyText))
	if err != nil {
		return "", err
	}
	return CreateTokenForAudience(privateKey, audience, claims, TokenTTL, ClockSkew)
}

func (m *Manager) loadIssuer(ctx context.Context) (issuerState, error) {
	if m == nil || m.store == nil {
		return issuerState{}, fmt.Errorf("worker-agent token issuer is not configured")
	}
	state, err := m.store.GetServerState(ctx, StateKey)
	if err != nil {
		return issuerState{}, err
	}
	var issuer issuerState
	if err := json.Unmarshal(state.Value, &issuer); err != nil {
		return issuerState{}, fmt.Errorf("decode worker-agent token issuer: %w", err)
	}
	if issuer.PublicKey == "" || len(issuer.EncryptedPrivateKey) == 0 {
		return issuerState{}, fmt.Errorf("worker-agent token issuer is incomplete")
	}
	return issuer, nil
}

func GenerateKeyPair() (publicKey, privateKey string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return encode(pub), encode(priv), nil
}

func DecodePrivateKey(value string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid worker-agent private key length")
	}
	return ed25519.PrivateKey(raw), nil
}

func CreateToken(privateKey ed25519.PrivateKey, claims TokenClaims, ttl, clockSkew time.Duration) (string, error) {
	return CreateTokenForAudience(privateKey, Audience, claims, ttl, clockSkew)
}

func CreateTokenForAudience(privateKey ed25519.PrivateKey, audience string, claims TokenClaims, ttl, clockSkew time.Duration) (string, error) {
	if audience == "" {
		return "", fmt.Errorf("audience is required")
	}
	pasetoKey, err := paseto.NewV4AsymmetricSecretKeyFromEd25519(privateKey)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	now := time.Now()
	token := paseto.NewToken()
	token.SetAudience(audience)
	token.SetIssuedAt(now)
	token.SetNotBefore(now.Add(-clockSkew))
	token.SetExpiration(now.Add(ttl))
	token.SetJti(encode(nonce))
	token.SetString("project_id", claims.ProjectID)
	token.SetString("worker_id", claims.WorkerID)
	if claims.SandboxID != "" {
		token.SetString("sandbox_id", claims.SandboxID)
	}
	if err := token.Set("scopes", claims.Scopes); err != nil {
		return "", err
	}
	return token.V4Sign(pasetoKey, nil), nil
}

func encode(value []byte) string {
	return base64.StdEncoding.EncodeToString(value)
}
