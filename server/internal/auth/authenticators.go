package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/poolauth"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// Authenticator authenticates a request and returns the matched principal.
type Authenticator interface {
	Authenticate(*http.Request) (Principal, bool, error)
}

// PoolAuthenticator authenticates pool agent runtime requests from signed
// agent assertions verified against the pool's registered public key.
type PoolAuthenticator struct {
	Store *store.Store
}

func (a PoolAuthenticator) Authenticate(r *http.Request) (Principal, bool, error) {
	if !isPoolRuntimePath(r.URL.Path) {
		return Principal{}, false, nil
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		return Principal{}, false, errors.New("pool agent assertion required")
	}
	routePoolID, err := poolIDFromRuntimePath(r.URL.Path)
	if err != nil {
		return Principal{}, false, err
	}
	pool, err := a.Store.GetPoolByID(r.Context(), routePoolID)
	if err != nil {
		return Principal{}, false, errors.New("pool not found")
	}
	if pool.RevokedAt != nil {
		return Principal{}, false, errors.New("pool is revoked")
	}
	if pool.KeyType != poolauth.KeyType {
		return Principal{}, false, errors.New("unsupported pool key type")
	}
	claims, err := poolauth.VerifyToken(pool.PublicKey, token)
	if err != nil {
		// Two unrelated faults arrive here, and they must not read
		// identically: that would leave the only visible symptom — a bare 401
		// the pool agent exits on — pointing at nothing.
		//
		// A signature that does not verify means the control plane holds a
		// different key than the agent signs with: a restored database, or a
		// pool row recreated under an agent that kept its stored key.
		//
		// A token outside its validity window means the two machines disagree
		// about the time. On a Mac that slept, that is the guest's clock hours
		// behind the host's, minting assertions the control plane reads as long
		// expired (ADR 0062). Naming the control plane's own clock is what makes
		// the gap legible from the agent's log, which is the side that reports
		// the failure.
		return Principal{}, false, fmt.Errorf("invalid pool agent assertion: %w (control plane clock %s)",
			err, time.Now().UTC().Format(time.RFC3339))
	}
	if claims.PoolID != pool.ID || claims.PoolID != routePoolID || claims.ProjectID != pool.ProjectID {
		return Principal{}, false, errors.New("pool agent assertion identity does not match route")
	}
	return Principal{Type: PrincipalTypePool, PoolID: claims.PoolID, Scopes: claims.Scopes}, true, nil
}

// SandboxForwardAuthenticator authenticates a sandbox's own call to the
// discobox API, which the pool hosting it forwarded from the reserved host
// (ADR 0140 §§2–3). It takes the pool's word for which sandbox is calling, and
// checks only what makes that word good: the pool's assertion verifies, carries
// the forwarding scope, comes from a pool that is not revoked, and names a
// sandbox the control plane placed on that pool.
//
// It fails rather than stepping aside once a request names a forwarded
// sandbox. A forwarded call that fell through would be answered by the next
// authenticator, and the next one answers every request as the default user.
type SandboxForwardAuthenticator struct {
	Store *store.Store
}

func (a SandboxForwardAuthenticator) Authenticate(r *http.Request) (Principal, bool, error) {
	sandboxID := strings.TrimSpace(r.Header.Get(poolauth.ForwardedSandboxHeader))
	if sandboxID == "" {
		return Principal{}, false, nil
	}
	refuse := func(reason string) (Principal, bool, error) {
		return Principal{}, false, errors.New("forwarded sandbox call refused: " + reason)
	}
	token := bearerToken(r.Header.Get("Authorization"))
	poolID := strings.TrimSpace(r.Header.Get(poolauth.ForwardingPoolHeader))
	if token == "" || poolID == "" {
		return refuse("its pool's assertion is required")
	}
	pool, err := a.Store.GetPoolByID(r.Context(), poolID)
	if err != nil {
		return refuse("pool not found")
	}
	if pool.RevokedAt != nil {
		return refuse("pool is revoked")
	}
	if pool.KeyType != poolauth.KeyType {
		return refuse("unsupported pool key type")
	}
	claims, err := poolauth.VerifyToken(pool.PublicKey, token)
	if err != nil {
		return refuse("invalid pool assertion: " + err.Error())
	}
	if claims.PoolID != pool.ID || claims.ProjectID != pool.ProjectID || !claims.HasScope(poolauth.ScopeSandboxForward) {
		return refuse("the pool's assertion does not allow forwarding")
	}
	sandbox, err := a.Store.GetSandboxByID(r.Context(), sandboxID)
	if err != nil || sandbox.PoolID != pool.ID || sandbox.ProjectID != pool.ProjectID {
		return refuse("the pool does not host that sandbox")
	}
	return Principal{
		Type:      PrincipalTypeSandbox,
		SandboxID: sandbox.ID,
		ProjectID: sandbox.ProjectID,
		PoolID:    pool.ID,
		UserID:    sandbox.CreatedByUserID,
	}, true, nil
}

// DefaultUserAuthenticator authenticates every request as the configured user.
type DefaultUserAuthenticator struct {
	UserID string
}

func (a DefaultUserAuthenticator) Authenticate(*http.Request) (Principal, bool, error) {
	return Principal{
		Type:   PrincipalTypeUser,
		UserID: a.UserID,
		Scopes: []string{ScopeAll},
	}, true, nil
}

func bearerToken(authorization string) string {
	authorization = strings.TrimSpace(authorization)
	if authorization == "" {
		return ""
	}
	parts := strings.Fields(authorization)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

// poolRuntimeActions allowlists the actions a pool agent may reach under
// /api/pools/{poolId}/. It is an allowlist rather than a pattern on purpose:
// a route that is not named here is not authenticated as a pool route at all,
// so a new or misspelled one fails closed instead of inheriting pool access.
//
// The cost of that is a step every new pool route must remember, and forgetting
// it is invisible to any test that calls a service directly — the route simply
// answers 403 to the real agent. Add the action here in the same change that
// adds the route.
//
// The value reports whether the action addresses a specific resource, i.e.
// whether one trailing path segment (its ID) belongs to the route.
var poolRuntimeActions = map[string]bool{
	"sandbox-states":         false,
	"status":                 false,
	"resolve-sandbox-secret": false,
	// What an upstream made of a swapped credential (ADR 0132). One call
	// reports one verdict, so it takes no trailing ID.
	"sandbox-secret-rejections": false,
	// Putting a job to the project's judge (ADR 26-09-22-838 §2). One call is one
	// job, so it takes no trailing ID.
	"judge":                       false,
	"sandbox-agent-status-tokens": false,
	"sandbox-agent-status":        false,
	// The pool's resource report (ADR 0071, resource accounting). It addresses the pool itself, so
	// it takes no trailing ID.
	"resources": false,
	// The agent credentials broker (ADR 0031).
	"sandbox-credentials":         false,
	"sandbox-credential-requests": true, // .../{requestId} polls one request
	// The judge's verdict trail (ADR 0091). It addresses no resource of its
	// own — one call records one verdict — so it takes no trailing ID.
	"sandbox-credential-verdicts": false,
	// Host trust (ADR 0149): an agent's ask, polled by its ID, and the pool's
	// read of every live trust its proxy enforces.
	"sandbox-trust-requests": true, // .../{requestId} polls one request
	"sandbox-host-trusts":    false,
}

func isPoolRuntimePath(path string) bool {
	_, ok := poolRuntimeAction(path)
	return ok
}

// poolRuntimeAction returns the allowlisted action a pool runtime path names.
func poolRuntimeAction(path string) (string, bool) {
	if !strings.HasPrefix(path, "/api/pools/") {
		return "", false
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) < 4 || len(segments) > 5 {
		return "", false
	}
	allowsResourceID, ok := poolRuntimeActions[segments[3]]
	if !ok {
		return "", false
	}
	// A trailing segment is only ever a resource ID, and only for the actions
	// that take one. Accepting it everywhere would let an unlisted subroute of a
	// listed action inherit pool access.
	if len(segments) == 5 && !allowsResourceID {
		return "", false
	}
	return segments[3], true
}

func poolIDFromRuntimePath(path string) (string, error) {
	if _, ok := poolRuntimeAction(path); !ok {
		return "", errors.New("pool runtime path is invalid")
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	poolID, err := url.PathUnescape(segments[2])
	if err != nil {
		return "", err
	}
	poolID = strings.TrimSpace(poolID)
	if poolID == "" {
		return "", errors.New("pool ID is required")
	}
	return poolID, nil
}
