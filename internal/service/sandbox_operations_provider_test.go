package service_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/obot-platform/disco2/internal/model"
	"github.com/obot-platform/disco2/internal/sandbox"
	"github.com/obot-platform/disco2/internal/service"
)

func TestSandboxOperationsDelegatesToProvider(t *testing.T) {
	ctx := context.Background()
	provider := &recordingSandboxProvider{}
	operations := service.NewSandboxOperations(service.WithSandboxProvider(provider))
	sb := &model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       "project-1",
		CreatedByUserID: "user-1",
		SourceURL:       stringPtr("https://example.com/repo.git"),
		SourceRef:       stringPtr("main"),
		SecretState:     []byte("initial"),
		RuntimeState:    []byte(`{"existing":true}`),
	}

	if err := operations.Start(ctx, sb); err != nil {
		t.Fatalf("start: %v", err)
	}
	if provider.createCalls != 1 || provider.startCalls != 1 {
		t.Fatalf("create/start calls = %d/%d, want 1/1", provider.createCalls, provider.startCalls)
	}
	if provider.createRef.ProjectID != "project-1" || provider.createOptions.WorkspaceSource != "https://example.com/repo.git" || provider.createOptions.WorkspaceRef != "main" {
		t.Fatalf("create ref/options = %#v %#v", provider.createRef, provider.createOptions)
	}
	if string(sb.SecretState) != "started" {
		t.Fatalf("secret state after start = %q, want started", string(sb.SecretState))
	}
	if len(sb.RuntimeState) == 0 {
		t.Fatal("expected runtime state to be set from provider sandbox")
	}
	if sb.LastActiveAt == nil {
		t.Fatal("expected last active time")
	}

	if err := operations.Stop(ctx, sb); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if provider.stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", provider.stopCalls)
	}
	if string(sb.SecretState) != "stopped" {
		t.Fatalf("secret state after stop = %q, want stopped", string(sb.SecretState))
	}

	if err := operations.Delete(ctx, sb); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if provider.removeCalls != 1 {
		t.Fatalf("remove calls = %d, want 1", provider.removeCalls)
	}
	if sb.SecretState != nil {
		t.Fatalf("secret state after delete = %q, want nil", string(sb.SecretState))
	}
	if sb.RuntimeState != nil {
		t.Fatalf("runtime state after delete = %q, want nil", string(sb.RuntimeState))
	}
}

func TestSandboxOperationsInjectsTrustKey(t *testing.T) {
	ctx := context.Background()
	provider := &recordingSandboxProvider{}
	auth := &recordingSandboxAuth{trustKey: "public-key"}
	operations := service.NewSandboxOperations(
		service.WithSandboxProvider(provider),
		service.WithSandboxAuthenticator(auth),
	)
	sb := &model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       "project-1",
		CreatedByUserID: "user-1",
	}

	if err := operations.Start(ctx, sb); err != nil {
		t.Fatalf("start: %v", err)
	}
	if auth.userID != "user-1" {
		t.Fatalf("auth user id = %q, want user-1", auth.userID)
	}
	if auth.projectID != "project-1" {
		t.Fatalf("auth project id = %q, want project-1", auth.projectID)
	}
	if got := provider.createOptions.Env["DISCO2_TRUST_KEY"]; got != "public-key" {
		t.Fatalf("trust key env = %q, want public-key", got)
	}
}

func TestSandboxOperationsNoProviderKeepsStubBehavior(t *testing.T) {
	sb := &model.Sandbox{ID: "sandbox-1", ProjectID: "project-1"}
	operations := service.NewSandboxOperations()
	if err := operations.Start(context.Background(), sb); err != nil {
		t.Fatalf("start: %v", err)
	}
	if sb.LastActiveAt == nil {
		t.Fatal("expected last active time")
	}
}

func TestServiceSandboxProviderCatalog(t *testing.T) {
	svc, _ := newSandboxTestService(t, nil)
	provider := &recordingSandboxProvider{}
	svc.RegisterSandboxProvider("recording", provider)
	svc.RegisterSandboxProviderDefinition("planned", sandbox.ProviderDefinition{Name: "Planned"})

	names := svc.ListSandboxProviderNames()
	if len(names) != 1 || names[0] != "recording" {
		t.Fatalf("provider names = %#v, want [recording]", names)
	}
	if got := svc.DefaultSandboxProviderName(); got != "recording" {
		t.Fatalf("default provider = %q, want recording", got)
	}
	statuses := svc.ListSandboxProviderStatuses()
	if _, ok := statuses["recording"]; !ok {
		t.Fatalf("statuses = %#v, want recording", statuses)
	}
	catalog := svc.ListSandboxProviderCatalog()
	if len(catalog) != 2 {
		t.Fatalf("catalog length = %d, want 2", len(catalog))
	}
}

type recordingSandboxAuth struct {
	projectID string
	userID    string
	trustKey  string
}

func (a *recordingSandboxAuth) EnsureTrustKey(_ context.Context, projectID, userID string) (string, error) {
	a.projectID = projectID
	a.userID = userID
	return a.trustKey, nil
}

func (a *recordingSandboxAuth) CreateToken(context.Context, string, string) (string, error) {
	return "", nil
}

type recordingSandboxProvider struct {
	createCalls   int
	startCalls    int
	stopCalls     int
	removeCalls   int
	createRef     sandbox.SandboxRef
	createOptions sandbox.CreateOptions
}

func (p *recordingSandboxProvider) List(context.Context) ([]*sandbox.Sandbox, error) {
	return nil, nil
}
func (p *recordingSandboxProvider) Watch(context.Context) (<-chan sandbox.StateEvent, error) {
	ch := make(chan sandbox.StateEvent)
	close(ch)
	return ch, nil
}
func (p *recordingSandboxProvider) Reconcile(context.Context) error             { return nil }
func (p *recordingSandboxProvider) RemoveProject(context.Context, string) error { return nil }
func (p *recordingSandboxProvider) PrepareState(context.Context, sandbox.SandboxRef, sandbox.CreateOptions) ([]byte, error) {
	return nil, nil
}
func (p *recordingSandboxProvider) Create(_ context.Context, ref sandbox.SandboxRef, _ []byte, opts sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	p.createCalls++
	p.createRef = ref
	p.createOptions = opts
	return &sandbox.Sandbox{
		ID:        "runtime-" + ref.SandboxID,
		SandboxID: ref.SandboxID,
		Status:    sandbox.StatusCreated,
		Image:     "recording:latest",
		CreatedAt: time.Now().UTC(),
	}, []byte("created"), nil
}
func (p *recordingSandboxProvider) Start(context.Context, sandbox.SandboxRef, []byte) (*sandbox.Sandbox, []byte, error) {
	p.startCalls++
	return nil, []byte("started"), nil
}
func (p *recordingSandboxProvider) Stop(context.Context, sandbox.SandboxRef, []byte, time.Duration) (*sandbox.Sandbox, []byte, error) {
	p.stopCalls++
	return nil, []byte("stopped"), nil
}
func (p *recordingSandboxProvider) Remove(context.Context, sandbox.SandboxRef, []byte, ...sandbox.RemoveOption) ([]byte, error) {
	p.removeCalls++
	return nil, nil
}
func (p *recordingSandboxProvider) Get(context.Context, sandbox.SandboxRef, []byte) (*sandbox.Sandbox, error) {
	return nil, nil
}
func (p *recordingSandboxProvider) AcquireHTTPClient(context.Context, sandbox.SandboxRef, []byte) (*sandbox.HTTPClientLease, error) {
	return sandbox.NewHTTPClientLease(http.DefaultClient, nil), nil
}

func stringPtr(value string) *string {
	return &value
}
