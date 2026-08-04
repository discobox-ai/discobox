package auth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/obot-platform/discobox/pool-agent/poolauth"
	"github.com/obot-platform/discobox/server/internal/store"
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
		return Principal{}, false, errors.New("invalid pool agent assertion")
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
	"sandbox-states":         {},
	"status":                 {},
	"resolve-sandbox-secret": {},
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
