package poolruntime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/pprofserve"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	poolagentserver "github.com/discobox-ai/discobox/pool-agent/server"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// newPprofRuntimeProvider stands up the read path of ADR 26-11-24-418 §3 in
// process: a pool process serving its profiles as pprofserve does, and a real
// pool-agent router relaying to it, accepting a token with exactly scopes.
func newPprofRuntimeProvider(t *testing.T, scopes ...string) *testRuntimeProvider {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "pprof-proxy.token")
	bound, stop, err := pprofserve.Start(context.Background(), pprofserve.Config{Setting: "test", Addr: "127.0.0.1:0", TokenFile: tokenFile})
	if err != nil {
		t.Fatalf("start pprof: %v", err)
	}
	t.Cleanup(stop)
	controlPlaneKey, poolToken := newPoolAgentTestAuth(t, "project-1", "pool-1", scopes...)
	router, err := poolagentserver.NewRouter(poolagentserver.Config{
		Identity:              poolagentserver.Identity{ProjectID: "project-1", PoolID: "pool-1"},
		Runtime:               sandboxruntime.NewMemorySandboxRuntime(),
		ControlPlanePublicKey: controlPlaneKey,
		Pprof:                 map[string]poolagentserver.PprofTarget{"proxy": {Address: bound.String(), TokenFile: tokenFile}},
	})
	if err != nil {
		t.Fatalf("new pool-agent router: %v", err)
	}
	agent := httptest.NewServer(router)
	t.Cleanup(agent.Close)
	return &testRuntimeProvider{baseURL: agent.URL, client: agent.Client(), token: poolToken}
}

func TestPoolProviderFetchesAProfileThroughTheAgent(t *testing.T) {
	runtimeProvider := newPprofRuntimeProvider(t, poolagentauth.ScopePoolPprof)
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

	profiled, err := provider.OpenPprof(context.Background(), activePool("pool-1"), "proxy", "heap", 0)
	if err != nil {
		t.Fatalf("open profile: %v", err)
	}
	body, err := io.ReadAll(profiled.Body)
	_ = profiled.Body.Close()
	if err != nil || len(body) == 0 || profiled.ContentType != "application/octet-stream" {
		t.Fatalf("profile = %d bytes of %q, %v; want a binary profile", len(body), profiled.ContentType, err)
	}

	// The agent's refusal keeps its status and says why.
	_, err = provider.OpenPprof(context.Background(), activePool("pool-1"), "agent", "heap", 0)
	var statusErr apperrors.StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusNotFound || !strings.Contains(err.Error(), "no process") {
		t.Fatalf("profile a process the pool does not serve = %v, want the agent's 404 and its detail", err)
	}
}

// Profiling a pool is not any other read: a token with every other scope is
// refused.
func TestPoolAgentPprofRequiresItsOwnScope(t *testing.T) {
	runtimeProvider := newPprofRuntimeProvider(t, poolagentauth.ScopeSandboxRead, poolagentauth.ScopeSandboxWrite, poolagentauth.ScopeAuditRead, poolagentauth.ScopePoolSync)
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	_, err := provider.OpenPprof(context.Background(), activePool("pool-1"), "proxy", "heap", 0)
	var statusErr apperrors.StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusForbidden {
		t.Fatalf("profile with every other scope = %v, want 403", err)
	}
}

// The control plane mints the relay a token of its one scope, naming no
// sandbox.
func TestPoolAgentClientPprofMintsAPprofToken(t *testing.T) {
	runtimeProvider := newPprofRuntimeProvider(t, poolagentauth.ScopePoolPprof)
	runtimeProvider.staticToken = true
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

	_, _ = provider.OpenPprof(context.Background(), activePool("pool-1"), "proxy", "heap", 0)
	if len(manager.agentTokenClaims) == 0 {
		t.Fatal("profile minted no pool-agent token")
	}
	claims := manager.agentTokenClaims[0]
	if claims.ProjectID != "project-1" || claims.PoolID != "pool-1" || claims.SandboxID != "" || !reflect.DeepEqual(claims.Scopes, []string{poolagentauth.ScopePoolPprof}) {
		t.Fatalf("minted claims = %+v, want pool:pprof alone for the pool", claims)
	}
}

// An agent too old for the relay answers the router's text/plain 404, which
// is the pool being unable to profile, not a missing profile.
func TestPoolAgentWithoutThePprofRelayIsUnsupported(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	t.Cleanup(agent.Close)
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	runtimeProvider.baseURL = agent.URL
	runtimeProvider.client = agent.Client()
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	if _, err := provider.OpenPprof(context.Background(), activePool("pool-1"), "proxy", "heap", 0); !errors.Is(err, sandbox.ErrPoolAgentUnsupported) {
		t.Fatalf("a router 404 = %v, want ErrPoolAgentUnsupported", err)
	}
}

// A pool's bootstrap says whether its processes serve profiles, and says
// nothing for a pool that does not, so its placement is unchanged.
func TestAPoolsBootstrapSaysWhetherItIsProfiled(t *testing.T) {
	project := &model.Project{ID: "project-1"}
	for _, profiling := range []bool{true, false} {
		pool := activePool("pool-1")
		pool.Profiling = profiling
		maker, err := newPoolMaker(&fakePoolManager{pool: pool}, project, pool, false)
		if err != nil {
			t.Fatal(err)
		}
		bootstrap, err := maker.Bootstrap(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if bootstrap.Profiling != profiling {
			t.Fatalf("a pool with profiling %v: bootstrap says %v", profiling, bootstrap.Profiling)
		}
	}
}
