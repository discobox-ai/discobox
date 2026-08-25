// Package sandboxauth manages sandbox trust-key authentication.
package sandboxauth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"aidanwoods.dev/go-paseto"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/secrets"
	"github.com/discobox-ai/discobox/server/internal/store"
)

const TokenTTL = 12 * time.Hour

// clockSkew is how far apart the minting and verifying clocks may be.
//
// It was one second, which is not an allowance: these tokens are minted by the
// control plane and verified inside a pool VM, whose clock is stepped off the
// host RTC every 30s and drifts either side of it in between. A minute is the
// same allowance the proxy's control tokens carry, and the expiry still bounds
// the far end.
const clockSkew = time.Minute

type TokenClaims struct {
	ProjectID string
	SandboxID string
	UserID    string
}

type UserStore interface {
	GetProjectUserKey(ctx context.Context, projectID, userID string) (*model.ProjectUserKey, error)
	CreateProjectUserKeyIfMissing(ctx context.Context, key *model.ProjectUserKey) (bool, error)
}

type Manager struct {
	store  UserStore
	sealer secrets.Sealer
}

func NewManager(store UserStore, sealer secrets.Sealer) *Manager {
	return &Manager{store: store, sealer: sealer}
}

func (m *Manager) EnsureTrustKey(ctx context.Context, projectID, userID string) (string, error) {
	if m == nil || m.store == nil || m.sealer == nil || projectID == "" || userID == "" {
		return "", nil
	}
	key, err := m.store.GetProjectUserKey(ctx, projectID, userID)
	if err == nil {
		if key.PublicKey != "" && len(key.EncryptedPrivateKey) > 0 {
			return key.PublicKey, nil
		}
		return "", fmt.Errorf("project user sandbox trust key is incomplete")
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	publicKey, privateKey, err := GenerateKeyPair()
	if err != nil {
		return "", err
	}
	resourceID := keyResourceID(projectID, userID)
	encryptedPrivateKey, err := m.sealer.Seal(ctx, "sandbox_access_issuer_keys.private_key", resourceID, []byte(privateKey))
	if err != nil {
		return "", err
	}
	created, err := m.store.CreateProjectUserKeyIfMissing(ctx, &model.ProjectUserKey{
		ProjectID:           projectID,
		UserID:              userID,
		PublicKey:           publicKey,
		EncryptedPrivateKey: encryptedPrivateKey,
		KeyType:             "ed25519",
	})
	if err != nil {
		return "", err
	}
	if created {
		return publicKey, nil
	}
	reloaded, err := m.store.GetProjectUserKey(ctx, projectID, userID)
	if err != nil {
		return "", err
	}
	if reloaded.PublicKey == "" || len(reloaded.EncryptedPrivateKey) == 0 {
		return "", fmt.Errorf("sandbox trust key was not stored")
	}
	return reloaded.PublicKey, nil
}

func (m *Manager) CreateToken(ctx context.Context, claims TokenClaims) (string, error) {
	if m == nil || m.store == nil || m.sealer == nil || claims.ProjectID == "" || claims.UserID == "" {
		return "", nil
	}
	key, err := m.store.GetProjectUserKey(ctx, claims.ProjectID, claims.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	if len(key.EncryptedPrivateKey) == 0 {
		return "", nil
	}
	privateKeyText, err := m.sealer.Open(ctx, "sandbox_access_issuer_keys.private_key", keyResourceID(claims.ProjectID, claims.UserID), key.EncryptedPrivateKey)
	if err != nil {
		return "", err
	}
	privateKey, err := DecodePrivateKey(string(privateKeyText))
	if err != nil {
		return "", err
	}
	return CreateToken(privateKey, claims, TokenTTL)
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
		return nil, fmt.Errorf("invalid sandbox private key length")
	}
	return ed25519.PrivateKey(raw), nil
}

func CreateToken(privateKey ed25519.PrivateKey, claims TokenClaims, ttl time.Duration) (string, error) {
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
	// Both backdated by the same allowance: a verifier checks IssuedAt as well
	// as NotBefore, so skew tolerance on one and not the other tolerates
	// nothing. See poolauth.CreateTokenWithTTL.
	token.SetIssuedAt(now.Add(-clockSkew))
	token.SetNotBefore(now.Add(-clockSkew))
	token.SetExpiration(now.Add(ttl))
	token.SetJti(encode(nonce))
	token.SetString("project_id", claims.ProjectID)
	token.SetString("user_id", claims.UserID)
	if claims.SandboxID != "" {
		token.SetString("sandbox_id", claims.SandboxID)
	}
	return token.V4Sign(pasetoKey, nil), nil
}

func encode(value []byte) string {
	return base64.StdEncoding.EncodeToString(value)
}

func keyResourceID(projectID, userID string) string {
	return projectID + "/" + userID
}
