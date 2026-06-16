package middleware

import (
	"net/http"
	"strings"

	"github.com/obot-platform/discobox/server/internal/authctx"
)

// Authenticator authenticates a request and returns the matched principal.
type Authenticator interface {
	Authenticate(*http.Request) (authctx.Principal, bool, error)
}

// WorkerAuthenticator recognizes worker route bearer-authenticated requests.
type WorkerAuthenticator struct{}

func (WorkerAuthenticator) Authenticate(r *http.Request) (authctx.Principal, bool, error) {
	if bearerToken(r.Header.Get("Authorization")) == "" {
		return authctx.Principal{}, false, nil
	}
	if !strings.HasPrefix(r.URL.Path, "/api/workers/") {
		return authctx.Principal{}, false, nil
	}
	return authctx.Principal{Type: authctx.PrincipalTypeWorker}, true, nil
}

// DefaultUserAuthenticator authenticates every request as the configured user.
type DefaultUserAuthenticator struct {
	TenantID string
	UserID   string
}

func (a DefaultUserAuthenticator) Authenticate(*http.Request) (authctx.Principal, bool, error) {
	return authctx.Principal{
		Type:     authctx.PrincipalTypeUser,
		TenantID: a.TenantID,
		UserID:   a.UserID,
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
