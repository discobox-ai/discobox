// Package sandboxtoken issues and verifies the tokens a pool agent gives its
// own sandboxes, for the requests a sandbox makes of its pool rather than of
// the control plane: fetching its sources' origins (ADR 0126 §4).
//
// They are signed with the pool's identity key, so they outlive an agent
// restart exactly as the key does, and carry an audience of their own, so a
// sandbox token is never mistaken for the pool's assertions to the control
// plane (poolauth) or the control plane's tokens to this agent. The same agent
// signs and verifies, against one clock, so there is no skew to allow for.
package sandboxtoken

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"aidanwoods.dev/go-paseto"
)

const (
	// Audience is what every sandbox token is issued for and what Verify
	// requires.
	Audience = "discobox-pool-sandbox"

	// ScopeOriginFetch fetches the origins of the token's own sandbox's
	// sources, and nothing else: never a push, and never another sandbox's.
	ScopeOriginFetch = "origin:fetch"
)

// Claims say which sandbox a token speaks for and what it may do.
type Claims struct {
	ProjectID string
	PoolID    string
	SandboxID string
	Scopes    []string
}

// Issue signs claims for ttl. Every claim is required: a sandbox token always
// names the one sandbox it is for.
func Issue(privateKey ed25519.PrivateKey, claims Claims, ttl time.Duration) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("pool private key length = %d, want %d", len(privateKey), ed25519.PrivateKeySize)
	}
	if claims.ProjectID == "" || claims.PoolID == "" || claims.SandboxID == "" {
		return "", errors.New("project_id, pool_id and sandbox_id claims are required")
	}
	if len(claims.Scopes) == 0 {
		return "", errors.New("a sandbox token needs at least one scope")
	}
	if ttl <= 0 {
		return "", fmt.Errorf("sandbox token lifetime %s must be positive", ttl)
	}
	now := time.Now().UTC()
	token := paseto.NewToken()
	token.SetAudience(Audience)
	token.SetIssuedAt(now)
	token.SetNotBefore(now)
	token.SetExpiration(now.Add(ttl))
	token.SetString("project_id", claims.ProjectID)
	token.SetString("pool_id", claims.PoolID)
	token.SetString("sandbox_id", claims.SandboxID)
	if err := token.Set("scopes", claims.Scopes); err != nil {
		return "", fmt.Errorf("set sandbox token scopes: %w", err)
	}
	secretKey, err := paseto.NewV4AsymmetricSecretKeyFromEd25519(privateKey)
	if err != nil {
		return "", fmt.Errorf("load pool private key: %w", err)
	}
	return token.V4Sign(secretKey, nil), nil
}

// Verifier checks tokens against the public half of the key that issued them.
type Verifier struct {
	publicKey paseto.V4AsymmetricPublicKey
}

func NewVerifier(publicKey ed25519.PublicKey) (*Verifier, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("pool public key length = %d, want %d", len(publicKey), ed25519.PublicKeySize)
	}
	key, err := paseto.NewV4AsymmetricPublicKeyFromEd25519(publicKey)
	if err != nil {
		return nil, fmt.Errorf("load pool public key: %w", err)
	}
	return &Verifier{publicKey: key}, nil
}

// Verify returns the claims of a valid, current sandbox token.
func (v *Verifier) Verify(tokenText string) (Claims, error) {
	parser := paseto.NewParserForValidNow()
	parser.AddRule(paseto.ForAudience(Audience))
	token, err := parser.ParseV4Public(v.publicKey, tokenText, nil)
	if err != nil {
		return Claims{}, err
	}
	var claims Claims
	for name, value := range map[string]*string{
		"project_id": &claims.ProjectID,
		"pool_id":    &claims.PoolID,
		"sandbox_id": &claims.SandboxID,
	} {
		read, err := token.GetString(name)
		if err != nil || read == "" {
			return Claims{}, fmt.Errorf("sandbox token has no %s claim", name)
		}
		*value = read
	}
	if err := token.Get("scopes", &claims.Scopes); err != nil {
		return Claims{}, fmt.Errorf("read scopes claim: %w", err)
	}
	return claims, nil
}
