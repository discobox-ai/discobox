package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// The worktree route is the sandbox agent's to serve (ADR 0126 §4): the pool
// checks its own token's scope, then forwards the request — path below the
// sandbox, query, and the control plane's sandbox-agent token — and runs no git
// of its own.
func TestWorktreeRouteForwardsToTheSandboxAgent(t *testing.T) {
	projectID, poolID, sandboxID := "project-1", "pool-1", "sandbox-1"
	var reached atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Inside a discobox every new listener is probed once; that is not a
		// forwarded request (REVIEW.md).
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return
		}
		reached.Add(1)
		if !strings.HasPrefix(r.URL.Path, "/api/projects/project-1/sandboxes/sandbox-1/git-repositories/primary.git/") {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sandbox-token" {
			t.Errorf("upstream authorization = %q", got)
		}
		if got := r.Header.Get(sandboxAgentAuthorizationHeader); got != "" {
			t.Errorf("internal auth header leaked upstream: %q", got)
		}
		_, _ = w.Write([]byte(r.URL.RawQuery))
	}))
	t.Cleanup(upstream.Close)
	baseURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               newProxyTestRuntime(t, baseURL),
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	const base = "/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/git-repositories/primary.git"
	for _, tc := range []struct {
		name, method, path, scope string
		downstream                bool
		want                      int
		forwarded                 bool
	}{
		{"fetch advertisement on read", http.MethodGet, "/info/refs?service=git-upload-pack", ScopeSandboxRead, true, http.StatusOK, true},
		{"fetch on read", http.MethodPost, "/git-upload-pack", ScopeSandboxRead, true, http.StatusOK, true},
		{"push advertisement on read", http.MethodGet, "/info/refs?service=git-receive-pack", ScopeSandboxRead, true, http.StatusForbidden, false},
		{"push on read", http.MethodPost, "/git-receive-pack", ScopeSandboxRead, true, http.StatusForbidden, false},
		{"push on write", http.MethodPost, "/git-receive-pack", ScopeSandboxWrite, true, http.StatusOK, true},
		{"fetch on an exec scope", http.MethodGet, "/info/refs?service=git-upload-pack", ScopeExecRead, true, http.StatusForbidden, false},
		{"no sandbox-agent token", http.MethodGet, "/info/refs?service=git-upload-pack", ScopeSandboxRead, false, http.StatusUnauthorized, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := reached.Load()
			req := httptest.NewRequestWithContext(context.Background(), tc.method, base+tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+sign(projectID, poolID, sandboxID, tc.scope))
			if tc.downstream {
				req.Header.Set(sandboxAgentAuthorizationHeader, "Bearer sandbox-token")
			}
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)
			if resp.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", resp.Code, tc.want, resp.Body.String())
			}
			if forwarded := reached.Load() != before; forwarded != tc.forwarded {
				t.Fatalf("forwarded = %v, want %v", forwarded, tc.forwarded)
			}
			if _, query, _ := strings.Cut(tc.path, "?"); tc.forwarded && resp.Body.String() != query {
				t.Fatalf("upstream query = %q, want %q", resp.Body.String(), query)
			}
		})
	}
}

// The worktree is inside the sandbox, so a sandbox that will not start cannot
// hand it over: the route is refused with the start's failure and what to do
// about it — the text git prints to the person fetching — rather than
// answered from the pool's copy of its files.
func TestWorktreeRouteRefusesASandboxThatCannotStart(t *testing.T) {
	bindMount := errors.New("invalid mount config: bind source path does not exist")
	service := &sandboxService{runtime: &ensureRecorder{err: bindMount}}
	router := chi.NewRouter()
	registerSandboxGitRoutes(router, service)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/project/p/pool/w/sandboxes/sbx_1/git-repositories/primary.git/info/refs?service=git-upload-pack", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, bindMount.Error()) || !strings.Contains(body, "discobox admin box repair sbx_1") {
		t.Fatalf("body = %q, want the start failure and the way out of it", body)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("content type = %q: git prints a refusal's body only when it is plain text", got)
	}
}

// A sandbox pinned to an image whose agent predates the route is told to
// upgrade, rather than forwarded to an agent whose router answers a bare 404
// that git reports as a repository that does not exist.
func TestWorktreeRouteTellsAnOldSandboxToUpgrade(t *testing.T) {
	projectID, poolID, sandboxID := "project-1", "pool-1", "sandbox-1"
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return
		}
		t.Errorf("forwarded %s to an agent too old to serve it", r.URL.Path)
	}))
	t.Cleanup(upstream.Close)
	baseURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               oldAgentRuntime{newProxyTestRuntime(t, baseURL)},
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/git-repositories/primary.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer "+sign(projectID, poolID, sandboxID, ScopeSandboxRead))
	req.Header.Set(sandboxAgentAuthorizationHeader, "Bearer sandbox-token")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "discobox admin box upgrade sandbox-1") {
		t.Fatalf("status = %d, body = %q; want 409 naming the upgrade", resp.Code, resp.Body.String())
	}
}

// oldAgentRuntime is a sandbox whose image predates the worktree route.
type oldAgentRuntime struct{ proxyTestRuntime }

func (oldAgentRuntime) SandboxServesWorktree(_ context.Context, sandboxID string) error {
	return fmt.Errorf("%w; run `discobox admin box upgrade %s`, then try again", sandboxruntime.ErrWorktreeUnsupported, sandboxID)
}
