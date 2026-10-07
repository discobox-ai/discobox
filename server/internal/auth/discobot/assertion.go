// Package discobot verifies the assertions discobot signs to say which person
// a request is for (ADR 26-10-07-005 §2). discobox keeps no users: it takes
// discobot's word for the person, checked against discobot's Ed25519 public
// key, and records and propagates that person as it does any principal.
//
// An assertion is a PASETO v4.public token, the format the server already
// verifies for pool agents (pool-agent/poolauth), carried in AssertionHeader.
// Sign is the reference for producing one; discobot's own signer is outside
// this repository.
package discobot

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"aidanwoods.dev/go-paseto"
)

const (
	// AssertionHeader carries the assertion. It is its own header, not
	// Authorization, which other callers fill with bearer tokens of their own.
	AssertionHeader = "X-Discobot-Assertion"
	Issuer          = "discobot"
	Audience        = "discobox"
	// MaxLifetime bounds how far ahead an assertion may expire. One is signed
	// per request and good for moments; a long-lived one would be a credential
	// in all but name, and there is no revoking it.
	MaxLifetime = 5 * time.Minute
)

// Claims is what an assertion says.
type Claims struct {
	// UserID is the person, by discobot's ID for them (sub).
	UserID string
	// Name is how discobot shows them.
	Name string
	// ProjectID is the one project the request may act in. Empty for a request
	// that acts in none, such as listing or creating projects.
	ProjectID string
	// ID is the assertion's own (jti).
	ID string
}

// ParsePublicKey reads discobot's public key: base64 of the 32 raw Ed25519
// bytes.
func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil {
		return nil, fmt.Errorf("decode discobot public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("discobot public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// Verify checks an assertion against discobot's public key and returns what
// it says. It must be signed by that key, issued by discobot for discobox,
// valid now, expire within MaxLifetime, and name a person and itself.
func Verify(publicKey ed25519.PublicKey, token string) (Claims, error) {
	key, err := paseto.NewV4AsymmetricPublicKeyFromEd25519(publicKey)
	if err != nil {
		return Claims{}, fmt.Errorf("load discobot public key: %w", err)
	}
	parser := paseto.NewParserForValidNow()
	parser.AddRule(paseto.ForAudience(Audience))
	parser.AddRule(paseto.IssuedBy(Issuer))
	parsed, err := parser.ParseV4Public(key, token, nil)
	if err != nil {
		return Claims{}, err
	}
	expiration, err := parsed.GetExpiration()
	if err != nil {
		return Claims{}, errors.New("the assertion has no expiry")
	}
	if expiration.After(time.Now().Add(MaxLifetime)) {
		return Claims{}, fmt.Errorf("the assertion expires at %s, more than %s ahead", expiration.UTC().Format(time.RFC3339), MaxLifetime)
	}
	subject, err := parsed.GetSubject()
	if err != nil || strings.TrimSpace(subject) == "" {
		return Claims{}, errors.New("the assertion names no person")
	}
	jti, err := parsed.GetJti()
	if err != nil || strings.TrimSpace(jti) == "" {
		return Claims{}, errors.New("the assertion has no ID")
	}
	claims := Claims{UserID: strings.TrimSpace(subject), ID: jti}
	// The name and project are optional claims; a present one must be a string.
	if err := optionalString(parsed, "name", &claims.Name); err != nil {
		return Claims{}, err
	}
	if err := optionalString(parsed, "project_id", &claims.ProjectID); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

// Sign produces an assertion for claims, valid for ttl from now. It is the
// format's reference; discobot signs its own.
func Sign(privateKey ed25519.PrivateKey, claims Claims, ttl time.Duration) (string, error) {
	key, err := paseto.NewV4AsymmetricSecretKeyFromEd25519(privateKey)
	if err != nil {
		return "", fmt.Errorf("load discobot signing key: %w", err)
	}
	now := time.Now()
	token := paseto.NewToken()
	token.SetIssuer(Issuer)
	token.SetAudience(Audience)
	token.SetSubject(claims.UserID)
	token.SetJti(claims.ID)
	token.SetIssuedAt(now)
	token.SetNotBefore(now)
	token.SetExpiration(now.Add(ttl))
	if claims.Name != "" {
		token.SetString("name", claims.Name)
	}
	if claims.ProjectID != "" {
		token.SetString("project_id", claims.ProjectID)
	}
	return token.V4Sign(key, nil), nil
}

func optionalString(token *paseto.Token, claim string, into *string) error {
	var value any
	if err := token.Get(claim, &value); err != nil {
		return nil //nolint:nilerr // An absent claim is allowed.
	}
	text, ok := value.(string)
	if !ok {
		return fmt.Errorf("the assertion's %s is not a string", claim)
	}
	*into = strings.TrimSpace(text)
	return nil
}
