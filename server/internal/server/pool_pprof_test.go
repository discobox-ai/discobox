package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// pprofPools answers OpenPoolPprof and records what it was asked; every other
// PoolService method is the nil embedded interface.
type pprofPools struct {
	services.PoolService
	asked  *[]string
	closed *bool
	err    error
}

func (p pprofPools) OpenPoolPprof(_ context.Context, projectID, poolID, process, profile string, seconds int) (*sandbox.PoolProfile, error) {
	*p.asked = []string{projectID, poolID, process, profile, strings.Repeat("s", seconds)}
	if p.err != nil {
		return nil, p.err
	}
	return &sandbox.PoolProfile{
		Body:        closeRecorder{Reader: strings.NewReader("profile bytes"), closed: p.closed},
		ContentType: "application/octet-stream",
	}, nil
}

func TestPoolPprofRouteStreamsTheProfile(t *testing.T) {
	var asked []string
	var closed bool
	router := chi.NewRouter()
	registerPoolPprofRoutes(router, pprofPools{asked: &asked, closed: &closed})

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/projects/project-1/pools/pool-a/pprof/proxy/profile?seconds=3", nil))
	if resp.Code != http.StatusOK || resp.Body.String() != "profile bytes" {
		t.Fatalf("status = %d, body = %q", resp.Code, resp.Body.String())
	}
	if got := strings.Join(asked, " "); got != "project-1 pool-a proxy profile sss" {
		t.Fatalf("asked %q", got)
	}
	if resp.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(resp.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("headers = %v, want a profile that is never rendered as a page", resp.Header())
	}
	if !closed {
		t.Fatal("the profile body, which holds a pool-agent lease, was not closed")
	}

	for _, path := range []string{
		"/projects/project-1/pools/pool-a/pprof/buildkitd/heap",
		"/projects/project-1/pools/pool-a/pprof/agent/profile?seconds=0",
		"/projects/project-1/pools/pool-a/pprof/agent/profile?seconds=soon",
	} {
		asked = nil
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
		if resp.Code != http.StatusBadRequest || asked != nil {
			t.Fatalf("GET %s = %d (asked %v), want 400 before the pool is asked", path, resp.Code, asked)
		}
	}
}

// A pool with profiling off, or an agent's refusal, keeps its status and its
// words.
func TestPoolPprofRouteKeepsTheRefusal(t *testing.T) {
	var asked []string
	router := chi.NewRouter()
	registerPoolPprofRoutes(router, pprofPools{asked: &asked, err: apperrors.NewStatusError(http.StatusNotFound, "profiling is off for pool pool-a")})
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/projects/project-1/pools/pool-a/pprof/agent/heap", nil))
	if resp.Code != http.StatusNotFound || !strings.Contains(resp.Body.String(), "profiling is off for pool pool-a") {
		t.Fatalf("status = %d, body = %q; want the refusal", resp.Code, resp.Body.String())
	}
}
