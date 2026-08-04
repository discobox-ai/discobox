package sandbox_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
)

func TestProviderManagerRegisterStatusAndDefinitions(t *testing.T) {
	manager := sandbox.NewProviderManager()
	provider := &fakeProvider{
		image:  "fake:latest",
		status: sandbox.ProviderStatus{Available: true, State: "ready", Message: "ok"},
		definition: sandbox.ProviderDefinition{
			Name:        "Fake",
			Description: "Fake provider",
		},
	}
	manager.RegisterProvider("fake", provider)
	manager.SetDefault("fake")

	if got := manager.DefaultProviderName(); got != "fake" {
		t.Fatalf("default provider = %q, want fake", got)
	}
	if !manager.EnsureDefaultAvailable() {
		t.Fatal("expected default to be available")
	}
	if names := manager.ListProviders(); len(names) != 1 || names[0] != "fake" {
		t.Fatalf("providers = %#v, want [fake]", names)
	}
	status, ok := manager.GetProviderStatus("fake")
	if !ok {
		t.Fatal("expected fake status")
	}
	if !status.Available || status.State != "ready" {
		t.Fatalf("status = %#v", status)
	}
	definition, ok := manager.GetProviderDefinition("fake")
	if !ok {
		t.Fatal("expected fake definition")
	}
	if definition.Name != "Fake" {
		t.Fatalf("definition name = %q, want Fake", definition.Name)
	}
}

func TestProviderManagerResolveForSandbox(t *testing.T) {
	ctx := context.Background()
	manager := sandbox.NewProviderManager()
	defaultProvider := &fakeProvider{image: "default:latest"}
	instanceProvider := &fakeProvider{image: "instance:latest"}
	manager.RegisterProvider("default", defaultProvider)
	manager.RegisterProvider("custom", instanceProvider)
	manager.SetDefault("default")

	// Provider resolution is sandbox → pool → provider instance: a sandbox
	// without its pool attached cannot resolve.
	if _, err := manager.ResolveForSandbox(ctx, &model.Sandbox{ID: "s1"}); err == nil {
		t.Fatal("expected error resolving a sandbox without a pool")
	}

	_ = defaultProvider
	pool := &model.Pool{
		ID:               "pool-1",
		ProviderInstance: &model.SandboxProviderInstance{ID: "inst-1", Type: "custom"},
	}
	provider, err := manager.ResolveForSandbox(ctx, &model.Sandbox{ID: "s1", PoolID: pool.ID, Pool: pool})
	if err != nil {
		t.Fatalf("resolve via pool provider instance: %v", err)
	}
	if provider != instanceProvider {
		t.Fatal("expected custom provider")
	}
}

func TestProviderManagerFactoryCachesProviderInstances(t *testing.T) {
	ctx := context.Background()
	manager := sandbox.NewProviderManager()
	calls := 0
	manager.RegisterFactory("remote", func(context.Context, *model.SandboxProviderInstance) (sandbox.Provider, error) {
		calls++
		return &fakeProvider{image: "remote:latest"}, nil
	})
	instance := &model.SandboxProviderInstance{
		ID:        "provider-1",
		Type:      "remote",
		UpdatedAt: time.Now().UTC(),
	}

	first, err := manager.ResolveInstance(ctx, instance)
	if err != nil {
		t.Fatalf("resolve first: %v", err)
	}
	second, err := manager.ResolveInstance(ctx, instance)
	if err != nil {
		t.Fatalf("resolve second: %v", err)
	}
	if first != second {
		t.Fatal("expected cached provider instance")
	}
	if calls != 1 {
		t.Fatalf("factory calls = %d, want 1", calls)
	}
}

func TestProviderManagerAggregatesProviderOperations(t *testing.T) {
	ctx := context.Background()
	manager := sandbox.NewProviderManager()
	first := &fakeProvider{
		image: "first:latest",
		sandboxes: []*sandbox.Sandbox{
			{ID: "runtime-1", SandboxID: "sandbox-1", Status: sandbox.StatusRunning},
		},
	}
	second := &fakeProvider{
		image:        "second:latest",
		listErr:      errors.New("list failed"),
		reconcileErr: errors.New("reconcile failed"),
		removeErr:    errors.New("remove failed"),
	}
	manager.RegisterProvider("first", first)
	manager.RegisterProvider("second", second)

	sandboxes, err := manager.ListRuntimeSandboxes(ctx)
	if err == nil || !strings.Contains(err.Error(), `sandbox provider "second"`) {
		t.Fatalf("list error = %v, want provider context", err)
	}
	if len(sandboxes) != 1 || sandboxes[0].SandboxID != "sandbox-1" {
		t.Fatalf("sandboxes = %#v", sandboxes)
	}

	err = manager.ReconcileProviders(ctx)
	if err == nil || !strings.Contains(err.Error(), "reconcile failed") {
		t.Fatalf("reconcile error = %v, want provider error", err)
	}
	if first.reconcileCalls != 1 || second.reconcileCalls != 1 {
		t.Fatalf("reconcile calls = %d/%d, want 1/1", first.reconcileCalls, second.reconcileCalls)
	}

	err = manager.RemoveProjectResources(ctx, "project-1")
	if err == nil || !strings.Contains(err.Error(), "remove failed") {
		t.Fatalf("remove error = %v, want provider error", err)
	}
	if first.removeProjectID != "project-1" || second.removeProjectID != "project-1" {
		t.Fatalf("remove project ids = %q/%q, want project-1", first.removeProjectID, second.removeProjectID)
	}
}

type fakeProvider struct {
	image           string
	status          sandbox.ProviderStatus
	definition      sandbox.ProviderDefinition
	sandboxes       []*sandbox.Sandbox
	listErr         error
	reconcileErr    error
	removeErr       error
	reconcileCalls  int
	removeProjectID string
}

func (p *fakeProvider) Close() error {
	return nil
}

func (p *fakeProvider) List(context.Context) ([]*sandbox.Sandbox, error) {
	if p.listErr != nil {
		return nil, p.listErr
	}
	return p.sandboxes, nil
}
func (p *fakeProvider) Initialize(context.Context, *model.SandboxProviderInstance) error {
	return nil
}
func (p *fakeProvider) Reconcile(context.Context) error {
	p.reconcileCalls++
	return p.reconcileErr
}
func (p *fakeProvider) RemoveProject(_ context.Context, projectID string) error {
	p.removeProjectID = projectID
	return p.removeErr
}
func (p *fakeProvider) Create(context.Context, sandbox.SandboxRef, []byte, sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	return nil, nil, nil
}
func (p *fakeProvider) Update(context.Context, sandbox.SandboxRef, []byte, sandbox.UpdateOptions) (*sandbox.Sandbox, []byte, error) {
	return nil, nil, nil
}
func (p *fakeProvider) Start(context.Context, sandbox.SandboxRef, []byte) ([]byte, error) {
	return nil, nil
}
func (p *fakeProvider) Stop(context.Context, sandbox.SandboxRef, []byte, time.Duration) ([]byte, error) {
	return nil, nil
}
func (p *fakeProvider) Restart(context.Context, sandbox.SandboxRef, []byte, time.Duration) ([]byte, error) {
	return nil, nil
}
func (p *fakeProvider) Remove(context.Context, sandbox.SandboxRef, []byte, ...sandbox.RemoveOption) ([]byte, error) {
	return nil, nil
}
func (p *fakeProvider) Get(context.Context, sandbox.SandboxRef, []byte) (*sandbox.Sandbox, error) {
	return nil, nil
}
func (p *fakeProvider) AcquireHTTPClient(context.Context, sandbox.SandboxRef, []byte, []string) (*transport.HTTPClientLease, error) {
	return transport.NewHTTPClientLease(http.DefaultClient, nil), nil
}
func (p *fakeProvider) Status() sandbox.ProviderStatus {
	return p.status
}
func (p *fakeProvider) Definition() sandbox.ProviderDefinition {
	return p.definition
}
