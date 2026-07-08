package auth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/obot-platform/discobox/server/internal/store"
	"github.com/obot-platform/discobox/worker-agent/workerauth"
)

// Authenticator authenticates a request and returns the matched principal.
type Authenticator interface {
	Authenticate(*http.Request) (Principal, bool, error)
}

// WorkerAuthenticator authenticates worker runtime requests from signed worker assertions.
type WorkerAuthenticator struct {
	Store *store.Store
}

func (a WorkerAuthenticator) Authenticate(r *http.Request) (Principal, bool, error) {
	if !isWorkerRuntimePath(r.URL.Path) {
		return Principal{}, false, nil
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		return Principal{}, false, errors.New("worker assertion required")
	}
	routeWorkerID, err := workerIDFromRuntimePath(r.URL.Path)
	if err != nil {
		return Principal{}, false, err
	}
	worker, err := a.Store.GetWorker(r.Context(), routeWorkerID)
	if err != nil {
		return Principal{}, false, errors.New("worker not found")
	}
	if worker.RevokedAt != nil {
		return Principal{}, false, errors.New("worker is revoked")
	}
	if worker.KeyType != workerauth.KeyType {
		return Principal{}, false, errors.New("unsupported worker key type")
	}
	claims, err := workerauth.VerifyToken(worker.PublicKey, token)
	if err != nil {
		return Principal{}, false, errors.New("invalid worker assertion")
	}
	if claims.WorkerID != worker.ID || claims.WorkerID != routeWorkerID || claims.ProjectID != worker.ProjectID {
		return Principal{}, false, errors.New("worker assertion identity does not match route")
	}
	return Principal{Type: PrincipalTypeWorker, WorkerID: claims.WorkerID, Scopes: claims.Scopes}, true, nil
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

var workerRuntimeActions = map[string]struct{}{
	"status":                 {},
	"resolve-sandbox-secret": {},
}

func isWorkerRuntimePath(path string) bool {
	if !strings.HasPrefix(path, "/api/workers/") {
		return false
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) != 4 {
		return false
	}
	_, ok := workerRuntimeActions[segments[3]]
	return ok
}

func workerIDFromRuntimePath(path string) (string, error) {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) != 4 || segments[0] != "api" || segments[1] != "workers" {
		return "", errors.New("worker runtime path is invalid")
	}
	if _, ok := workerRuntimeActions[segments[3]]; !ok {
		return "", errors.New("worker runtime path is invalid")
	}
	workerID, err := url.PathUnescape(segments[2])
	if err != nil {
		return "", err
	}
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return "", errors.New("worker ID is required")
	}
	return workerID, nil
}
