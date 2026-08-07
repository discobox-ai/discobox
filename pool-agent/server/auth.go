package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"aidanwoods.dev/go-paseto"

	workerapi "github.com/obot-platform/discobox/pool-agent/api/gen"
)

const (
	PoolAgentAudience = "pool-agent"

	ScopeSandboxRead   = "sandbox:read"
	ScopeSandboxWrite  = "sandbox:write"
	ScopeSandboxHTTP   = "sandbox:http"
	ScopeTerminalRead  = "terminal:read"
	ScopeTerminalWrite = "terminal:write"
	ScopeExecRead      = "exec:read"
	ScopeExecWrite     = "exec:write"
	// ScopeTCPConnect gates the direct-tcpip tunnel endpoint (ADR 0024 §3):
	// dialing an arbitrary host:port from inside the sandbox's own network
	// namespace, distinct from ScopeSandboxHTTP's container-IP-only reach.
	ScopeTCPConnect = "tcp:connect"
	// ScopePoolSync authorizes host-wide pool reconciliation (reaping pools not
	// in the known set). Only the control-plane provider driver carries it.
	ScopePoolSync = "pool:sync"
)

type signedTokenClaimsContextKey struct{}

type SignedTokenClaims struct {
	ProjectID string
	PoolID    string
	SandboxID string
	Scopes    []string
}

func (c SignedTokenClaims) HasScope(scope string) bool {
	for _, candidate := range c.Scopes {
		switch candidate {
		case scope, "*":
			return true
		case "sandbox:*":
			if strings.HasPrefix(scope, "sandbox:") {
				return true
			}
		case "terminal:*":
			if strings.HasPrefix(scope, "terminal:") {
				return true
			}
		case "exec:*":
			if strings.HasPrefix(scope, "exec:") {
				return true
			}
		case "tcp:*":
			if strings.HasPrefix(scope, "tcp:") {
				return true
			}
		}
	}
	return false
}

func SignedTokenClaimsFromContext(ctx context.Context) (SignedTokenClaims, bool) {
	claims, ok := ctx.Value(signedTokenClaimsContextKey{}).(SignedTokenClaims)
	return claims, ok
}

func withSignedTokenClaims(ctx context.Context, claims SignedTokenClaims) context.Context {
	return context.WithValue(ctx, signedTokenClaimsContextKey{}, claims)
}

type SignedTokenAuthenticator struct {
	identity  Identity
	publicKey paseto.V4AsymmetricPublicKey
}

func NewSignedTokenAuthenticator(identity Identity, publicKeyText string) (*SignedTokenAuthenticator, error) {
	publicKeyText = strings.TrimSpace(publicKeyText)
	if publicKeyText == "" {
		return nil, errors.New("pool-agent control plane public key is required")
	}
	publicKeyBytes, err := base64.StdEncoding.DecodeString(publicKeyText)
	if err != nil {
		return nil, fmt.Errorf("decode pool-agent control plane public key: %w", err)
	}
	if len(publicKeyBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("pool-agent control plane public key length = %d, want %d", len(publicKeyBytes), ed25519.PublicKeySize)
	}
	publicKey, err := paseto.NewV4AsymmetricPublicKeyFromEd25519(ed25519.PublicKey(publicKeyBytes))
	if err != nil {
		return nil, fmt.Errorf("load pool-agent control plane public key: %w", err)
	}
	return &SignedTokenAuthenticator{identity: identity, publicKey: publicKey}, nil
}

func (a *SignedTokenAuthenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenText, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		token, err := a.parseToken(tokenText)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		claims, err := signedTokenClaimsFromToken(token)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		if err := a.authorizeRequestPath(r.URL.Path, claims); err != nil {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(withSignedTokenClaims(r.Context(), claims)))
	})
}

func (a *SignedTokenAuthenticator) parseToken(tokenText string) (*paseto.Token, error) {
	parser := paseto.NewParserForValidNow()
	parser.AddRule(paseto.ForAudience(PoolAgentAudience))
	return parser.ParseV4Public(a.publicKey, tokenText, nil)
}

func signedTokenClaimsFromToken(token *paseto.Token) (SignedTokenClaims, error) {
	projectID, err := token.GetString("project_id")
	if err != nil {
		return SignedTokenClaims{}, fmt.Errorf("read project_id claim: %w", err)
	}
	poolID, err := token.GetString("pool_id")
	if err != nil {
		return SignedTokenClaims{}, fmt.Errorf("read pool_id claim: %w", err)
	}
	var sandboxID string
	_ = token.Get("sandbox_id", &sandboxID)
	var scopes []string
	if err := token.Get("scopes", &scopes); err != nil {
		return SignedTokenClaims{}, fmt.Errorf("read scopes claim: %w", err)
	}
	return SignedTokenClaims{
		ProjectID: projectID,
		PoolID:    poolID,
		SandboxID: sandboxID,
		Scopes:    scopes,
	}, nil
}

func (a *SignedTokenAuthenticator) authorizeRequestPath(path string, claims SignedTokenClaims) error {
	if claims.ProjectID == "" || claims.PoolID == "" {
		return errors.New("missing pool-agent token identity")
	}
	if claims.ProjectID != a.identity.ProjectID || claims.PoolID != a.identity.PoolID {
		return errors.New("pool-agent token identity does not match this pool")
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) < 4 || segments[0] != "api" || segments[1] != "project" || segments[3] != "pool" {
		return errors.New("pool-agent route identity not found")
	}
	if segments[2] != claims.ProjectID || len(segments) < 5 || segments[4] != claims.PoolID {
		return errors.New("pool-agent token identity does not match route")
	}
	if claims.SandboxID != "" {
		if sandboxID, ok := sandboxIDFromPathSegments(segments); ok && sandboxID != claims.SandboxID {
			return errors.New("pool-agent token sandbox does not match route")
		}
	}
	return nil
}

func sandboxIDFromPathSegments(segments []string) (string, bool) {
	for i := 0; i < len(segments)-1; i++ {
		if segments[i] == "sandboxes" {
			return segments[i+1], true
		}
	}
	return "", false
}

func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}

func requiredPoolOperationScope(operation workerapi.OperationName) string {
	switch operation {
	case workerapi.PoolListSandboxesOperation, workerapi.PoolGetSandboxOperation:
		return ScopeSandboxRead
	case workerapi.PoolCreateSandboxOperation,
		workerapi.PoolUpdateSandboxOperation,
		workerapi.PoolArchiveSandboxOperation,
		workerapi.PoolDeleteSandboxOperation,
		workerapi.PoolStartSandboxOperation,
		workerapi.PoolStopSandboxOperation:
		return ScopeSandboxWrite
	case workerapi.PoolSyncOperation:
		return ScopePoolSync
	default:
		return ""
	}
}
