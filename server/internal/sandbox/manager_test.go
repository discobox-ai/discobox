package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/sourcecache"

	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

func TestProviderManagerRegisterStatusAndDefinitions(t *testing.T) {
	manager := sandbox.NewProviderManager(sealedFields(nil))
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
	manager := sandbox.NewProviderManager(sealedFields(nil))
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
	manager := sandbox.NewProviderManager(sealedFields(nil))
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

func TestProviderManagerValidatesOnlyResolvableTypes(t *testing.T) {
	manager := sandbox.NewProviderManager(sealedFields(nil))
	manager.RegisterFactory("remote", func(context.Context, *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return &fakeProvider{}, nil
	})
	manager.RegisterProvider("local", &fakeProvider{})
	manager.RegisterProviderDefinition("described", sandbox.ProviderDefinition{Name: "Described"})
	manager.RegisterProviderConfigValidator("remote", func(json.RawMessage) error {
		return errors.New("bad config")
	})

	if err := manager.ValidateProviderConfig("local", nil); err != nil {
		t.Errorf("validate registered provider type: %v", err)
	}
	if err := manager.ValidateProviderConfig("remote", nil); err == nil || err.Error() != "bad config" {
		t.Errorf("validate factory type = %v, want its validator's error", err)
	}
	for _, providerType := range []string{"described", "missing", ""} {
		if err := manager.ValidateProviderConfig(providerType, nil); err == nil {
			t.Errorf("validate %q succeeded, want unknown type", providerType)
		}
	}
}

func TestProviderManagerClosesProviderThatFailsToInitialize(t *testing.T) {
	ctx := context.Background()
	manager := sandbox.NewProviderManager(sealedFields(nil))
	provider := &fakeProvider{initErr: errors.New("host unreachable")}
	manager.RegisterFactory("remote", func(context.Context, *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return provider, nil
	})

	if _, err := manager.ResolveInstance(ctx, &model.SandboxProviderInstance{ID: "provider-1", Type: "remote"}); err == nil {
		t.Fatal("resolve succeeded, want the initialize error")
	}
	if !provider.closed {
		t.Error("provider that failed to initialize was not closed")
	}
}

func TestProviderManagerAggregatesProviderOperations(t *testing.T) {
	ctx := context.Background()
	manager := sandbox.NewProviderManager(sealedFields(nil))
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
	initErr         error
	onInitialize    func(*model.SandboxProviderInstance)
	closed          bool
}

func (p *fakeProvider) Close() error {
	p.closed = true
	return nil
}

func (p *fakeProvider) List(context.Context) ([]*sandbox.Sandbox, error) {
	if p.listErr != nil {
		return nil, p.listErr
	}
	return p.sandboxes, nil
}
func (p *fakeProvider) Initialize(_ context.Context, instance *model.SandboxProviderInstance) error {
	if p.onInitialize != nil {
		p.onInitialize(instance)
	}
	return p.initErr
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

func (p *fakeProvider) PowerDown(_ context.Context, _ sandbox.SandboxRef, state []byte, _ sandbox.PowerDownReason) (sandbox.Wake, []byte, error) {
	return sandbox.Wake{}, state, nil
}

func (p *fakeProvider) Suspend(_ context.Context, _ sandbox.SandboxRef, state []byte) ([]byte, error) {
	return state, nil
}
func (p *fakeProvider) Restart(context.Context, sandbox.SandboxRef, []byte, time.Duration) ([]byte, error) {
	return nil, nil
}
func (p *fakeProvider) Archive(context.Context, sandbox.SandboxRef, []byte) ([]byte, error) {
	return nil, nil
}

func (p *fakeProvider) Remove(context.Context, sandbox.SandboxRef, []byte) ([]byte, error) {
	return nil, nil
}
func (p *fakeProvider) Get(context.Context, sandbox.SandboxRef, []byte) (*sandbox.Sandbox, error) {
	return nil, nil
}
func (p *fakeProvider) EnsureRunning(context.Context, sandbox.SandboxRef, []byte) error {
	return nil
}

func (p *fakeProvider) AcquireHTTPClient(context.Context, sandbox.SandboxRef, []byte, []string, sandbox.Use) (*transport.HTTPClientLease, error) {
	return transport.NewHTTPClientLease(http.DefaultClient, nil), nil
}
func (p *fakeProvider) Status() sandbox.ProviderStatus {
	return p.status
}
func (p *fakeProvider) Definition() sandbox.ProviderDefinition {
	return p.definition
}

func (*fakeProvider) ExportTree(context.Context, sandbox.SandboxRef, string, sandbox.Delivery, []byte) (io.ReadCloser, error) {
	return nil, nil
}

func (*fakeProvider) ImportTree(_ context.Context, _ sandbox.SandboxRef, poolID string, _ platform.Platform, _ io.Reader) (string, error) {
	return poolID, nil
}

func (p *fakeProvider) PullSource(context.Context, sandbox.SandboxRef, sourcecache.PullRequest) (sourcecache.Delivery, error) {
	panic("unexpected source pull")
}

// sealedFields stands in for the store: what it opens for every instance.
type sealedFields map[string]json.RawMessage

func (f sealedFields) OpenSandboxProviderSecrets(context.Context, *model.SandboxProviderInstance) (map[string]json.RawMessage, error) {
	return f, nil
}

func TestProviderManagerJoinsSealedFieldsIntoTheConfigItsProviderDecodes(t *testing.T) {
	ctx := context.Background()
	manager := sandbox.NewProviderManager(sealedFields{"apiKey": json.RawMessage(`"key-1"`)})
	manager.RegisterProviderDefinition("remote", sandbox.ProviderDefinition{ConfigFields: []sandbox.ProviderConfigField{
		{Key: "driver", Type: "string"},
		{Key: "apiKey", Type: sandbox.ProviderConfigFieldSecret},
	}})
	var built, initialized string
	provider := &fakeProvider{}
	manager.RegisterFactory("remote", func(_ context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		built = string(instance.Config)
		provider.onInitialize = func(instance *model.SandboxProviderInstance) { initialized = string(instance.Config) }
		return provider, nil
	})
	stored := &model.SandboxProviderInstance{ID: "provider-1", Type: "remote", Config: model.RawJSON(`{"driver":"boxd"}`)}
	if _, err := manager.ResolveInstance(ctx, stored); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	const want = `{"apiKey":"key-1","driver":"boxd"}`
	if built != want || initialized != want {
		t.Errorf("factory saw %s and Initialize %s, want %s for both", built, initialized, want)
	}
	if string(stored.Config) != `{"driver":"boxd"}` {
		t.Errorf("stored instance's config became %s: the join must not write back to the row", stored.Config)
	}
}

func TestProviderManagerRefusesASecretFieldLeftInTheClear(t *testing.T) {
	ctx := context.Background()
	manager := sandbox.NewProviderManager(sealedFields(nil))
	manager.RegisterProviderDefinition("remote", sandbox.ProviderDefinition{ConfigFields: []sandbox.ProviderConfigField{
		{Key: "apiKey", Type: sandbox.ProviderConfigFieldSecret},
	}})
	built := false
	manager.RegisterFactory("remote", func(context.Context, *model.SandboxProviderInstance) (sandbox.Provider, error) {
		built = true
		return &fakeProvider{}, nil
	})
	_, err := manager.ResolveInstance(ctx, &model.SandboxProviderInstance{ID: "provider-1", Type: "remote", Config: model.RawJSON(`{"apiKey":"key-1"}`)})
	if err == nil || !strings.Contains(err.Error(), "apiKey unsealed") || !strings.Contains(err.Error(), "restart the server") {
		t.Fatalf("resolve = %v, want the unsealed apiKey refused with a restart to seal it", err)
	}
	if built {
		t.Error("the provider was built from a config with its key in the clear")
	}
}
