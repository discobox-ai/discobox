package auth

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"

	"github.com/obot-platform/discobox/server/internal/store"
)

// Authenticator authenticates a request and returns the matched principal.
type Authenticator interface {
	Authenticate(*http.Request) (Principal, bool, error)
}

// WorkerAuthenticator authenticates worker runtime requests from bearer tokens.
type WorkerAuthenticator struct {
	Store *store.Store
}

func (a WorkerAuthenticator) Authenticate(r *http.Request) (Principal, bool, error) {
	if !isWorkerRuntimePath(r.URL.Path) {
		return Principal{}, false, nil
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		return Principal{}, false, errors.New("worker auth token required")
	}
	h := sha256.Sum256([]byte(token))
	workerID, err := a.Store.AuthenticateWorkerAuthToken(r.Context(), h[:])
	if err != nil {
		return Principal{}, false, errors.New("invalid worker auth token")
	}
	return Principal{Type: PrincipalTypeWorker, WorkerID: workerID}, true, nil
}

// DefaultUserAuthenticator authenticates every request as the configured user.
type DefaultUserAuthenticator struct {
	UserID string
}

func (a DefaultUserAuthenticator) Authenticate(*http.Request) (Principal, bool, error) {
	return Principal{
		Type:   PrincipalTypeUser,
		UserID: a.UserID,
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

func isWorkerRuntimePath(path string) bool {
	return strings.HasPrefix(path, "/api/workers/") && strings.HasSuffix(path, "/status")
}
