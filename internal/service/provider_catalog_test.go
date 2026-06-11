package service_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/obot-platform/disco2/internal/api"
	"github.com/obot-platform/disco2/internal/database"
	"github.com/obot-platform/disco2/internal/model"
	"github.com/obot-platform/disco2/internal/sandbox"
	"github.com/obot-platform/disco2/internal/sandboxauth"
	"github.com/obot-platform/disco2/internal/service"
	"github.com/obot-platform/disco2/internal/store"
)

func TestSandboxReconcilerDelegatesToProvider(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSandboxTestService(t, nil)
	provider := &recordingSandboxProvider{}
	svc.RegisterSandboxProvider("recording", provider)
	if got := svc.DefaultSandboxProviderName(); got != "recording" {
		t.Fatalf("default provider = %q, want recording", got)
	}
	reconciler := svc.NewSandboxReconciler()

	sb, err := svc.CreateSandbox(ctx, service.DefaultProjectID, api.CreateSandboxBody{
		Name:      "sandbox-1",
		SourceURL: stringPtr("https://example.com/repo.git"),
		SourceRef: stringPtr("main"),
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	sb.SecretState = []byte("initial")
	sb.RuntimeState = []byte(`{"existing":true}`)
	if err := reconciler.ReconcileSandboxJob(ctx, sb.ProjectID, sb.ID, "job-start", sb.Generation); err != nil {
		t.Fatalf("reconcile start: %v", err)
	}
	if provider.createCalls != 1 || provider.startCalls != 1 {
		t.Fatalf("create/start calls = %d/%d, want 1/1", provider.createCalls, provider.startCalls)
	}
	if provider.createRef.ProjectID != service.DefaultProjectID || provider.createOptions.WorkspaceSource != "https://example.com/repo.git" || provider.createOptions.WorkspaceRef != "main" {
		t.Fatalf("create ref/options = %#v %#v", provider.createRef, provider.createOptions)
	}
	sb, err = svc.GetSandbox(ctx, service.DefaultProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox after start: %v", err)
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
	if sb.WorkerID == nil || *sb.WorkerID != "worker-1" {
		t.Fatalf("worker id = %v, want worker-1", sb.WorkerID)
	}

	sb, err = svc.StopSandbox(ctx, service.DefaultProjectID, sb.ID, api.StopSandboxBody{})
	if err != nil {
		t.Fatalf("stop sandbox: %v", err)
	}
	if err := reconciler.ReconcileSandboxJob(ctx, sb.ProjectID, sb.ID, "job-stop", sb.Generation); err != nil {
		t.Fatalf("reconcile stop: %v", err)
	}
	if provider.stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", provider.stopCalls)
	}
	sb, err = svc.GetSandbox(ctx, service.DefaultProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox after stop: %v", err)
	}
	if string(sb.SecretState) != "stopped" {
		t.Fatalf("secret state after stop = %q, want stopped", string(sb.SecretState))
	}

	if err := svc.DeleteSandbox(ctx, service.DefaultProjectID, sb.ID); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	sb, err = svc.GetSandbox(ctx, service.DefaultProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox after delete intent: %v", err)
	}
	if err := reconciler.ReconcileSandboxJob(ctx, sb.ProjectID, sb.ID, "job-delete", sb.Generation); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}
	if provider.removeCalls != 1 {
		t.Fatalf("remove calls = %d, want 1", provider.removeCalls)
	}
	sb, err = svc.GetSandbox(ctx, service.DefaultProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox after delete: %v", err)
	}
	if sb.SecretState != nil {
		t.Fatalf("secret state after delete = %q, want nil", string(sb.SecretState))
	}
	if sb.RuntimeState != nil {
		t.Fatalf("runtime state after delete = %q, want nil", string(sb.RuntimeState))
	}
}

func TestSandboxReconcilerInjectsTrustKey(t *testing.T) {
	ctx := context.Background()
	appStore := newProviderCatalogTestStore(t)
	provider := &recordingSandboxProvider{}
	auth := &recordingSandboxAuth{trustKey: "public-key"}
	reconciler := sandbox.NewSandboxReconciler(appStore, sandbox.WithSandboxProvider(provider), sandbox.WithSandboxAuthenticator(auth))
	sb := &model.Sandbox{
		ID:                "sandbox-1",
		ProjectID:         service.DefaultProjectID,
		CreatedByUserID:   service.DefaultUserID,
		Name:              "sandbox-1",
		ResourceLifecycle: model.NewResourceLifecycle(model.SandboxCreateOperation, nil),
	}
	sb.IncrementGeneration()
	if err := appStore.CreateSandbox(ctx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if err := reconciler.ReconcileSandboxJob(ctx, sb.ProjectID, sb.ID, "job-1", sb.Generation); err != nil {
		t.Fatalf("reconcile start: %v", err)
	}
	if auth.userID != service.DefaultUserID {
		t.Fatalf("auth user id = %q, want %q", auth.userID, service.DefaultUserID)
	}
	if auth.projectID != service.DefaultProjectID {
		t.Fatalf("auth project id = %q, want %q", auth.projectID, service.DefaultProjectID)
	}
	if got := provider.createOptions.Env["DISCO2_TRUST_KEY"]; got != "public-key" {
		t.Fatalf("trust key env = %q, want public-key", got)
	}
}

func newProviderCatalogTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.MigrateTenant(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	appStore := store.New(database.StaticResolver{DB: db}, store.WithDefaultTenantID(service.DefaultTenantID))
	project := &model.Project{ID: service.DefaultProjectID, TenantID: service.DefaultTenantID, OwnerUserID: service.DefaultUserID, Name: "Default Project", Slug: "default"}
	if err := db.Write.WithContext(ctx).Create(project).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	return appStore
}

func TestSandboxReconcilerNoProviderKeepsStubBehavior(t *testing.T) {
	ctx := context.Background()
	svc, reconciler := newSandboxTestService(t, nil)
	sb, err := svc.CreateSandbox(ctx, service.DefaultProjectID, api.CreateSandboxBody{Name: "sandbox-1"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if err := reconciler.ReconcileSandboxJob(ctx, sb.ProjectID, sb.ID, "job-start", sb.Generation); err != nil {
		t.Fatalf("reconcile start: %v", err)
	}
	sb, err = svc.GetSandbox(ctx, service.DefaultProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
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
	hasDigitalOcean := false
	hasDockerVM := false
	for _, item := range catalog {
		switch item.ID {
		case "digitalocean":
			hasDigitalOcean = true
		case "dockervm":
			hasDockerVM = true
		}
	}
	if !hasDigitalOcean || !hasDockerVM {
		t.Fatalf("catalog = %#v, want digitalocean and dockervm", catalog)
	}
}

func TestServiceResolvesDigitalOceanProviderInstance(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSandboxTestService(t, nil)
	t.Setenv("TEST_DIGITALOCEAN_TOKEN", "token-1")

	provider, err := svc.SandboxProviderManager().ResolveInstance(ctx, &model.SandboxProviderInstance{
		ID:   "provider-1",
		Type: "digitalocean",
		Config: []byte(`{
			"tokenEnv": "TEST_DIGITALOCEAN_TOKEN",
			"controlPlaneUrl": "https://control.example",
			"region": "sfo3",
			"size": "s-1vcpu-1gb",
			"image": "ubuntu-24-04-x64",
			"tags": "sandbox,ci"
		}`),
	})
	if err != nil {
		t.Fatalf("resolve digitalocean provider: %v", err)
	}
	definitionProvider, ok := provider.(sandbox.DefinitionProvider)
	if !ok {
		t.Fatalf("provider does not expose definition")
	}
	if got := definitionProvider.Definition().Name; got != "DigitalOcean" {
		t.Fatalf("definition name = %q", got)
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

func (a *recordingSandboxAuth) CreateToken(context.Context, sandboxauth.TokenClaims) (string, error) {
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
		Metadata:  map[string]string{"worker_id": "worker-1"},
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
