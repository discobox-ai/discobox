package poolruntime

import (
	"context"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	poolagentserver "github.com/discobox-ai/discobox/pool-agent/server"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// newClearCacheRuntimeProvider serves a pool agent whose token carries exactly
// the given scopes, so a test can tell the operation's scope from any other.
func newClearCacheRuntimeProvider(t *testing.T, scopes ...string) *testRuntimeProvider {
	t.Helper()
	runtime := sandboxruntime.NewMemorySandboxRuntime()
	controlPlaneKey, poolToken := newPoolAgentTestAuth(t, "project-1", "pool-1", scopes...)
	router, _ := poolagentserver.NewRouter(poolagentserver.Config{
		Identity:              poolagentserver.Identity{ProjectID: "project-1", PoolID: "pool-1"},
		Runtime:               runtime,
		ControlPlanePublicKey: controlPlaneKey,
	})
	agent := httptest.NewServer(router)
	t.Cleanup(agent.Close)
	return &testRuntimeProvider{baseURL: agent.URL, client: agent.Client(), token: poolToken, runtime: runtime}
}

func TestPoolProviderClearCacheStopsRunningSandboxesThroughTheAgent(t *testing.T) {
	runtimeProvider := newClearCacheRuntimeProvider(t, poolagentserver.ScopePoolCacheClear)
	for _, id := range []string{"sandbox-2", "sandbox-1"} {
		if _, err := runtimeProvider.runtime.CreateSandbox(context.Background(), &workerapimodel.PoolSandboxCreateRequest{SandboxId: id}); err != nil {
			t.Fatal(err)
		}
	}
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)

	stopped, err := provider.ClearCache(context.Background(), activePool("pool-1"))
	if err != nil {
		t.Fatalf("clear cache: %v", err)
	}
	if want := []string{"sandbox-1", "sandbox-2"}; !reflect.DeepEqual(stopped, want) {
		t.Fatalf("stopped = %v, want %v", stopped, want)
	}
	for _, id := range stopped {
		sb, err := runtimeProvider.runtime.GetSandbox(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if sb.Status != sandboxruntime.StatusStopped {
			t.Fatalf("%s is %s after the clear", id, sb.Status)
		}
	}
}

// Stopping a whole pool is not a sandbox write: a token scoped to operate on
// sandboxes must not be able to do it.
func TestPoolAgentClearCacheRequiresItsOwnScope(t *testing.T) {
	runtimeProvider := newClearCacheRuntimeProvider(t, poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite)
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)

	_, err := provider.ClearCache(context.Background(), activePool("pool-1"))
	if err == nil || !strings.Contains(err.Error(), "403") && !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("clear cache with sandbox scopes = %v, want forbidden", err)
	}
}

func TestPoolAgentClientClearCacheMintsTheCacheClearScope(t *testing.T) {
	runtimeProvider := newClearCacheRuntimeProvider(t, poolagentserver.ScopePoolCacheClear)
	runtimeProvider.staticToken = true
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)

	_, _ = provider.ClearCache(context.Background(), activePool("pool-1"))
	if len(manager.agentTokenClaims) == 0 {
		t.Fatal("clear cache minted no pool-agent token")
	}
	claims := manager.agentTokenClaims[0]
	if claims.ProjectID != "project-1" || claims.PoolID != "pool-1" || claims.SandboxID != "" || !reflect.DeepEqual(claims.Scopes, []string{poolagentauth.ScopePoolCacheClear}) {
		t.Fatalf("agent token claims = %#v", claims)
	}
}
