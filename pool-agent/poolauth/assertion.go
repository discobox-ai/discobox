// Package poolauth creates and verifies pool-to-control-plane assertions.
package poolauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"aidanwoods.dev/go-paseto"
)

const (
	Audience  = "discobox-control-plane"
	KeyType   = "ed25519"
	TokenTTL  = 5 * time.Minute
	ClockSkew = 5 * time.Minute

	// ScopeSecretResolve authorizes sentinel secret resolution. It is carried by
	// the long-lived token the pool mints for the isolated proxy unit.
	ScopeSecretResolve = "secret:resolve"

	// ScopeCredentialBroker authorizes the agent credentials broker routes: list
	// a sandbox's granted credentials, record its requests, poll them
	// (ADR 0031). It rides the same token as ScopeSecretResolve — the proxy unit
	// holds both because it is the process that owns the sentinel registry — but
	// stays a distinct scope, because resolving is what the proxy does with
	// traffic it already has and brokering is what it does on a sandbox's behalf.
	//nolint:gosec // A scope name, not a credential.
	ScopeCredentialBroker = "credential:broker"
)

type Claims struct {
	ProjectID string
	PoolID    string
	ID        string
	Scopes    []string
}

// HasScope reports whether the claims include scope.
func (c Claims) HasScope(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

func CreateToken(privateKey ed25519.PrivateKey, claims Claims) (string, error) {
	return CreateTokenWithTTL(privateKey, claims, TokenTTL)
}

// CreateTokenWithTTL signs a pool assertion with an explicit lifetime. It is
// used for the longer-lived scoped resolve token distributed to the proxy unit.
func CreateTokenWithTTL(privateKey ed25519.PrivateKey, claims Claims, ttl time.Duration) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("pool private key length = %d, want %d", len(privateKey), ed25519.PrivateKeySize)
	}
	if claims.ProjectID == "" || claims.PoolID == "" {
		return "", errors.New("project_id and pool_id claims are required")
	}
	if ttl <= 0 {
		ttl = TokenTTL
	}
	jti := claims.ID
	if jti == "" {
		generated, err := randomJTI()
		if err != nil {
			return "", err
		}
		jti = generated
	}
	now := time.Now().UTC()
	token := paseto.NewToken()
	token.SetAudience(Audience)
	// Both backdated by the same allowance, because both are "not in the
	// future" claims and a verifier checks both. Backdating only NotBefore is
	// not an allowance at all: a guest whose clock is a second ahead of the
	// control plane's mints a token whose IssuedAt is in the verifier's future,
	// and the verifier rejects it with "the ValidAt time is before this token
	// was issued" — which is exactly what a pool VM does, because it steps its
	// clock off the host RTC every 30s and spends the interval drifting either
	// side of it. That 401 killed the pool agent on startup.
	token.SetIssuedAt(now.Add(-ClockSkew))
	token.SetNotBefore(now.Add(-ClockSkew))
	token.SetExpiration(now.Add(ttl))
	token.SetJti(jti)
	token.SetString("project_id", claims.ProjectID)
	token.SetString("pool_id", claims.PoolID)
	if len(claims.Scopes) > 0 {
		if err := token.Set("scopes", claims.Scopes); err != nil {
			return "", fmt.Errorf("set pool assertion scopes: %w", err)
		}
	}

	secretKey, err := paseto.NewV4AsymmetricSecretKeyFromEd25519(privateKey)
	if err != nil {
		return "", fmt.Errorf("load pool private key: %w", err)
	}
	return token.V4Sign(secretKey, nil), nil
}

func VerifyToken(publicKeyText, tokenText string) (Claims, error) {
	publicKey, err := parsePublicKey(publicKeyText)
	if err != nil {
		return Claims{}, err
	}
	parser := paseto.NewParserForValidNow()
	parser.AddRule(paseto.ForAudience(Audience))
	token, err := parser.ParseV4Public(publicKey, tokenText, nil)
	if err != nil {
		return Claims{}, err
	}
	projectID, err := token.GetString("project_id")
	if err != nil {
		return Claims{}, fmt.Errorf("read project_id claim: %w", err)
	}
	poolID, err := token.GetString("pool_id")
	if err != nil {
		return Claims{}, fmt.Errorf("read pool_id claim: %w", err)
	}
	jti, _ := token.GetJti()
	var scopes []string
	_ = token.Get("scopes", &scopes)
	return Claims{ProjectID: projectID, PoolID: poolID, ID: jti, Scopes: scopes}, nil
}

func EncodePublicKey(publicKey ed25519.PublicKey) (string, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return "", fmt.Errorf("pool public key length = %d, want %d", len(publicKey), ed25519.PublicKeySize)
	}
	return base64.StdEncoding.EncodeToString(publicKey), nil
}

func parsePublicKey(publicKeyText string) (paseto.V4AsymmetricPublicKey, error) {
	publicKeyBytes, err := base64.StdEncoding.DecodeString(publicKeyText)
	if err != nil {
		return paseto.V4AsymmetricPublicKey{}, fmt.Errorf("decode pool public key: %w", err)
	}
	if len(publicKeyBytes) != ed25519.PublicKeySize {
		return paseto.V4AsymmetricPublicKey{}, fmt.Errorf("pool public key length = %d, want %d", len(publicKeyBytes), ed25519.PublicKeySize)
	}
	publicKey, err := paseto.NewV4AsymmetricPublicKeyFromEd25519(ed25519.PublicKey(publicKeyBytes))
	if err != nil {
		return paseto.V4AsymmetricPublicKey{}, fmt.Errorf("load pool public key: %w", err)
	}
	return publicKey, nil
}

func randomJTI() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate pool assertion id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
