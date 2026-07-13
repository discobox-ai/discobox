package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"testing"
	"time"

	serverapi "github.com/obot-platform/discobox/api/gen"
	sandboxauth "github.com/obot-platform/discobox/server/internal/auth/sandbox"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/resources/sandboxes"
	"github.com/obot-platform/discobox/server/internal/service"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
	"github.com/obot-platform/discobox/server/internal/transport"
	providerdocker "github.com/obot-platform/discobox/server/providers/docker"
)

// reconcileSandbox mirrors the reconcile engine's entry: load the LATEST
// sandbox state, then converge it.
func reconcileSandbox(ctx context.Context, t *testing.T, svc *service.Service, executor *sandboxes.SandboxReconciler, projectID, sandboxID string) error {
	t.Helper()
	current, err := svc.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		t.Fatalf("get sandbox for reconcile: %v", err)
	}
	return executor.ReconcileSandbox(ctx, current)
}

func TestSandboxReconcileExecutorDelegatesToProvider(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSandboxTestService(t, nil)
	provider := &recordingSandboxProvider{}
	svc.RegisterSandboxProvider("recording", provider)
	if got := svc.DefaultSandboxProviderName(); got != "recording" {
		t.Fatalf("default provider = %q, want recording", got)
	}
	providerInstance, err := svc.CreateSandboxProviderInstance(ctx, service.DefaultProjectID, services.CreateSandboxProviderInstanceBody{
		Type: "recording",
		Name: "recording",
	})
	if err != nil {
		t.Fatalf("create recording provider instance: %v", err)
	}
	executor := svc.NewSandboxReconciler()

	sourceURL := mustParseURL(t, "https://example.com/repo.git")
	sb, err := svc.CreateSandbox(ctx, service.DefaultProjectID, services.CreateSandboxBody{
		ProviderInstanceId: serverapi.NewOptString(providerInstance.ID),
		Config: serverapi.SandboxCreateConfig{
			Name: "sandbox-1",
			Source: serverapi.NewOptGitSource(serverapi.GitSource{
				Kind: serverapi.GitSourceKindGit,
				URL:  serverapi.NewOptURI(sourceURL),
				Checkout: serverapi.NewOptGitSourceCheckout(serverapi.GitSourceCheckout{
					RefName: serverapi.NewOptString("main"),
				}),
			}),
		},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	sb.SecretState = []byte("initial")
	sb.RuntimeState = []byte(`{"existing":true}`)
	if err := reconcileSandbox(ctx, t, svc, executor, sb.ProjectID, sb.ID); err != nil {
		t.Fatalf("reconcile start: %v", err)
	}
	if provider.createCalls != 1 || provider.startCalls != 1 {
		t.Fatalf("create/start calls = %d/%d, want 1/1", provider.createCalls, provider.startCalls)
	}
	if provider.createRef.ProjectID != service.DefaultProjectID || provider.createOptions.Source == nil || provider.createOptions.Source.URL == nil || *provider.createOptions.Source.URL != "https://example.com/repo.git" || provider.createOptions.Source.Checkout == nil || provider.createOptions.Source.Checkout.RefName == nil || *provider.createOptions.Source.Checkout.RefName != "main" {
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
	if err := reconcileSandbox(ctx, t, svc, executor, sb.ProjectID, sb.ID); err != nil {
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
	if err := reconcileSandbox(ctx, t, svc, executor, sb.ProjectID, sb.ID); err != nil {
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

func TestCreateSandboxUsesDefaultSandboxImage(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSandboxTestService(t, nil)
	svc.SetDefaultSandboxImage("discobox-sandbox-agent:default")
	provider := &recordingSandboxProvider{}
	svc.RegisterSandboxProvider("recording", provider)
	providerInstance, err := svc.CreateSandboxProviderInstance(ctx, service.DefaultProjectID, services.CreateSandboxProviderInstanceBody{
		Type: "recording",
		Name: "recording",
	})
	if err != nil {
		t.Fatalf("create recording provider instance: %v", err)
	}
	executor := svc.NewSandboxReconciler()

	sb, err := svc.CreateSandbox(ctx, service.DefaultProjectID, services.CreateSandboxBody{
		ProviderInstanceId: serverapi.NewOptString(providerInstance.ID),
		Config:             serverapi.SandboxCreateConfig{Name: "sandbox-default-image"},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if sb.Image != "discobox-sandbox-agent:default" {
		t.Fatalf("sandbox image = %q, want default image", sb.Image)
	}
	if err := reconcileSandbox(ctx, t, svc, executor, sb.ProjectID, sb.ID); err != nil {
		t.Fatalf("reconcile start: %v", err)
	}
	if provider.createOptions.Image.Name != "discobox-sandbox-agent:default" {
		t.Fatalf("provider image = %q, want default image", provider.createOptions.Image.Name)
	}
}

func TestCreateSandboxExplicitImageOverridesDefault(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSandboxTestService(t, nil)
	svc.SetDefaultSandboxImage("discobox-sandbox-agent:default")
	provider := &recordingSandboxProvider{}
	svc.RegisterSandboxProvider("recording", provider)
	providerInstance, err := svc.CreateSandboxProviderInstance(ctx, service.DefaultProjectID, services.CreateSandboxProviderInstanceBody{
		Type: "recording",
		Name: "recording",
	})
	if err != nil {
		t.Fatalf("create recording provider instance: %v", err)
	}
	executor := svc.NewSandboxReconciler()

	sb, err := svc.CreateSandbox(ctx, service.DefaultProjectID, services.CreateSandboxBody{
		ProviderInstanceId: serverapi.NewOptString(providerInstance.ID),
		Config: serverapi.SandboxCreateConfig{
			Name:  "sandbox-explicit-image",
			Image: serverapi.NewOptString("custom:sandbox"),
		},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if sb.Image != "custom:sandbox" {
		t.Fatalf("sandbox image = %q, want explicit image", sb.Image)
	}
	if err := reconcileSandbox(ctx, t, svc, executor, sb.ProjectID, sb.ID); err != nil {
		t.Fatalf("reconcile start: %v", err)
	}
	if provider.createOptions.Image.Name != "custom:sandbox" {
		t.Fatalf("provider image = %q, want explicit image", provider.createOptions.Image.Name)
	}
}

func TestSandboxReconcileExecutorInjectsTrustKey(t *testing.T) {
	ctx := context.Background()
	appStore := newProviderCatalogTestStore(t)
	provider := &recordingSandboxProvider{}
	auth := &recordingSandboxAuth{trustKey: "public-key"}
	executor := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider), sandboxes.WithSandboxAuthenticator(auth))
	sb := &model.Sandbox{
		ID:                "sandbox-1",
		ProjectID:         service.DefaultProjectID,
		CreatedByUserID:   service.DefaultUserID,
		Name:              "sandbox-1",
		ResourceLifecycle: model.NewResourceLifecycle(model.SandboxCreateOperation),
	}
	sb.IncrementGeneration()
	if err := appStore.CreateSandbox(ctx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if err := executor.ReconcileSandbox(ctx, sb); err != nil {
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
	svc := service.New(appStore, nil, service.JobManagerOptions{})

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
		assertDefaultDockerProviderConfig(t, providers[0].Config, "")
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
	svc := service.New(appStore, nil, service.JobManagerOptions{})

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
	assertDefaultDockerProviderConfig(t, provider.Config, "")
}

func TestInitializeDefaultsDoesNotPersistBuiltInDockerProviderImageFromEnv(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("default docker provider is installed on linux")
	}
	t.Setenv("DISCOBOX_DOCKER_WORKER_IMAGE", "discobox-worker-agent:test")

	ctx := context.Background()
	appStore := newProviderCatalogTestStore(t)
	svc := service.New(appStore, nil, service.JobManagerOptions{})

	if err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	provider, err := appStore.GetSandboxProviderInstance(ctx, service.DefaultProjectID, service.DefaultProviderInstanceID)
	if err != nil {
		t.Fatalf("get default provider: %v", err)
	}
	assertDefaultDockerProviderConfig(t, provider.Config, "")

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
	assertDefaultDockerProviderConfig(t, provider.Config, "")

	provider.Config = []byte(`{"image":"discobox-worker-agent:dev-old","agentPort":3002,"systemd":true,"minWorkers":1,"minHealthyWorkers":1}`)
	if err := appStore.UpdateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("reset default provider config to legacy dev image: %v", err)
	}
	if err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults after legacy dev image reset: %v", err)
	}
	provider, err = appStore.GetSandboxProviderInstance(ctx, service.DefaultProjectID, service.DefaultProviderInstanceID)
	if err != nil {
		t.Fatalf("get repaired default provider: %v", err)
	}
	assertDefaultDockerProviderConfig(t, provider.Config, "")
}

func assertDefaultDockerProviderConfig(t *testing.T, data []byte, expectedImage string) {
	t.Helper()
	var cfg struct {
		Image             string   `json:"image"`
		AgentPort         int      `json:"agentPort"`
		Systemd           bool     `json:"systemd"`
		MinWorkers        int      `json:"minWorkers"`
		MaxWorkers        int      `json:"maxWorkers"`
		MinHealthyWorkers int      `json:"minHealthyWorkers"`
		BindDockerSocket  string   `json:"bindDockerSocket"`
		HostMounts        []string `json:"hostMounts"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode provider config %s: %v", data, err)
	}
	if cfg.Image != expectedImage {
		t.Fatalf("config image = %q, want %q", cfg.Image, expectedImage)
	}
	if cfg.AgentPort != providerdocker.DefaultAgentPort() {
		t.Fatalf("config agentPort = %d, want %d", cfg.AgentPort, providerdocker.DefaultAgentPort())
	}
	if !cfg.Systemd || cfg.MinWorkers != 1 || cfg.MaxWorkers != 1 || cfg.MinHealthyWorkers != 1 {
		t.Fatalf("provider config = %+v, want systemd and worker pool defaults", cfg)
	}
	if cfg.BindDockerSocket != "/var/run/docker.sock" {
		t.Fatalf("bindDockerSocket = %q, want /var/run/docker.sock", cfg.BindDockerSocket)
	}
	expectedHostMounts := existingDefaultHostMounts(t)
	if len(cfg.HostMounts) != len(expectedHostMounts) {
		t.Fatalf("hostMounts = %#v, want %#v", cfg.HostMounts, expectedHostMounts)
	}
	for i, expected := range expectedHostMounts {
		if cfg.HostMounts[i] != expected {
			t.Fatalf("hostMounts[%d] = %#v, want %q", i, cfg.HostMounts[i], expected)
		}
	}
}

func existingDefaultHostMounts(t *testing.T) []string {
	t.Helper()
	var mounts []string
	for _, candidate := range []string{"/home", "/Users"} {
		info, err := os.Stat(candidate)
		if err == nil && info.IsDir() {
			mounts = append(mounts, candidate+":ro")
		}
	}
	return mounts
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
	if got := provider.Definition().Name; got != "DigitalOcean" {
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

func (p *recordingSandboxProvider) Initialize(context.Context, *model.SandboxProviderInstance) error {
	return nil
}

func (p *recordingSandboxProvider) Close() error {
	return nil
}

func (p *recordingSandboxProvider) Definition() sandboxes.ProviderDefinition {
	return sandboxes.ProviderDefinition{Name: "recording"}
}

func (p *recordingSandboxProvider) Status() sandboxes.ProviderStatus {
	return sandboxes.ProviderStatus{Available: true, State: "ready"}
}

func (p *recordingSandboxProvider) List(context.Context) ([]*sandboxes.Sandbox, error) {
	return nil, nil
}
func (p *recordingSandboxProvider) Reconcile(context.Context) error             { return nil }
func (p *recordingSandboxProvider) RemoveProject(context.Context, string) error { return nil }
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
func (p *recordingSandboxProvider) Update(context.Context, sandboxes.SandboxRef, []byte, sandboxes.UpdateOptions) (*sandboxes.Sandbox, []byte, error) {
	return nil, []byte("updated"), nil
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
func (p *recordingSandboxProvider) AcquireHTTPClient(context.Context, sandboxes.SandboxRef, []byte, []string) (*transport.HTTPClientLease, error) {
	return transport.NewHTTPClientLease(http.DefaultClient, nil), nil
}

func mustParseURL(t *testing.T, value string) url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatalf("parse url %q: %v", value, err)
	}
	return *parsed
}
