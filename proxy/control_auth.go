package proxy

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aidanwoods.dev/go-paseto"
)

const (
	ControlAudience  = "discobox-proxy-control"
	ScopeAuditRead   = "audit:read"
	controlClockSkew = time.Minute
	controlTokenTTL  = 5 * time.Minute
)

type controlClaimsContextKey struct{}

// ControlTokenClaims are the claims a proxy control API token carries.
// CreateControlToken signs them.
type ControlTokenClaims struct {
	ProjectID string
	WorkerID  string
	SandboxID string
	Scopes    []string
}

func (c ControlTokenClaims) hasScope(scope string) bool {
	for _, candidate := range c.Scopes {
		switch candidate {
		case scope, "*":
			return true
		case "audit:*":
			if strings.HasPrefix(scope, "audit:") {
				return true
			}
		}
	}
	return false
}

// ControlTokenClaimsFromContext returns verified control token claims.
func ControlTokenClaimsFromContext(ctx context.Context) (ControlTokenClaims, bool) {
	claims, ok := ctx.Value(controlClaimsContextKey{}).(ControlTokenClaims)
	return claims, ok
}

// CreateControlToken signs a short-lived proxy control token.
func CreateControlToken(privateKey ed25519.PrivateKey, claims ControlTokenClaims) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("control private key length = %d, want %d", len(privateKey), ed25519.PrivateKeySize)
	}
	now := time.Now().UTC()
	token := paseto.NewToken()
	token.SetAudience(ControlAudience)
	// Both backdated by the same allowance: a verifier checks IssuedAt as well
	// as NotBefore, so skew tolerance on one and not the other tolerates
	// nothing. See poolauth.CreateTokenWithTTL.
	token.SetIssuedAt(now.Add(-controlClockSkew))
	token.SetNotBefore(now.Add(-controlClockSkew))
	token.SetExpiration(now.Add(controlTokenTTL))
	token.SetString("project_id", claims.ProjectID)
	token.SetString("worker_id", claims.WorkerID)
	if claims.SandboxID != "" {
		token.SetString("sandbox_id", claims.SandboxID)
	}
	scopes := claims.Scopes
	if len(scopes) == 0 {
		scopes = []string{ScopeAuditRead}
	}
	if err := token.Set("scopes", scopes); err != nil {
		return "", fmt.Errorf("set control scopes: %w", err)
	}
	key, err := paseto.NewV4AsymmetricSecretKeyFromEd25519(privateKey)
	if err != nil {
		return "", fmt.Errorf("load control private key: %w", err)
	}
	return token.V4Sign(key, nil), nil
}

type controlAuthenticator struct {
	publicKey paseto.V4AsymmetricPublicKey
	projectID string
	workerID  string
}

func newControlAuthenticator(cfg ControlConfig) (*controlAuthenticator, error) {
	keyText := strings.TrimSpace(cfg.TrustPublicKey)
	if keyText == "" {
		return nil, nil
	}
	keyBytes, err := base64.StdEncoding.DecodeString(keyText)
	if err != nil {
		return nil, fmt.Errorf("decode proxy control public key: %w", err)
	}
	if len(keyBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("proxy control public key length = %d, want %d", len(keyBytes), ed25519.PublicKeySize)
	}
	publicKey, err := paseto.NewV4AsymmetricPublicKeyFromEd25519(ed25519.PublicKey(keyBytes))
	if err != nil {
		return nil, fmt.Errorf("load proxy control public key: %w", err)
	}
	return &controlAuthenticator{publicKey: publicKey, projectID: cfg.ProjectID, workerID: cfg.WorkerID}, nil
}

func (a *controlAuthenticator) Middleware(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenText, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		claims, err := a.parseClaims(tokenText)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		if err := a.authorize(r, claims); err != nil {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), controlClaimsContextKey{}, claims)
		next.ServeHTTP(w, scopeToToken(r.WithContext(ctx), claims))
	})
}

func (a *controlAuthenticator) parseClaims(tokenText string) (ControlTokenClaims, error) {
	parser := paseto.NewParserForValidNow()
	parser.AddRule(paseto.ForAudience(ControlAudience))
	token, err := parser.ParseV4Public(a.publicKey, tokenText, nil)
	if err != nil {
		return ControlTokenClaims{}, err
	}
	projectID, err := token.GetString("project_id")
	if err != nil {
		return ControlTokenClaims{}, fmt.Errorf("read project_id claim: %w", err)
	}
	workerID, err := token.GetString("worker_id")
	if err != nil {
		return ControlTokenClaims{}, fmt.Errorf("read worker_id claim: %w", err)
	}
	var sandboxID string
	_ = token.Get("sandbox_id", &sandboxID)
	var scopes []string
	if err := token.Get("scopes", &scopes); err != nil {
		return ControlTokenClaims{}, fmt.Errorf("read scopes claim: %w", err)
	}
	return ControlTokenClaims{ProjectID: projectID, WorkerID: workerID, SandboxID: sandboxID, Scopes: scopes}, nil
}

func (a *controlAuthenticator) authorize(r *http.Request, claims ControlTokenClaims) error {
	if a.projectID != "" && claims.ProjectID != a.projectID {
		return errors.New("proxy control token project does not match")
	}
	if a.workerID != "" && claims.WorkerID != a.workerID {
		return errors.New("proxy control token worker does not match")
	}
	if !claims.hasScope(ScopeAuditRead) {
		return errors.New("proxy control token missing audit read scope")
	}
	if claims.SandboxID != "" {
		queryClientID := r.URL.Query().Get("client_id")
		if queryClientID != "" && queryClientID != claims.SandboxID {
			return errors.New("proxy control token sandbox does not match query client")
		}
	}
	return nil
}

// scopeToToken pins client_id to the token's sandbox before any handler runs.
//
// It is here, and not in the handlers, because this is the only place every
// read route passes through. Narrowing at each call site works exactly until
// someone adds a route and does not know they had to: the hole this closes was
// an authorize() check that compared client_id when the caller sent one and
// did nothing when they did not, so a sandbox-scoped token that simply omitted
// the parameter read every sandbox's rows — and, on the artifact routes, their
// spooled request and response bodies. Rewriting the query means a handler
// cannot express the unnarrowed read at all.
//
// A token with no sandbox_id is the pool-wide reader and is left alone. A
// contradictory client_id never reaches here: authorize rejects it, so asking
// for another sandbox is a 403 rather than a silent substitution of your own.
func scopeToToken(r *http.Request, claims ControlTokenClaims) *http.Request {
	if claims.SandboxID == "" {
		return r
	}
	// Copy the URL: r.WithContext is a shallow clone and still shares it with
	// the caller's request.
	scoped := *r.URL
	query := scoped.Query()
	query.Set("client_id", claims.SandboxID)
	scoped.RawQuery = query.Encode()
	r.URL = &scoped
	return r
}

func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}
