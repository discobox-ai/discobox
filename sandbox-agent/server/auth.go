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
)

const (
	SandboxAgentAudience = "sandbox-agent"

	ScopeTerminalRead  = "terminal:read"
	ScopeTerminalWrite = "terminal:write"
	ScopeExecRead      = "exec:read"
	ScopeExecWrite     = "exec:write"
	// ScopeStatusRead authorizes only the sandbox-agent status route. Tokens
	// carrying it are minted server-side with this scope hardcoded, never
	// derived from a caller's request (see server's MintSandboxAgentStatusTokens).
	ScopeStatusRead = "status:read"
	// ScopeTCPConnect gates the direct-tcpip tunnel endpoint (ADR 0024 §3).
	ScopeTCPConnect = "tcp:connect"
	// ScopeUDPConnect gates the UDP tunnel endpoint, its datagram twin
	// (ADR 0109 §4).
	ScopeUDPConnect = "udp:connect"
	// ScopeJudgeRun gates putting a job to the judge, and nothing else
	// (ADR 0150 §2). It is its own scope because it is its own authority: a
	// token that may ask the judge may read and write nothing in the sandbox,
	// and a token for a discobox's own work cannot ask.
	ScopeJudgeRun = "judge:run"
)

type signedTokenClaimsContextKey struct{}

type SignedTokenClaims struct {
	ProjectID string
	SandboxID string
	PoolID    string
	Scopes    []string
}

func (c SignedTokenClaims) HasScope(scope string) bool {
	for _, candidate := range c.Scopes {
		switch candidate {
		case scope, "*":
			return true
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
		case "udp:*":
			if strings.HasPrefix(scope, "udp:") {
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
		return nil, errors.New("sandbox-agent control plane public key is required")
	}
	publicKeyBytes, err := base64.StdEncoding.DecodeString(publicKeyText)
	if err != nil {
		return nil, fmt.Errorf("decode sandbox-agent control plane public key: %w", err)
	}
	if len(publicKeyBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("sandbox-agent control plane public key length = %d, want %d", len(publicKeyBytes), ed25519.PublicKeySize)
	}
	publicKey, err := paseto.NewV4AsymmetricPublicKeyFromEd25519(ed25519.PublicKey(publicKeyBytes))
	if err != nil {
		return nil, fmt.Errorf("load sandbox-agent control plane public key: %w", err)
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
		if err := a.authorizeRequest(r, claims); err != nil {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(withSignedTokenClaims(r.Context(), claims)))
	})
}

func (a *SignedTokenAuthenticator) parseToken(tokenText string) (*paseto.Token, error) {
	parser := paseto.NewParserForValidNow()
	parser.AddRule(paseto.ForAudience(SandboxAgentAudience))
	return parser.ParseV4Public(a.publicKey, tokenText, nil)
}

func signedTokenClaimsFromToken(token *paseto.Token) (SignedTokenClaims, error) {
	projectID, err := token.GetString("project_id")
	if err != nil {
		return SignedTokenClaims{}, fmt.Errorf("read project_id claim: %w", err)
	}
	sandboxID, err := token.GetString("sandbox_id")
	if err != nil {
		return SignedTokenClaims{}, fmt.Errorf("read sandbox_id claim: %w", err)
	}
	var workerID string
	_ = token.Get("pool_id", &workerID)
	var scopes []string
	if err := token.Get("scopes", &scopes); err != nil {
		return SignedTokenClaims{}, fmt.Errorf("read scopes claim: %w", err)
	}
	return SignedTokenClaims{
		ProjectID: projectID,
		SandboxID: sandboxID,
		PoolID:    workerID,
		Scopes:    scopes,
	}, nil
}

func (a *SignedTokenAuthenticator) authorizeRequest(r *http.Request, claims SignedTokenClaims) error {
	if claims.ProjectID != a.identity.ProjectID || claims.SandboxID != a.identity.SandboxID {
		return errors.New("sandbox-agent token identity does not match this sandbox")
	}
	if claims.PoolID != "" && claims.PoolID != a.identity.PoolID {
		return errors.New("sandbox-agent token worker does not match this sandbox")
	}
	projectID, sandboxID, ok := routeIdentity(r.URL.Path)
	if !ok {
		return errors.New("sandbox-agent route identity not found")
	}
	if projectID != claims.ProjectID || sandboxID != claims.SandboxID {
		return errors.New("sandbox-agent token identity does not match route")
	}
	if scope := requiredRequestScope(r); scope != "" && !claims.HasScope(scope) {
		return errors.New("sandbox-agent token missing required scope")
	}
	return nil
}

func routeIdentity(path string) (string, string, bool) {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) < 5 || segments[0] != "api" || segments[1] != "projects" || segments[3] != "sandboxes" {
		return "", "", false
	}
	return segments[2], segments[4], true
}

func requiredRequestScope(r *http.Request) string {
	// The status route reports git/session/connection telemetry and nothing
	// else, so it is gated on its own narrow scope rather than exec:read.
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/status") {
		return ScopeStatusRead
	}
	// Exec events across a sandbox are read-only audit data, read like an exec.
	// Named before /execs, which this path does not contain: without its own
	// entry it would fall through to no scope at all.
	if strings.HasSuffix(r.URL.Path, "/exec-events") {
		if r.Method == http.MethodGet {
			return ScopeExecRead
		}
		return ""
	}
	// Harness hooks are read-only audit data tied to execs (harness terminals).
	if strings.Contains(r.URL.Path, "/harness-hooks") {
		if r.Method == http.MethodGet {
			return ScopeExecRead
		}
		return ""
	}
	if strings.Contains(r.URL.Path, "/execs") {
		if strings.HasSuffix(r.URL.Path, "/attach") {
			return ScopeExecWrite
		}
		// A wait is a POST only because it carries a body; it reads (ADR 0137).
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/wait") {
			return ScopeExecRead
		}
		switch r.Method {
		case http.MethodGet:
			return ScopeExecRead
		case http.MethodPost, http.MethodDelete:
			return ScopeExecWrite
		default:
			return ""
		}
	}
	// Writing the meta file is gated like starting an exec: it is a write into
	// the sandbox's filesystem, and exec:write already allows any such write.
	if strings.HasSuffix(r.URL.Path, "/meta") {
		if r.Method == http.MethodPatch {
			return ScopeExecWrite
		}
		return ""
	}
	// Judging is asked for on its own scope. A judge runtime answers nobody
	// else, so nothing else about it is reachable with this token either.
	if strings.HasSuffix(r.URL.Path, "/judge") {
		if r.Method == http.MethodPost {
			return ScopeJudgeRun
		}
		return ""
	}
	if strings.Contains(r.URL.Path, "/tcp/attach") {
		if r.Method == http.MethodGet {
			return ScopeTCPConnect
		}
		return ""
	}
	if strings.Contains(r.URL.Path, "/udp/attach") {
		if r.Method == http.MethodGet {
			return ScopeUDPConnect
		}
		return ""
	}
	return ""
}

func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}
