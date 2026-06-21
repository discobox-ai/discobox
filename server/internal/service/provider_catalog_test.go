package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"runtime"
	"testing"
	"time"

	serverapi "github.com/obot-platform/discobox/api/gen"
	"github.com/obot-platform/discobox/orchestration"
	sandboxauth "github.com/obot-platform/discobox/server/internal/auth/sandbox"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/resources/sandboxes"
	"github.com/obot-platform/discobox/server/internal/service"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
	providerdocker "github.com/obot-platform/discobox/server/providers/sandbox/provider/docker"
	dockerdriver "github.com/obot-platform/discobox/server/providers/sandbox/vm/docker"
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

	sourceURL := mustParseURL(t, "https://example.com/repo.git")
	sb, err := svc.CreateSandbox(ctx, service.DefaultProjectID, services.CreateSandboxBody{
		Name:      "sandbox-1",
		SourceUrl: serverapi.NewOptURI(sourceURL),
		SourceRef: serverapi.NewOptString("main"),
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

	sb, err = svc.StopSandbox(ctx, service.DefaultProjectID, sb.ID, services.StopSandboxBody{})
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
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider), sandboxes.WithSandboxAuthenticator(auth))
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
	if got := provider.createOptions.Env["DISCOBOX_TRUST_KEY"]; got != "public-key" {
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
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	appStore := store.New(db.Write, db.Read)
	project := &model.Project{ID: service.DefaultProjectID, OwnerUserID: service.DefaultUserID, Name: "Default Project", Slug: "default"}
	if err := db.Write.WithContext(ctx).Create(project).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	return appStore
}

func TestSandboxReconcilerNoProviderKeepsStubBehavior(t *testing.T) {
	ctx := context.Background()
	svc, reconciler := newSandboxTestService(t, nil)
	sb, err := svc.CreateSandbox(ctx, service.DefaultProjectID, services.CreateSandboxBody{Name: "sandbox-1"})
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
	svc.RegisterSandboxProviderDefinition("planned", sandboxes.ProviderDefinition{Name: "Planned"})

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
	hasDocker := false
	for _, item := range catalog {
		switch item.ID {
		case "digitalocean":
			hasDigitalOcean = true
		case "docker":
			hasDocker = true
		}
	}
	if !hasDigitalOcean || !hasDocker {
		t.Fatalf("catalog = %#v, want digitalocean and docker", catalog)
	}
}

func TestCreateSandboxProviderInstanceAllowsMissingName(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSandboxTestService(t, nil)
	svc.RegisterSandboxProvider("recording", &recordingSandboxProvider{})

	provider, err := svc.CreateSandboxProviderInstance(ctx, service.DefaultProjectID, services.CreateSandboxProviderInstanceBody{
		Type: "recording",
	})
	if err != nil {
		t.Fatalf("create provider without name: %v", err)
	}
	if provider.Name != "" {
		t.Fatalf("provider name = %q, want empty", provider.Name)
	}
}

func TestInitializeDefaultsInstallsDefaultProviderOnce(t *testing.T) {
	ctx := context.Background()
	appStore := newProviderCatalogTestStore(t)
	svc := service.New(appStore, orchestration.QueueConfig{DefaultMaxAttempts: 3}, nil)

	if err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	providers, err := svc.ListSandboxProviderInstances(ctx, service.DefaultProjectID)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("providers len = %d, want 1", len(providers))
	}
	if providers[0].ID != service.DefaultProviderInstanceID || !providers[0].BuiltIn {
		t.Fatalf("provider = %#v, want built-in default id", providers[0])
	}
	if runtime.GOOS == "linux" {
		if providers[0].Type != "docker" || providers[0].Disabled {
			t.Fatalf("linux provider = %#v, want enabled docker", providers[0])
		}
		assertDefaultDockerProviderConfig(t, providers[0].Config, providerdocker.DefaultWorkerImage())
	}
	if _, err := appStore.GetServerState(ctx, "defaults.default_sandbox_provider.installed"); err != nil {
		t.Fatalf("get install state: %v", err)
	}

	if err := svc.DeleteSandboxProviderInstance(ctx, service.DefaultProjectID, service.DefaultProviderInstanceID); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	if err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults after provider delete: %v", err)
	}
	providers, err = svc.ListSandboxProviderInstances(ctx, service.DefaultProjectID)
	if err != nil {
		t.Fatalf("list providers after delete: %v", err)
	}
	if len(providers) != 0 {
		t.Fatalf("providers len after delete = %d, want 0", len(providers))
	}

	if err := appStore.DeleteServerState(ctx, "defaults.default_sandbox_provider.installed"); err != nil {
		t.Fatalf("delete install state: %v", err)
	}
	if err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults after state clear: %v", err)
	}
	providers, err = svc.ListSandboxProviderInstances(ctx, service.DefaultProjectID)
	if err != nil {
		t.Fatalf("list providers after state clear: %v", err)
	}
	if len(providers) != 1 || providers[0].ID != service.DefaultProviderInstanceID {
		t.Fatalf("providers after state clear = %#v, want recreated default", providers)
	}
}

func TestInitializeDefaultsRepairsEmptyBuiltInDockerProviderConfig(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("default docker provider is installed on linux")
	}

	ctx := context.Background()
	appStore := newProviderCatalogTestStore(t)
	svc := service.New(appStore, orchestration.QueueConfig{DefaultMaxAttempts: 3}, nil)

	if err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	provider, err := appStore.GetSandboxProviderInstance(ctx, service.DefaultProjectID, service.DefaultProviderInstanceID)
	if err != nil {
		t.Fatalf("get default provider: %v", err)
	}
	provider.Config = nil
	if err := appStore.UpdateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("clear default provider config: %v", err)
	}

	if err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults after config clear: %v", err)
	}
	provider, err = appStore.GetSandboxProviderInstance(ctx, service.DefaultProjectID, service.DefaultProviderInstanceID)
	if err != nil {
		t.Fatalf("get repaired default provider: %v", err)
	}
	assertDefaultDockerProviderConfig(t, provider.Config, providerdocker.DefaultWorkerImage())
}

func TestInitializeDefaultsRepairsBuiltInDockerProviderConfigImageFromEnv(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("default docker provider is installed on linux")
	}
	t.Setenv("DISCOBOX_DOCKER_WORKER_IMAGE", "discobox-worker-agent:test")

	ctx := context.Background()
	appStore := newProviderCatalogTestStore(t)
	svc := service.New(appStore, orchestration.QueueConfig{DefaultMaxAttempts: 3}, nil)

	if err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	provider, err := appStore.GetSandboxProviderInstance(ctx, service.DefaultProjectID, service.DefaultProviderInstanceID)
	if err != nil {
		t.Fatalf("get default provider: %v", err)
	}
	assertDefaultDockerProviderConfig(t, provider.Config, "discobox-worker-agent:test")

	provider.Config = []byte(`{"image":"ghcr.io/obot-platform/discobox-systemd:latest","agentPort":3002,"systemd":true,"minWorkers":1,"minHealthyWorkers":1}`)
	if err := appStore.UpdateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("reset default provider config: %v", err)
	}
	if err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults after static image reset: %v", err)
	}
	provider, err = appStore.GetSandboxProviderInstance(ctx, service.DefaultProjectID, service.DefaultProviderInstanceID)
	if err != nil {
		t.Fatalf("get repaired default provider: %v", err)
	}
	assertDefaultDockerProviderConfig(t, provider.Config, "discobox-worker-agent:test")
}

func assertDefaultDockerProviderConfig(t *testing.T, data []byte, expectedImage string) {
	t.Helper()
	var cfg struct {
		Image             string `json:"image"`
		AgentPort         int    `json:"agentPort"`
		Systemd           bool   `json:"systemd"`
		MinWorkers        int    `json:"minWorkers"`
		MaxWorkers        int    `json:"maxWorkers"`
		MinHealthyWorkers int    `json:"minHealthyWorkers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode provider config %s: %v", data, err)
	}
	if cfg.Image != expectedImage {
		t.Fatalf("config image = %q, want %q", cfg.Image, expectedImage)
	}
	if cfg.AgentPort != dockerdriver.DefaultAgentPort() {
		t.Fatalf("config agentPort = %d, want %d", cfg.AgentPort, dockerdriver.DefaultAgentPort())
	}
	if !cfg.Systemd || cfg.MinWorkers != 1 || cfg.MaxWorkers != 1 || cfg.MinHealthyWorkers != 1 {
		t.Fatalf("provider config = %+v, want systemd and worker pool defaults", cfg)
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
	definitionProvider, ok := provider.(sandboxes.DefinitionProvider)
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
	createRef     sandboxes.SandboxRef
	createOptions sandboxes.CreateOptions
}

func (p *recordingSandboxProvider) List(context.Context) ([]*sandboxes.Sandbox, error) {
	return nil, nil
}
func (p *recordingSandboxProvider) Watch(context.Context) (<-chan sandboxes.StateEvent, error) {
	ch := make(chan sandboxes.StateEvent)
	close(ch)
	return ch, nil
}
func (p *recordingSandboxProvider) Reconcile(context.Context) error             { return nil }
func (p *recordingSandboxProvider) RemoveProject(context.Context, string) error { return nil }
func (p *recordingSandboxProvider) PrepareState(context.Context, sandboxes.SandboxRef, sandboxes.CreateOptions) ([]byte, error) {
	return nil, nil
}
func (p *recordingSandboxProvider) Create(_ context.Context, ref sandboxes.SandboxRef, _ []byte, opts sandboxes.CreateOptions) (*sandboxes.Sandbox, []byte, error) {
	p.createCalls++
	p.createRef = ref
	p.createOptions = opts
	return &sandboxes.Sandbox{
		ID:        "runtime-" + ref.SandboxID,
		SandboxID: ref.SandboxID,
		Status:    sandboxes.StatusCreated,
		Image:     "recording:latest",
		CreatedAt: time.Now().UTC(),
		Metadata:  map[string]string{"worker_id": "worker-1"},
	}, []byte("created"), nil
}
func (p *recordingSandboxProvider) Start(context.Context, sandboxes.SandboxRef, []byte) (*sandboxes.Sandbox, []byte, error) {
	p.startCalls++
	return nil, []byte("started"), nil
}
func (p *recordingSandboxProvider) Stop(context.Context, sandboxes.SandboxRef, []byte, time.Duration) (*sandboxes.Sandbox, []byte, error) {
	p.stopCalls++
	return nil, []byte("stopped"), nil
}
func (p *recordingSandboxProvider) Remove(context.Context, sandboxes.SandboxRef, []byte, ...sandboxes.RemoveOption) ([]byte, error) {
	p.removeCalls++
	return nil, nil
}
func (p *recordingSandboxProvider) Get(context.Context, sandboxes.SandboxRef, []byte) (*sandboxes.Sandbox, error) {
	return nil, nil
}
func (p *recordingSandboxProvider) AcquireHTTPClient(context.Context, sandboxes.SandboxRef, []byte) (*sandboxes.HTTPClientLease, error) {
	return sandboxes.NewHTTPClientLease(http.DefaultClient, nil), nil
}

func mustParseURL(t *testing.T, value string) url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatalf("parse url %q: %v", value, err)
	}
	return *parsed
}
