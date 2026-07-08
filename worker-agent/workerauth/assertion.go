// Package workerauth creates and verifies worker-to-control-plane assertions.
package workerauth

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
	// the long-lived token the worker mints for the isolated proxy unit.
	ScopeSecretResolve = "secret:resolve"
)

type Claims struct {
	ProjectID string
	WorkerID  string
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

// CreateTokenWithTTL signs a worker assertion with an explicit lifetime. It is
// used for the longer-lived scoped resolve token distributed to the proxy unit.
func CreateTokenWithTTL(privateKey ed25519.PrivateKey, claims Claims, ttl time.Duration) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("worker private key length = %d, want %d", len(privateKey), ed25519.PrivateKeySize)
	}
	if claims.ProjectID == "" || claims.WorkerID == "" {
		return "", errors.New("project_id and worker_id claims are required")
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
	token.SetIssuedAt(now)
	token.SetNotBefore(now.Add(-ClockSkew))
	token.SetExpiration(now.Add(ttl))
	token.SetJti(jti)
	token.SetString("project_id", claims.ProjectID)
	token.SetString("worker_id", claims.WorkerID)
	if len(claims.Scopes) > 0 {
		if err := token.Set("scopes", claims.Scopes); err != nil {
			return "", fmt.Errorf("set worker assertion scopes: %w", err)
		}
	}

	secretKey, err := paseto.NewV4AsymmetricSecretKeyFromEd25519(privateKey)
	if err != nil {
		return "", fmt.Errorf("load worker private key: %w", err)
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
	workerID, err := token.GetString("worker_id")
	if err != nil {
		return Claims{}, fmt.Errorf("read worker_id claim: %w", err)
	}
	jti, _ := token.GetJti()
	var scopes []string
	_ = token.Get("scopes", &scopes)
	return Claims{ProjectID: projectID, WorkerID: workerID, ID: jti, Scopes: scopes}, nil
}

func EncodePublicKey(publicKey ed25519.PublicKey) (string, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return "", fmt.Errorf("worker public key length = %d, want %d", len(publicKey), ed25519.PublicKeySize)
	}
	return base64.StdEncoding.EncodeToString(publicKey), nil
}

func parsePublicKey(publicKeyText string) (paseto.V4AsymmetricPublicKey, error) {
	publicKeyBytes, err := base64.StdEncoding.DecodeString(publicKeyText)
	if err != nil {
		return paseto.V4AsymmetricPublicKey{}, fmt.Errorf("decode worker public key: %w", err)
	}
	if len(publicKeyBytes) != ed25519.PublicKeySize {
		return paseto.V4AsymmetricPublicKey{}, fmt.Errorf("worker public key length = %d, want %d", len(publicKeyBytes), ed25519.PublicKeySize)
	}
	publicKey, err := paseto.NewV4AsymmetricPublicKeyFromEd25519(ed25519.PublicKey(publicKeyBytes))
	if err != nil {
		return paseto.V4AsymmetricPublicKey{}, fmt.Errorf("load worker public key: %w", err)
	}
	return publicKey, nil
}

func randomJTI() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate worker assertion id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
