package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// Emptying the sandbox's caches is the machine pool's to ask (ADR
// 26-10-23-551 §3): signed with its own key, under a scope of its own that no
// wildcard grants and that reaches nothing else.
func TestCacheClearIsThePoolsAlone(t *testing.T) {
	cleared := 0
	router, token, poolToken := runtimeConfigRouterWithPoolConfig(t, func(cfg *Config) {
		cfg.ClearCaches = func() ([]string, error) {
			cleared++
			return []string{"/home/user/.cache/go-build"}, nil
		}
	})
	serve := func(tok string, path ...string) *httptest.ResponseRecorder {
		target := "/api/projects/project-1/sandboxes/sandbox-1/cache/clear"
		if len(path) > 0 {
			target = path[0]
		}
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, target, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		return resp
	}
	resp := serve(poolToken("worker-1", ScopeCacheClear))
	if resp.Code != http.StatusOK {
		t.Fatalf("clear with the pool's token = %d %s, want 200", resp.Code, resp.Body.String())
	}
	var body struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(body.Paths, []string{"/home/user/.cache/go-build"}) || cleared != 1 {
		t.Fatalf("cleared %d times, answered %v; want once, naming the cache", cleared, body.Paths)
	}
	for name, tok := range map[string]string{
		"a control-plane wildcard":        token("*"),
		"the pool's runtime-config scope": poolToken("worker-1", ScopeRuntimeConfig),
		"the pool's autostop scope":       poolToken("worker-1", ScopeAutostopConfirm),
		"another pool":                    poolToken("worker-2", ScopeCacheClear),
	} {
		if resp := serve(tok); resp.Code != http.StatusForbidden && resp.Code != http.StatusUnauthorized {
			t.Errorf("clear with %s = %d, want refused", name, resp.Code)
		}
	}
	// The generated router decodes an escaped letter, so the route is told
	// the way it reads it: an escaped spelling needs the same scope
	// (sandbox-agent/REVIEW.md, private harness capture routes).
	for _, path := range []string{
		"/api/projects/project-1/sandboxes/sandbox-1/cache/%63lear",
		"/api/projects/project-1/sandboxes/sandbox-1/%63ache/clear",
	} {
		if resp := serve(token("*"), path); resp.Code != http.StatusForbidden && resp.Code != http.StatusUnauthorized {
			t.Errorf("clear at %s with a wildcard = %d, want refused", path, resp.Code)
		}
	}
	if cleared != 1 {
		t.Fatalf("cleared %d times, want only the pool's request to clear", cleared)
	}
	if resp := serveRuntimeConfig(t, router, http.MethodPut, poolToken("worker-1", ScopeCacheClear), plainRuntimeConfig(1, "s")); resp.Code != http.StatusForbidden && resp.Code != http.StatusUnauthorized {
		t.Fatalf("runtime config with the cache-clear scope = %d, want refused", resp.Code)
	}
}
