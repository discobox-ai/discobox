package poolruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
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
		{"start authorization carries its type", http.StatusConflict, poolapimodel.ErrorTypeSandboxStartNotAuthorized, sandbox.ErrStartNotAuthorized},
		{"not running carries its type", http.StatusConflict, poolapimodel.ErrorTypeSandboxNotRunning, sandbox.ErrNotRunning},
		{"a bare conflict is already-exists", http.StatusConflict, "", sandbox.ErrAlreadyExists},
		{"an unrelated type is already-exists", http.StatusConflict, "https://discobox.ai/errors/other", sandbox.ErrAlreadyExists},
		{"agent startup is pool unavailability", http.StatusServiceUnavailable, "", sandbox.ErrPoolNotReachable},
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
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

	_, err := provider.ClearCache(context.Background(), activePool("pool-1"))
	if !errors.Is(err, sandbox.ErrPoolAgentUnsupported) {
		t.Fatalf("clear cache against an agent without the route = %v, want ErrPoolAgentUnsupported", err)
	}
	if errors.Is(err, sandbox.ErrNotFound) {
		t.Fatal("an agent without the route read as a missing sandbox")
	}
}

// A pool agent older than the control plane cannot decode a request carrying a
// field added since: every request schema is closed, and its generated server
// answers 400 in its own error body, not a problem. That is the window of
// every upgrade, where the server restarts first and the pool's agent is
// replaced after it (#523), and it is the same statement about the agent as a
// missing route, so a reconcile can wait it out rather than settle a failure.
//
// The agent here is the real one, behind a proxy that adds a field to every
// request body, so the test pins what its generated server actually answers.
func TestPoolAgentThatCannotDecodeTheRequestIsUnsupported(t *testing.T) {
	ctx := context.Background()
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	agent, err := url.Parse(runtimeProvider.baseURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(agent)
	proxy.Transport = runtimeProvider.client.Transport
	newer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			body["fieldFromANewerServer"] = true
			data, _ := json.Marshal(body)
			r.Body = io.NopCloser(bytes.NewReader(data))
			r.ContentLength = int64(len(data))
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(newer.Close)
	runtimeProvider.baseURL = newer.URL
	runtimeProvider.client = newer.Client()
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

	_, _, err = provider.Create(ctx, sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1", PoolID: "pool-1"}, nil, sandbox.CreateOptions{})
	if !errors.Is(err, sandbox.ErrPoolAgentUnsupported) {
		t.Fatalf("create against an agent that does not know a field = %v, want ErrPoolAgentUnsupported", err)
	}
	if !strings.Contains(err.Error(), "fieldFromANewerServer") {
		t.Fatalf("create error = %v, want the agent's account of the field it could not read", err)
	}
}

// The agent's other 400s stay ordinary failures, because a current agent gives
// them too: its own refusals, which are problems, and a value its generated
// validators refuse, which is in the generator's body like an unknown field.
func TestPoolAgentBadRequestOtherThanAnUnknownFieldIsNotUnsupported(t *testing.T) {
	for name, err := range map[string]error{
		"a problem": poolStatusError(http.StatusBadRequest, ""),
		"a value the validators refuse": &unexpectedPoolAgentResponse{StatusCode: http.StatusBadRequest,
			Body: `{"error_message":"operation PoolCreateSandbox: decode request: validate: invalid: sandboxId (string: len 0 less than minimum 1)"}`},
		"a 400 from something in front of the agent": &unexpectedPoolAgentResponse{StatusCode: http.StatusBadRequest, Body: "400 Bad Request: unexpected field"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := mapPoolClientError(err); errors.Is(got, sandbox.ErrPoolAgentUnsupported) {
				t.Fatalf("mapped to %v, want an ordinary failure", got)
			}
		})
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
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
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

func TestExplicitStartWaitsForOldAgentRouteWithoutSendingUnfenced(t *testing.T) {
	ctx := context.Background()
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	epoch := int64(8)
	ref := sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1", StartEpoch: &epoch}
	state := poolRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Metadata: map[string]string{"pool_id": "pool-1"}})
	var starting atomic.Bool
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if starting.Load() {
			http.Error(w, "the pool agent is starting", http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/start") {
			t.Error("sent an unfenced start")
		}
		http.NotFound(w, r)
	}))
	defer old.Close()
	newURL, newClient := runtimeProvider.baseURL, runtimeProvider.client
	runtimeProvider.baseURL, runtimeProvider.client = old.URL, old.Client()
	if _, err := provider.Start(ctx, ref, state); !errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("old agent: %v", err)
	}
	starting.Store(true)
	if _, err := provider.Start(ctx, ref, state); !errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("starting agent: %v", err)
	}
	runtimeProvider.baseURL, runtimeProvider.client = newURL, newClient
	if _, err := runtimeProvider.runtime.CreateSandbox(ctx, &poolapimodel.PoolSandboxCreateRequest{SandboxId: ref.SandboxID}); err != nil {
		t.Fatal(err)
	}
	runtimeProvider.runtime.AuthorizeStart = func(context.Context, string, int64) error { return nil }
	if _, err := provider.Start(ctx, ref, state); err != nil {
		t.Fatalf("replacement agent: %v", err)
	}
	runtimeProvider.runtime.AuthorizeStart = func(context.Context, string, int64) error { return errors.New("superseded") }
	if _, err := provider.Start(ctx, ref, state); !errors.Is(err, sandbox.ErrStartNotAuthorized) {
		t.Fatalf("execution refusal: %v", err)
	}
}

func TestPlainAgentStartupRefusalIsPoolUnavailability(t *testing.T) {
	err := mapPoolClientError(&unexpectedPoolAgentResponse{StatusCode: http.StatusServiceUnavailable, Body: "the pool agent is starting"})
	if !errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatal(err)
	}
}
