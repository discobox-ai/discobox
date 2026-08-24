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
		// Two unrelated faults arrive here and used to read identically, which
		// left the only visible symptom — a bare 401 the pool agent exits on —
		// pointing at nothing.
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

var poolRuntimeActions = map[string]struct{}{
	"sandbox-states":              {},
	"status":                      {},
	"resolve-sandbox-secret":      {},
	"sandbox-agent-status-tokens": {},
	"sandbox-agent-status":        {},
}

func isPoolRuntimePath(path string) bool {
	if !strings.HasPrefix(path, "/api/pools/") {
		return false
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) != 4 {
		return false
	}
	_, ok := poolRuntimeActions[segments[3]]
	return ok
}

func poolIDFromRuntimePath(path string) (string, error) {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) != 4 || segments[0] != "api" || segments[1] != "pools" {
		return "", errors.New("pool runtime path is invalid")
	}
	if _, ok := poolRuntimeActions[segments[3]]; !ok {
		return "", errors.New("pool runtime path is invalid")
	}
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
