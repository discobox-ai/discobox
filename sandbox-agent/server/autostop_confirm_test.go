package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const autostopConfirmPath = "/api/projects/project-1/sandboxes/sandbox-1/autostop/confirm"

func serveAutostopConfirm(t *testing.T, router http.Handler, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, autostopConfirmPath, bytes.NewBufferString(`{"reason":"sleep"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

// Confirming a power-down consumes the sandbox's sleep requests, so it is the
// pool's alone: signed with its own key, under a scope of its own that no
// wildcard grants and that reaches nothing else (ADR 26-10-16-337 §5).
func TestAutostopConfirmIsThePoolsAlone(t *testing.T) {
	router, token, poolToken := runtimeConfigRouterWithPool(t)
	refused := func(code int) bool { return code == http.StatusForbidden || code == http.StatusUnauthorized }
	// This router runs no policy, so an authorized confirmation is refused
	// with 409: no sleep is pending.
	if resp := serveAutostopConfirm(t, router, poolToken("worker-1", ScopeAutostopConfirm)); resp.Code != http.StatusConflict {
		t.Fatalf("confirm with the pool's token = %d %s, want 409", resp.Code, resp.Body.String())
	}
	for name, tok := range map[string]string{
		"a control-plane wildcard":        token("*"),
		"the pool's runtime-config scope": poolToken("worker-1", ScopeRuntimeConfig),
		"another pool":                    poolToken("worker-2", ScopeAutostopConfirm),
		"a second scope":                  poolToken("worker-1", ScopeAutostopConfirm, ScopeRuntimeConfig),
	} {
		if resp := serveAutostopConfirm(t, router, tok); !refused(resp.Code) {
			t.Errorf("confirm with %s = %d, want refused", name, resp.Code)
		}
	}
	if resp := serveRuntimeConfig(t, router, http.MethodPut, poolToken("worker-1", ScopeAutostopConfirm), plainRuntimeConfig(1, "s")); !refused(resp.Code) {
		t.Fatalf("runtime config with the autostop scope = %d, want refused", resp.Code)
	}
}
