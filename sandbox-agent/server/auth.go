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

	"github.com/discobox-ai/discobox/gitbackend"
	"github.com/discobox-ai/discobox/sandboxconfig"
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
	// (ADR 26-09-22-838 §2). It is its own scope because it is its own authority: a
	// token that may ask the judge may read and write nothing in the sandbox,
	// and a token for a discobox's own work cannot ask.
	ScopeJudgeRun = "judge:run"
	// ScopeRuntimeConfig gates the runtime-config intake, reading and
	// delivering alike (ADR 0126 §3). It is the pool's alone: what a sandbox
	// is told to be is not something its users may tell it, so no wildcard
	// grants it and a token has to name it. The pool signs it with its own
	// key (ADR 26-10-08-127 §3).
	ScopeRuntimeConfig = sandboxconfig.RuntimeConfigScope
	// ScopeSandboxRead and ScopeSandboxWrite gate the sandbox's own Git
	// repositories, as the pool's worktree route always has: a fetch reads,
	// a push writes (ADR 0126 §4).
	ScopeSandboxRead  = "sandbox:read"
	ScopeSandboxWrite = "sandbox:write"
)

// poolOnlyScopes are the scopes a "*" token does not carry.
var poolOnlyScopes = map[string]bool{ScopeRuntimeConfig: true}

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
		case scope:
			return true
		case "*":
			if !poolOnlyScopes[scope] {
				return true
			}
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

// SignedTokenAuthenticator verifies the tokens the sandbox agent is called
// with. Every token is the control plane's, except one kind: the pool signs
// the token that delivers a runtime-config document with its own key, and a
// token verified by that key may carry the runtime-config scope and nothing
// else (ADR 26-10-08-127 §3).
type SignedTokenAuthenticator struct {
	identity  Identity
	publicKey paseto.V4AsymmetricPublicKey
	// poolKey is the pool's key from the bootstrap, nil when none was placed.
	poolKey *paseto.V4AsymmetricPublicKey
}

func NewSignedTokenAuthenticator(identity Identity, publicKeyText, poolPublicKeyText string) (*SignedTokenAuthenticator, error) {
	if strings.TrimSpace(publicKeyText) == "" {
		return nil, errors.New("sandbox-agent control plane public key is required")
	}
	publicKey, err := parsePublicKey(publicKeyText)
	if err != nil {
		return nil, fmt.Errorf("sandbox-agent control plane public key: %w", err)
	}
	a := &SignedTokenAuthenticator{identity: identity, publicKey: publicKey}
	if strings.TrimSpace(poolPublicKeyText) != "" {
		poolKey, err := parsePublicKey(poolPublicKeyText)
		if err != nil {
			return nil, fmt.Errorf("sandbox-agent pool public key: %w", err)
		}
		a.poolKey = &poolKey
	}
	return a, nil
}

// parsePublicKey decodes a base64 Ed25519 public key.
func parsePublicKey(text string) (paseto.V4AsymmetricPublicKey, error) {
	keyBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil {
		return paseto.V4AsymmetricPublicKey{}, fmt.Errorf("decode: %w", err)
	}
	if len(keyBytes) != ed25519.PublicKeySize {
		return paseto.V4AsymmetricPublicKey{}, fmt.Errorf("length = %d, want %d", len(keyBytes), ed25519.PublicKeySize)
	}
	return paseto.NewV4AsymmetricPublicKeyFromEd25519(ed25519.PublicKey(keyBytes))
}

func (a *SignedTokenAuthenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenText, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		token, fromPool, err := a.parseToken(tokenText)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		claims, err := signedTokenClaimsFromToken(token)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		if fromPool {
			err = a.authorizePoolToken(r, claims)
		}
		if err == nil {
			err = a.authorizeRequest(r, claims)
		}
		if err != nil {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(withSignedTokenClaims(r.Context(), claims)))
	})
}

// parseToken verifies tokenText against the control plane's key, then the
// pool's, and says which verified it.
func (a *SignedTokenAuthenticator) parseToken(tokenText string) (*paseto.Token, bool, error) {
	parser := paseto.NewParserForValidNow()
	parser.AddRule(paseto.ForAudience(SandboxAgentAudience))
	token, err := parser.ParseV4Public(a.publicKey, tokenText, nil)
	if err == nil || a.poolKey == nil {
		return token, false, err
	}
	if token, poolErr := parser.ParseV4Public(*a.poolKey, tokenText, nil); poolErr == nil {
		return token, true, nil
	}
	return nil, false, err
}

// authorizePoolToken confines a pool-signed token to what the pool may sign
// for: exactly the runtime-config scope, on a route whose required scope is
// runtime-config, naming this sandbox's pool. Those routes are the pool's:
// the document itself and a source's project layer, which the pool reads to
// settle the spec. A route added under that scope is reachable by the pool
// too, so putting one there is deciding that. The scope list is checked
// whole, so a pool token can never carry a wildcard or a second scope that a
// later check would honor.
func (a *SignedTokenAuthenticator) authorizePoolToken(r *http.Request, claims SignedTokenClaims) error {
	if len(claims.Scopes) != 1 || claims.Scopes[0] != ScopeRuntimeConfig {
		return errors.New("a pool-signed token may carry only the runtime-config scope")
	}
	if requiredRequestScope(r) != ScopeRuntimeConfig {
		return errors.New("a pool-signed token is accepted only on routes that require the runtime-config scope")
	}
	if claims.PoolID == "" || claims.PoolID != a.identity.PoolID {
		return errors.New("a pool-signed token must name this sandbox's pool")
	}
	return nil
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
	// Read from the escaped path, which is what the router matches and the
	// handlers read their ids from. The decoded path is a different string once
	// a segment carries an escaped slash: ".../sandboxes/s%2Fx/git-repositories/..."
	// decodes to a path whose sandbox is "s" and whose next segment is not the
	// route the router serves.
	projectID, sandboxID, ok := routeIdentity(r.URL.EscapedPath())
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
	// The Git route is told by its route segment, before any suffix test: a
	// repository is named by its source's slug, and a slug like "execs" or
	// "status" would otherwise be gated as something it is not. The segment is
	// read from the escaped path the router matched, so no escaped slash can
	// make this miss a request the router hands the Git route.
	if gitRepositoryRoute(r.URL.EscapedPath()) {
		if gitbackend.IsReceivePack(r) {
			return ScopeSandboxWrite
		}
		return ScopeSandboxRead
	}
	// A source's project layer is read by the pool to settle the sandbox's
	// spec (ADR 0126 §4), on the same scope as the document that names the
	// source. Told by its route segments, like the Git route above and for the
	// same reason: the slug is a path segment and can spell what the tests
	// below look for — a source named "execs" is not an exec.
	if sourceProjectLayerRoute(r.URL.EscapedPath()) {
		return ScopeRuntimeConfig
	}
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
	// The runtime-config intake is the pool's, on a scope nothing else
	// carries, whatever the method: a route answering a method it does not
	// serve must not be the one that needs no token scope at all.
	if strings.HasSuffix(r.URL.Path, "/runtime-config") {
		return ScopeRuntimeConfig
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

// gitRepositoryRoute reports whether path is under
// /api/projects/{projectId}/sandboxes/{sandboxId}/git-repositories/.
func gitRepositoryRoute(path string) bool {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	return len(segments) > 6 && segments[5] == "git-repositories"
}

// sourceProjectLayerRoute reports whether path is
// /api/projects/{projectId}/sandboxes/{sandboxId}/sources/{slug}/project-layer.
func sourceProjectLayerRoute(path string) bool {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	return len(segments) == 8 && segments[5] == "sources" && segments[7] == "project-layer"
}

func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}
