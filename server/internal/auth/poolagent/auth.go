// Package poolagentauth manages control-plane signed requests to pool agents.
package poolagentauth

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

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/secrets"
	"github.com/discobox-ai/discobox/server/internal/store"
)

const (
	StateKey = "worker_agent_request_issuer"
	purpose  = "worker_agent_request_issuer.private_key"

	Audience             = "pool-agent"
	SandboxAgentAudience = "sandbox-agent"
	ScopeSandboxRead     = "sandbox:read"
	ScopeSandboxWrite    = "sandbox:write"
	ScopeSandboxHTTP     = "sandbox:http"
	ScopeTerminalRead    = "terminal:read"
	ScopeTerminalWrite   = "terminal:write"
	ScopeExecRead        = "exec:read"
	ScopeExecWrite       = "exec:write"
	// ScopeFilesRead and ScopeFilesWrite gate the files API (ADR 26-10-22-499
	// §3): stat, list and read, and every write. They are granted with the
	// exec scopes and apart from them.
	ScopeFilesRead  = "files:read"
	ScopeFilesWrite = "files:write"
	// ScopeTCPConnect gates the direct-tcpip tunnel endpoint (ADR 0024 §3):
	// dialing an arbitrary host:port from inside the sandbox's own network
	// namespace, distinct from ScopeSandboxHTTP's container-IP-only reach.
	ScopeTCPConnect = "tcp:connect"
	// ScopeUDPConnect gates the UDP tunnel endpoint, the same reach as
	// ScopeTCPConnect for datagrams (ADR 0109 §4).
	ScopeUDPConnect = "udp:connect"
	ScopePoolSync   = "pool:sync"
	// ScopeAuditRead authorizes reading a pool proxy's audit trail (ADR 0130
	// §4). A token that also names a sandbox reads only that sandbox's rows.
	ScopeAuditRead = "audit:read"
	// ScopePoolCacheClear authorizes stopping every sandbox on a pool and
	// emptying its cache.
	ScopePoolCacheClear = "pool:cache-clear"
	// ScopePoolControlPlaneChannel authorizes opening a pool's control-plane
	// channel (ADR 26-10-30-875 §1).
	ScopePoolControlPlaneChannel = "pool:control-plane-channel"
	// ScopeStatusRead authorizes only the sandbox-agent status endpoint, and
	// ScopeInventoryRead only its inventory endpoint (ADR 26-10-11-657 §1).
	// They are the two scopes MintSandboxAgentStatusTokens mints, hardcoded
	// there rather than accepted from the caller, so a pool agent can never
	// obtain a sandbox-agent token broader than those two reads through that
	// path.
	ScopeStatusRead    = "status:read"
	ScopeInventoryRead = "inventory:read"

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
	PoolID    string
	SandboxID string
	Scopes    []string
	// Use marks a token for a route into a sandbox with what the request is
	// for (ADR 26-10-12-711 §2). The pool agent starts a stopped sandbox, or
	// refuses to, by it. Empty on every other token.
	Use string
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
	if claims.ProjectID == "" || claims.PoolID == "" {
		return "", fmt.Errorf("projectID and workerID are required")
	}
	privateKey, err := m.IssuerKey(ctx)
	if err != nil {
		return "", err
	}
	return CreateTokenForAudience(privateKey, audience, claims, TokenTTL, ClockSkew)
}

// IssuerKey is the private half of the key every server request to a pool
// agent is signed with, made on first use. The agent already trusts its
// public half (EnsureTrustKey), which is why the server presents it on a
// control-plane channel's TLS too (ADR 26-10-30-875 §1).
func (m *Manager) IssuerKey(ctx context.Context) (ed25519.PrivateKey, error) {
	issuer, err := m.loadIssuer(ctx)
	if errors.Is(err, store.ErrNotFound) {
		if _, ensureErr := m.EnsureTrustKey(ctx); ensureErr != nil {
			return nil, ensureErr
		}
		issuer, err = m.loadIssuer(ctx)
	}
	if err != nil {
		return nil, err
	}
	privateKeyText, err := secrets.Open(ctx, m.sealer, purpose, StateKey, issuer.EncryptedPrivateKey)
	if err != nil {
		return nil, err
	}
	return DecodePrivateKey(string(privateKeyText))
}

func (m *Manager) loadIssuer(ctx context.Context) (issuerState, error) {
	if m == nil || m.store == nil {
		return issuerState{}, fmt.Errorf("pool-agent token issuer is not configured")
	}
	state, err := m.store.GetServerState(ctx, StateKey)
	if err != nil {
		return issuerState{}, err
	}
	var issuer issuerState
	if err := json.Unmarshal(state.Value, &issuer); err != nil {
		return issuerState{}, fmt.Errorf("decode pool-agent token issuer: %w", err)
	}
	if issuer.PublicKey == "" || len(issuer.EncryptedPrivateKey) == 0 {
		return issuerState{}, fmt.Errorf("pool-agent token issuer is incomplete")
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
		return nil, fmt.Errorf("invalid pool-agent private key length")
	}
	return ed25519.PrivateKey(raw), nil
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
	// Both backdated by the same allowance: a verifier checks IssuedAt as well
	// as NotBefore, so skew tolerance on one and not the other tolerates
	// nothing. See poolauth.CreateTokenWithTTL.
	token.SetIssuedAt(now.Add(-clockSkew))
	token.SetNotBefore(now.Add(-clockSkew))
	token.SetExpiration(now.Add(ttl))
	token.SetJti(encode(nonce))
	token.SetString("project_id", claims.ProjectID)
	token.SetString("pool_id", claims.PoolID)
	if claims.SandboxID != "" {
		token.SetString("sandbox_id", claims.SandboxID)
	}
	if err := token.Set("scopes", claims.Scopes); err != nil {
		return "", err
	}
	if claims.Use != "" {
		token.SetString("use", claims.Use)
	}
	return token.V4Sign(pasetoKey, nil), nil
}

func encode(value []byte) string {
	return base64.StdEncoding.EncodeToString(value)
}
