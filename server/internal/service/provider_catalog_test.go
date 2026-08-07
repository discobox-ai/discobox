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
	"github.com/obot-platform/discobox/id"
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
	svc, _, _, projectID := newSandboxTestService(t, nil)
	provider := &recordingSandboxProvider{}
	svc.RegisterSandboxProvider("recording", provider)
	if got := svc.DefaultSandboxProviderName(); got != "recording" {
		t.Fatalf("default provider = %q, want recording", got)
	}
	providerInstance, err := svc.CreateSandboxProviderInstance(ctx, projectID, services.CreateSandboxProviderInstanceBody{
		Type: "recording",
		Name: "recording",
	})
	if err != nil {
		t.Fatalf("create recording provider instance: %v", err)
	}
	executor := svc.NewSandboxReconciler()

	sourceURL := mustParseURL(t, "https://example.com/repo.git")
	poolID := createPoolForInstance(ctx, t, svc, projectID, providerInstance.ID)
	sb, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		PoolId: serverapi.NewOptString(poolID),
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
	// Bringing a brand-new sandbox up is part of creating it, not a separate
	// start: the runtime is told to start what it builds (ADR 0017 §13).
	if provider.createCalls != 1 || !provider.createOptions.Start {
		t.Fatalf("create calls = %d, asked to start = %v, want 1 and true",
			provider.createCalls, provider.createOptions.Start)
	}
	if provider.createRef.ProjectID != projectID || provider.createOptions.Source == nil || provider.createOptions.Source.URL == nil || *provider.createOptions.Source.URL != "https://example.com/repo.git" || provider.createOptions.Source.Checkout == nil || provider.createOptions.Source.Checkout.RefName == nil || *provider.createOptions.Source.Checkout.RefName != "main" {
		t.Fatalf("create ref/options = %#v %#v", provider.createRef, provider.createOptions)
	}
	sb, err = svc.GetSandbox(ctx, projectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox after start: %v", err)
	}
	// Create is the only runtime call in this flow now, so it is create's
	// rotated secret state that persists.
	if string(sb.SecretState) != "created" {
		t.Fatalf("secret state after create = %q, want created", string(sb.SecretState))
	}
	if len(sb.RuntimeState) == 0 {
		t.Fatal("expected runtime state to be set from provider sandbox")
	}
	if sb.LastActiveAt == nil {
		t.Fatal("expected last active time")
	}
	sb, err = svc.StopSandbox(ctx, projectID, sb.ID, services.StopSandboxBody{})
	if err != nil {
		t.Fatalf("stop sandbox: %v", err)
	}
	// The stop reaches the provider directly; there is no reconcile behind it,
	// because power is not orchestrated (ADR 0017 §9).
	if provider.stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", provider.stopCalls)
	}
	sb, err = svc.GetSandbox(ctx, projectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox after stop: %v", err)
	}
	if string(sb.SecretState) != "stopped" {
		t.Fatalf("secret state after stop = %q, want stopped", string(sb.SecretState))
	}

	// Delete archives: the provider is asked to drop the runtime and keep the
	// data, and the row survives so the sandbox can be restored (ADR 0022 §2).
	if err := svc.DeleteSandbox(ctx, projectID, sb.ID); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	sb, err = svc.GetSandbox(ctx, projectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox after archive intent: %v", err)
	}
	if err := reconcileSandbox(ctx, t, svc, executor, sb.ProjectID, sb.ID); err != nil {
		t.Fatalf("reconcile archive: %v", err)
	}
	if provider.archiveCalls != 1 {
		t.Fatalf("archive calls = %d, want 1", provider.archiveCalls)
	}
	if provider.removeCalls != 0 {
		t.Fatalf("archive removed the data: remove calls = %d, want 0", provider.removeCalls)
	}
	sb, err = svc.GetSandbox(ctx, projectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox after archive = %v, want it retained", err)
	}
	if sb.State != model.SandboxStateArchived {
		t.Fatalf("state after archive = %q, want archived", sb.State)
	}

	// Purge is what destroys it, and only returns once the provider confirms.
	if err := svc.PurgeSandbox(ctx, projectID, sb.ID); err != nil {
		t.Fatalf("purge sandbox: %v", err)
	}
	if provider.removeCalls != 1 {
		t.Fatalf("remove calls = %d, want 1", provider.removeCalls)
	}
	_, err = svc.GetSandbox(ctx, projectID, sb.ID)
	if !isNotFoundStatus(err) {
		t.Fatalf("get sandbox after purge = %v, want not found", err)
	}
}

func TestCreateSandboxUsesDefaultSandboxImage(t *testing.T) {
	ctx := context.Background()
	svc, _, _, projectID := newSandboxTestService(t, nil)
	svc.SetDefaultSandboxImage("discobox-sandbox-agent:default")
	provider := &recordingSandboxProvider{}
	svc.RegisterSandboxProvider("recording", provider)
	providerInstance, err := svc.CreateSandboxProviderInstance(ctx, projectID, services.CreateSandboxProviderInstanceBody{
		Type: "recording",
		Name: "recording",
	})
	if err != nil {
		t.Fatalf("create recording provider instance: %v", err)
	}
	executor := svc.NewSandboxReconciler()

	poolID := createPoolForInstance(ctx, t, svc, projectID, providerInstance.ID)
	sb, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		PoolId: serverapi.NewOptString(poolID),
		Config: serverapi.SandboxCreateConfig{Name: "sandbox-default-image"},
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
	svc, _, _, projectID := newSandboxTestService(t, nil)
	svc.SetDefaultSandboxImage("discobox-sandbox-agent:default")
	provider := &recordingSandboxProvider{}
	svc.RegisterSandboxProvider("recording", provider)
	providerInstance, err := svc.CreateSandboxProviderInstance(ctx, projectID, services.CreateSandboxProviderInstanceBody{
		Type: "recording",
		Name: "recording",
	})
	if err != nil {
		t.Fatalf("create recording provider instance: %v", err)
	}
	executor := svc.NewSandboxReconciler()

	poolID := createPoolForInstance(ctx, t, svc, projectID, providerInstance.ID)
	sb, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		PoolId: serverapi.NewOptString(poolID),
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
	appStore, projectID := newProviderCatalogTestStore(t)
	provider := &recordingSandboxProvider{}
	auth := &recordingSandboxAuth{trustKey: "public-key"}
	executor := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider), sandboxes.WithSandboxAuthenticator(auth))
	if err := appStore.CreateSandboxProviderInstance(ctx, &model.SandboxProviderInstance{ID: "prov-trust", ProjectID: projectID, Type: "recording", Name: "recording"}); err != nil {
		t.Fatalf("create provider instance: %v", err)
	}
	if err := appStore.CreatePool(ctx, &model.Pool{ID: "pool-trust", ProjectID: projectID, PoolManifest: model.PoolManifest{Name: "pool-trust", ProviderInstanceID: "prov-trust"}}); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	sb := &model.Sandbox{
		ID:                "sandbox-1",
		ProjectID:         projectID,
		PoolID:            "pool-trust",
		CreatedByUserID:   service.DefaultUserID,
		Name:              "sandbox-1",
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.DesiredStatePresent, State: model.SandboxStatePending},
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
	if auth.projectID != projectID {
		t.Fatalf("auth project id = %q, want %q", auth.projectID, projectID)
	}
	if got := provider.createOptions.Env["DISCOBOX_TRUST_KEY"]; got != "public-key" {
		t.Fatalf("trust key env = %q, want public-key", got)
	}
}

// newProviderCatalogTestStore builds a store and seeds the default identity
// and project via InitializeDefaults, without installing the built-in
// provider, so callers control provider/pool setup themselves.
func newProviderCatalogTestStore(t *testing.T) (*store.Store, string) {
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
	svc := service.New(appStore, nil, service.Options{})
	project, err := svc.InitializeDefaults(ctx, service.DefaultUserID, service.WithoutDefaultProviderInstallation())
	if err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	return appStore, project.ID
}

func TestServiceSandboxProviderCatalog(t *testing.T) {
	svc, _, _, _ := newSandboxTestService(t, nil)
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
	svc, _, _, projectID := newSandboxTestService(t, nil)
	svc.RegisterSandboxProvider("recording", &recordingSandboxProvider{})

	provider, err := svc.CreateSandboxProviderInstance(ctx, projectID, services.CreateSandboxProviderInstanceBody{
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
	appStore, projectID := newProviderCatalogTestStore(t)
	svc := service.New(appStore, nil, service.Options{})

	if _, err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	providers, err := svc.ListSandboxProviderInstances(ctx, projectID)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("providers len = %d, want 1", len(providers))
	}
	if !id.IsGenerated(providers[0].ID) {
		t.Fatalf("provider = %#v, want generated id", providers[0])
	}
	defaultProviderID := providers[0].ID
	if runtime.GOOS == "linux" {
		if providers[0].Type != "docker" || providers[0].Disabled {
			t.Fatalf("linux provider = %#v, want enabled docker", providers[0])
		}
		assertDefaultDockerProviderConfig(t, providers[0].Config, "")
	}
	if _, err := appStore.GetServerState(ctx, "defaults.default_sandbox_provider.installed"); err != nil {
		t.Fatalf("get install state: %v", err)
	}

	// The default pool is seeded exactly once and the project points at it.
	pools, err := svc.ListPools(ctx, projectID)
	if err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if len(pools) != 1 {
		t.Fatalf("pools len = %d, want 1", len(pools))
	}
	pool := pools[0]
	if pool.ProviderInstanceID != defaultProviderID || !id.IsGenerated(pool.ID) {
		t.Fatalf("default pool = %#v, want generated pool on the default provider", pool)
	}
	project, err := appStore.GetProject(ctx, projectID)
	if err != nil {
		t.Fatalf("get default project: %v", err)
	}
	if project.DefaultPoolID != pool.ID {
		t.Fatalf("project default pool = %q, want %q", project.DefaultPoolID, pool.ID)
	}

	if _, err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults again: %v", err)
	}
	pools, err = svc.ListPools(ctx, projectID)
	if err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if len(pools) != 1 {
		t.Fatalf("pools len = %d, want 1 after re-init", len(pools))
	}

	// Deleting the default pool is a normal, permanent user action: a later
	// restart (another InitializeDefaults call) must not recreate it.
	if err := appStore.DeletePool(ctx, projectID, pool.ID); err != nil {
		t.Fatalf("delete default pool: %v", err)
	}
	if _, err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults after pool delete: %v", err)
	}
	pools, err = svc.ListPools(ctx, projectID)
	if err != nil {
		t.Fatalf("list pools after delete: %v", err)
	}
	if len(pools) != 0 {
		t.Fatalf("pools len = %d after delete + re-init, want 0 (deleted pool must not be recreated)", len(pools))
	}

	// The provider is likewise not protected once it has no pools, and is not
	// recreated on restart either.
	if err := svc.DeleteSandboxProviderInstance(ctx, projectID, defaultProviderID); err != nil {
		t.Fatalf("delete default provider: %v", err)
	}
	if _, err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults after provider delete: %v", err)
	}
	providers, err = svc.ListSandboxProviderInstances(ctx, projectID)
	if err != nil {
		t.Fatalf("list providers after delete: %v", err)
	}
	if len(providers) != 0 {
		t.Fatalf("providers len = %d after delete + re-init, want 0 (deleted provider must not be recreated)", len(providers))
	}
}

// Seeding happens once; afterward the provider instance belongs to the user.
// The server must never rewrite its config on a later boot, not even to restore
// a key the user removed.
func TestInitializeDefaultsLeavesEditedDefaultProviderConfigAlone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("default docker provider is installed on linux")
	}

	ctx := context.Background()
	appStore, projectID := newProviderCatalogTestStore(t)
	svc := service.New(appStore, nil, service.Options{})

	if _, err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	providerID := defaultProviderID(ctx, t, appStore, projectID)
	provider, err := appStore.GetSandboxProviderInstance(ctx, projectID, providerID)
	if err != nil {
		t.Fatalf("get default provider: %v", err)
	}
	edited := []byte(`{"agentPort":3999}`)
	provider.Config = edited
	if err := appStore.UpdateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("edit default provider config: %v", err)
	}

	if _, err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults after config edit: %v", err)
	}
	provider, err = appStore.GetSandboxProviderInstance(ctx, projectID, providerID)
	if err != nil {
		t.Fatalf("get default provider after re-init: %v", err)
	}
	if string(provider.Config) != string(edited) {
		t.Fatalf("config = %s, want the user's edit %s preserved", provider.Config, edited)
	}
}

// defaultProviderID returns the ID of the (single) provider instance seeded
// for the project, since InitializeDefaults no longer uses a fixed ID.
func defaultProviderID(ctx context.Context, t *testing.T, appStore *store.Store, projectID string) string {
	t.Helper()
	providers, err := appStore.ListSandboxProviderInstances(ctx, projectID)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("providers len = %d, want 1", len(providers))
	}
	return providers[0].ID
}

func TestInitializeDefaultsDoesNotPersistDefaultDockerProviderImageFromEnv(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("default docker provider is installed on linux")
	}
	t.Setenv("DISCOBOX_DOCKER_POOL_IMAGE", "discobox-pool-agent:test")

	ctx := context.Background()
	appStore, projectID := newProviderCatalogTestStore(t)
	svc := service.New(appStore, nil, service.Options{})

	if _, err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	providerID := defaultProviderID(ctx, t, appStore, projectID)
	provider, err := appStore.GetSandboxProviderInstance(ctx, projectID, providerID)
	if err != nil {
		t.Fatalf("get default provider: %v", err)
	}
	assertDefaultDockerProviderConfig(t, provider.Config, "")
}

// createPoolForInstance creates a pool bound to the provider instance so a
// sandbox can be scheduled into it (sandboxes bind to pools, not providers).
func createPoolForInstance(ctx context.Context, t *testing.T, svc *service.Service, projectID, providerInstanceID string) string {
	t.Helper()
	pool, err := svc.CreatePool(ctx, projectID, services.CreatePoolBody{
		Name:               "pool-" + providerInstanceID,
		ProviderInstanceId: providerInstanceID,
	})
	if err != nil {
		t.Fatalf("create pool for %s: %v", providerInstanceID, err)
	}
	return pool.ID
}

func assertDefaultDockerProviderConfig(t *testing.T, data []byte, expectedImage string) {
	t.Helper()
	var cfg struct {
		Image            string   `json:"image"`
		AgentPort        int      `json:"agentPort"`
		BindDockerSocket string   `json:"bindDockerSocket"`
		HostMounts       []string `json:"hostMounts"`
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
	svc, _, _, _ := newSandboxTestService(t, nil)
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
	archiveCalls  int
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
		Metadata:  map[string]string{"pool_id": "pool-1"},
	}, []byte("created"), nil
}
func (p *recordingSandboxProvider) Update(context.Context, sandboxes.SandboxRef, []byte, sandboxes.UpdateOptions) (*sandboxes.Sandbox, []byte, error) {
	return nil, []byte("updated"), nil
}
func (p *recordingSandboxProvider) Start(context.Context, sandboxes.SandboxRef, []byte) ([]byte, error) {
	p.startCalls++
	return []byte("started"), nil
}
func (p *recordingSandboxProvider) Stop(context.Context, sandboxes.SandboxRef, []byte, time.Duration) ([]byte, error) {
	p.stopCalls++
	return []byte("stopped"), nil
}
func (p *recordingSandboxProvider) Restart(context.Context, sandboxes.SandboxRef, []byte, time.Duration) ([]byte, error) {
	return nil, nil
}
func (p *recordingSandboxProvider) Archive(context.Context, sandboxes.SandboxRef, []byte) ([]byte, error) {
	p.archiveCalls++
	return nil, nil
}

func (p *recordingSandboxProvider) Remove(context.Context, sandboxes.SandboxRef, []byte) ([]byte, error) {
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
