package poolruntime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	poolclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	poolapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	sandboxruntime "github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

func poolStatusError(status int, errorType string) error {
	response := poolapimodel.ErrorModel{
		Status: poolclient.NewOptInt64(int64(status)),
		Title:  poolclient.NewOptString(http.StatusText(status)),
	}
	if errorType != "" {
		parsed, err := url.Parse(errorType)
		if err != nil {
			panic(err)
		}
		response.Type = poolclient.NewOptURI(*parsed)
	}
	return &poolclient.ErrorModelStatusCode{StatusCode: status, Response: response}
}

// A conflict is several different conditions on this API, and only the type tells
// them apart. Reporting archived as already-exists let the reconciler swallow a
// create the pool agent had refused, settling a sandbox with no container as
// converged and `ready`.
func TestMapPoolClientErrorSeparatesTheTwoConflicts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		errorType string
		want      error
	}{
		{"archived carries its type", http.StatusConflict, poolapimodel.ErrorTypeSandboxArchived, sandbox.ErrArchived},
		{"no container carries its type", http.StatusConflict, poolapimodel.ErrorTypeSandboxNoContainer, sandbox.ErrNoContainer},
		{"not running carries its type", http.StatusConflict, poolapimodel.ErrorTypeSandboxNotRunning, sandbox.ErrNotRunning},
		{"a bare conflict is already-exists", http.StatusConflict, "", sandbox.ErrAlreadyExists},
		{"an unrelated type is already-exists", http.StatusConflict, "https://discobox.ai/errors/other", sandbox.ErrAlreadyExists},
		{"not found is unchanged", http.StatusNotFound, "", sandbox.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapPoolClientError(poolStatusError(tc.status, tc.errorType)); !errors.Is(got, tc.want) {
				t.Fatalf("mapped = %v, want %v", got, tc.want)
			}
		})
	}
}

// Archived must not read as already-exists anywhere, because that is the one
// error the create path treats as success.
func TestArchivedIsNotAlreadyExists(t *testing.T) {
	if errors.Is(sandbox.ErrArchived, sandbox.ErrAlreadyExists) {
		t.Fatal("ErrArchived matches ErrAlreadyExists; the create path would swallow a refused create")
	}
	if errors.Is(sandbox.ErrNoContainer, sandbox.ErrAlreadyExists) {
		t.Fatal("ErrNoContainer matches ErrAlreadyExists; the create path would swallow a refused create")
	}
}

// An image the pool cannot obtain reaches the reconciler as its own error,
// carrying the agent's account of which image, so the failure it records says
// why and names the reason a client offers the upgrade on.
func TestMapPoolClientErrorKeepsImageUnavailable(t *testing.T) {
	statusErr := poolStatusError(http.StatusUnprocessableEntity, poolapimodel.ErrorTypeSandboxImageUnavailable)
	var typed *poolclient.ErrorModelStatusCode
	if errors.As(statusErr, &typed) {
		typed.Response.Detail = poolclient.NewOptString(`sandbox image is not available: "harness:local" is not on this pool`)
	}
	got := mapPoolClientError(statusErr)
	if !errors.Is(got, sandbox.ErrImageUnavailable) {
		t.Fatalf("mapped = %v, want ErrImageUnavailable", got)
	}
	if got.Error() != `sandbox image is not available: "harness:local" is not on this pool` {
		t.Fatalf("message = %q, want the agent's detail", got.Error())
	}
	if errors.Is(mapPoolClientError(poolStatusError(http.StatusUnprocessableEntity, "")), sandbox.ErrImageUnavailable) {
		t.Fatal("an untyped 422 was read as an unavailable image")
	}
}

// A pool agent older than the control plane answers an operation added since
// with its router's plain-text 404. That is a statement about the agent, and
// it must not read as the sandbox or pool not being found.
func TestPoolAgentWithoutTheRouteIsUnsupported(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(old.Close)
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	runtimeProvider.baseURL = old.URL
	runtimeProvider.client = old.Client()
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)

	_, err := provider.ClearCache(context.Background(), activePool("pool-1"))
	if !errors.Is(err, sandbox.ErrPoolAgentUnsupported) {
		t.Fatalf("clear cache against an agent without the route = %v, want ErrPoolAgentUnsupported", err)
	}
	if errors.Is(err, sandbox.ErrNotFound) {
		t.Fatal("an agent without the route read as a missing sandbox")
	}
}

// The ensure an action makes before it is forwarded (ADR 26-10-12-711 §2)
// starts a stopped sandbox on the pool agent, and its two refusals reach the
// service as what it acts on: a pool that holds no such sandbox is not found,
// and an agent from before the operation is unsupported, which the service
// reads as "that agent starts sandboxes itself" rather than as a failure.
func TestEnsureRunningStartsTheSandboxAndMapsItsRefusals(t *testing.T) {
	ctx := context.Background()
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)
	ref := sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}
	state := poolRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Metadata: map[string]string{"pool_id": "pool-1"}})

	if err := provider.EnsureRunning(ctx, ref, state); !errors.Is(err, sandbox.ErrNotFound) || errors.Is(err, sandbox.ErrPoolAgentUnsupported) {
		t.Fatalf("ensure of a sandbox the pool does not hold = %v, want ErrNotFound", err)
	}

	if _, err := runtimeProvider.runtime.CreateSandbox(ctx, &poolapimodel.PoolSandboxCreateRequest{SandboxId: "sandbox-1"}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if err := runtimeProvider.runtime.StopSandbox(ctx, "sandbox-1", &poolapimodel.PoolSandboxOperationRequest{}); err != nil {
		t.Fatalf("stop sandbox: %v", err)
	}
	if err := provider.EnsureRunning(ctx, ref, state); err != nil {
		t.Fatalf("ensure of a stopped sandbox: %v", err)
	}
	if sb, err := runtimeProvider.runtime.GetSandbox(ctx, "sandbox-1"); err != nil || sb.Status != sandboxruntime.StatusRunning {
		t.Fatalf("after the ensure the sandbox is %v, %v; want it running", sb, err)
	}

	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(old.Close)
	runtimeProvider.baseURL = old.URL
	runtimeProvider.client = old.Client()
	if err := provider.EnsureRunning(ctx, ref, state); !errors.Is(err, sandbox.ErrPoolAgentUnsupported) || errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("ensure against an agent without the route = %v, want ErrPoolAgentUnsupported and not a missing sandbox", err)
	}
}
