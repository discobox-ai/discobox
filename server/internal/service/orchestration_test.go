package service_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	serverapi "github.com/obot-platform/discobox/api/gen"
	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/events"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/reconcile"
	"github.com/obot-platform/discobox/server/internal/resources/sandboxes"
	"github.com/obot-platform/discobox/server/internal/service"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
	"gorm.io/gorm"
)

func TestSandboxReconcileCancelsWhenGenerationChanges(t *testing.T) {
	ctx := context.Background()
	svc, executor := newSandboxTestService(t, nil)

	sandbox, err := svc.CreateSandbox(ctx, service.DefaultProjectID, services.CreateSandboxBody{Config: serverapi.SandboxCreateConfig{Name: "alpha"}})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if sandbox.Generation != 1 {
		t.Fatalf("create generation = %d, want 1", sandbox.Generation)
	}

	stopped, err := svc.StopSandbox(ctx, service.DefaultProjectID, sandbox.ID, services.StopSandboxBody{})
	if err != nil {
		t.Fatalf("stop sandbox: %v", err)
	}
	if stopped.Generation != 2 {
		t.Fatalf("stop generation = %d, want 2", stopped.Generation)
	}

	// The in-memory sandbox still carries generation 1; the store moved on to 2.
	// The reconcile's generation-guarded writes must supersede, not clobber.
	err = executor.ReconcileSandbox(ctx, sandbox)
	if !errors.Is(err, reconcile.ErrSuperseded) {
		t.Fatalf("stale reconcile error = %v, want ErrJobCanceled", err)
	}
}

func TestSandboxIntentCreatesGenerationScopedJobs(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSandboxTestService(t, nil)

	created, err := svc.CreateSandbox(ctx, service.DefaultProjectID, services.CreateSandboxBody{Config: serverapi.SandboxCreateConfig{Name: "alpha"}})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	started, err := svc.StartSandbox(ctx, service.DefaultProjectID, created.ID, services.StartSandboxBody{})
	if err != nil {
		t.Fatalf("start sandbox: %v", err)
	}
	if started.Generation != created.Generation+1 {
		t.Fatalf("start generation = %d, want %d", started.Generation, created.Generation+1)
	}
}

func TestReconcileSandboxDoesNotChangeIntent(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSandboxTestService(t, nil)

	created, err := svc.CreateSandbox(ctx, service.DefaultProjectID, services.CreateSandboxBody{Config: serverapi.SandboxCreateConfig{Name: "alpha"}})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	reconciled, err := svc.ReconcileSandbox(ctx, service.DefaultProjectID, created.ID)
	if err != nil {
		t.Fatalf("reconcile sandbox: %v", err)
	}
	if reconciled.Generation != created.Generation {
		t.Fatalf("reconcile generation = %d, want %d", reconciled.Generation, created.Generation)
	}
	if reconciled.DesiredState != created.DesiredState || derefString(reconciled.ActiveOperation) != derefString(created.ActiveOperation) {
		t.Fatalf("reconcile intent = %q/%q, want %q/%q", reconciled.DesiredState, derefString(reconciled.ActiveOperation), created.DesiredState, derefString(created.ActiveOperation))
	}
}

func TestCreateSandboxDefaultsGitSourceSlugs(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSandboxTestService(t, nil)

	created, err := svc.CreateSandbox(ctx, service.DefaultProjectID, services.CreateSandboxBody{
		Config: serverapi.SandboxCreateConfig{
			Name: "alpha",
			Source: serverapi.NewOptGitSource(serverapi.GitSource{
				Kind: serverapi.GitSourceKindGit,
			}),
			SourceCodeReferences: serverapi.NewOptSandboxCreateConfigSourceCodeReferences(serverapi.SandboxCreateConfigSourceCodeReferences{
				"/workspace/Docs": {
					Kind: serverapi.GitSourceKindGit,
				},
				"workspace docs": {
					Kind: serverapi.GitSourceKindGit,
				},
				"tools": {
					Kind: serverapi.GitSourceKindGit,
					Slug: serverapi.NewOptString("custom-tools"),
				},
			}),
		},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	if created.Source == nil || created.Source.Slug == nil || *created.Source.Slug != "primary" {
		t.Fatalf("primary slug = %#v, want primary", created.Source)
	}
	docs := created.SourceCodeReferences["/workspace/Docs"]
	if docs.Slug == nil || *docs.Slug != "workspace-docs" {
		t.Fatalf("docs ref slug = %v, want workspace-docs", docs.Slug)
	}
	colliding := created.SourceCodeReferences["workspace docs"]
	if colliding.Slug == nil || *colliding.Slug == "workspace-docs" || !strings.HasPrefix(*colliding.Slug, "workspace-docs-") {
		t.Fatalf("colliding ref slug = %v, want deterministic workspace-docs suffix", colliding.Slug)
	}
	tools := created.SourceCodeReferences["tools"]
	if tools.Slug == nil || *tools.Slug != "custom-tools" {
		t.Fatalf("tools ref slug = %v, want custom-tools", tools.Slug)
	}
}

func TestCreateSandboxDerivesSourceRootFromPrimarySource(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSandboxTestService(t, nil)

	remoteURL, err := url.Parse("https://github.com/obot-platform/discobox.git")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		source serverapi.GitSource
		want   string
	}{
		{
			name: "local repository root",
			source: serverapi.GitSource{
				Kind:           serverapi.GitSourceKindGit,
				LocalDirectory: serverapi.NewOptString("/home/darren/src/disco2"),
				Checkout:       serverapi.NewOptGitSourceCheckout(serverapi.GitSourceCheckout{Commit: serverapi.NewOptString("abc123")}),
			},
			want: "/home/darren/src/disco2",
		},
		{
			name: "remote url",
			source: serverapi.GitSource{
				Kind:     serverapi.GitSourceKindGit,
				URL:      serverapi.NewOptURI(*remoteURL),
				Checkout: serverapi.NewOptGitSourceCheckout(serverapi.GitSourceCheckout{RefName: serverapi.NewOptString("main")}),
			},
			want: "https://github.com/obot-platform/discobox.git",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created, err := svc.CreateSandbox(ctx, service.DefaultProjectID, services.CreateSandboxBody{
				Config: serverapi.SandboxCreateConfig{
					Name:   "alpha",
					Source: serverapi.NewOptGitSource(tc.source),
				},
			})
			if err != nil {
				t.Fatalf("create sandbox: %v", err)
			}
			if created.SourceRoot == nil || *created.SourceRoot != tc.want {
				t.Fatalf("source root = %v, want %q", created.SourceRoot, tc.want)
			}
		})
	}

	created, err := svc.CreateSandbox(ctx, service.DefaultProjectID, services.CreateSandboxBody{
		Config: serverapi.SandboxCreateConfig{Name: "sourceless"},
	})
	if err != nil {
		t.Fatalf("create sandbox without source: %v", err)
	}
	if created.SourceRoot != nil {
		t.Fatalf("source root = %v, want nil for a sandbox with no source", created.SourceRoot)
	}
}

func TestCreateSandboxPinsDefaultHarnessConfig(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSandboxTestService(t, nil)

	harness, err := svc.CreateHarnessConfig(ctx, service.DefaultProjectID, services.CreateHarnessConfigBody{
		Name:       serverapi.NewOptString("Codex"),
		RunCommand: serverapi.NewOptNilStringArray([]string{"codex", "exec"}),
	})
	if err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	project, err := svc.GetProject(ctx, service.DefaultProjectID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if project.DefaultHarnessConfigID != harness.ID {
		t.Fatalf("default harness config = %q, want %q", project.DefaultHarnessConfigID, harness.ID)
	}

	// With no explicit harness selector, the sandbox pins the project default so its
	// required-secret gate and binding materialization resolve at create time.
	created, err := svc.CreateSandbox(ctx, service.DefaultProjectID, services.CreateSandboxBody{Config: serverapi.SandboxCreateConfig{Name: "alpha"}})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if created.HarnessConfigID == nil || *created.HarnessConfigID != harness.ID {
		t.Fatalf("sandbox agent config = %v, want %q", created.HarnessConfigID, harness.ID)
	}
}

// newTestReconcileEngine builds the level-triggered reconcile engine job
// managers now require (provider reconciliation rides it).
func newTestReconcileEngine(t *testing.T, db *gorm.DB) *reconcile.Engine {
	t.Helper()
	engine, err := reconcile.New(db, reconcile.Options{SingleNode: true})
	if err != nil {
		t.Fatalf("new reconcile engine: %v", err)
	}
	return engine
}

func TestCreateSandboxRequiresResolvedProviderInstance(t *testing.T) {
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
	engine := newTestReconcileEngine(t, db.Write)
	svc := service.New(appStore, engine, service.JobManagerOptions{})
	if err := svc.InitializeDefaults(ctx, service.DefaultUserID, service.WithoutDefaultProviderInstallation()); err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("start reconcile engine: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := engine.Stop(stopCtx); err != nil {
			t.Fatalf("stop reconcile engine: %v", err)
		}
	})

	_, err = svc.CreateSandbox(ctx, service.DefaultProjectID, services.CreateSandboxBody{Config: serverapi.SandboxCreateConfig{Name: "alpha"}})
	var statusErr apperrors.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("create sandbox error = %v, want status error", err)
	}
	if statusErr.Status != http.StatusBadRequest || statusErr.Message != "sandbox provider instance is required" {
		t.Fatalf("status error = %d %q, want 400 provider required", statusErr.Status, statusErr.Message)
	}
}

func TestSandboxIntentIsReconciledByJobQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

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

	broker := events.NewBroker()
	appStore := store.New(db.Write, db.Read, store.WithPublisher(broker))
	engine := newTestReconcileEngine(t, db.Write)
	svc := service.New(appStore, engine, service.JobManagerOptions{}, broker)

	if err := svc.InitializeDefaults(ctx, service.DefaultUserID, service.WithoutDefaultProviderInstallation()); err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	svc.RegisterSandboxProvider("test", noopSandboxProvider{})
	installDefaultSandboxProviderInstance(ctx, t, appStore, "provider-test", "test")
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		if err := engine.Stop(stopCtx); err != nil {
			t.Fatalf("stop reconcile engine: %v", err)
		}
	})

	sandbox, err := svc.CreateSandbox(ctx, service.DefaultProjectID, services.CreateSandboxBody{Config: serverapi.SandboxCreateConfig{Name: "alpha"}})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	sandbox = waitForSandboxPhase(ctx, t, svc, sandbox.ID, model.SandboxPhaseRunning)
	if sandbox.DesiredState != model.SandboxDesiredStateRunning {
		t.Fatalf("created desired state = %q, want %q", sandbox.DesiredState, model.SandboxDesiredStateRunning)
	}
	if sandbox.ActiveOperation != nil {
		t.Fatalf("created active operation = %v, want nil", *sandbox.ActiveOperation)
	}

	if _, err := svc.StopSandbox(ctx, service.DefaultProjectID, sandbox.ID, services.StopSandboxBody{}); err != nil {
		t.Fatalf("stop sandbox: %v", err)
	}
	sandbox = waitForSandboxPhase(ctx, t, svc, sandbox.ID, model.SandboxPhaseStopped)
	if sandbox.DesiredState != model.SandboxDesiredStateStopped {
		t.Fatalf("stopped desired state = %q, want %q", sandbox.DesiredState, model.SandboxDesiredStateStopped)
	}

	if _, err := svc.StartSandbox(ctx, service.DefaultProjectID, sandbox.ID, services.StartSandboxBody{}); err != nil {
		t.Fatalf("start sandbox: %v", err)
	}
	sandbox = waitForSandboxPhase(ctx, t, svc, sandbox.ID, model.SandboxPhaseRunning)
	if sandbox.DesiredState != model.SandboxDesiredStateRunning {
		t.Fatalf("started desired state = %q, want %q", sandbox.DesiredState, model.SandboxDesiredStateRunning)
	}

	if _, err := svc.RestartSandbox(ctx, service.DefaultProjectID, sandbox.ID, services.RestartSandboxBody{}); err != nil {
		t.Fatalf("restart sandbox: %v", err)
	}
	sandbox = waitForSandboxPhase(ctx, t, svc, sandbox.ID, model.SandboxPhaseRunning)
	if sandbox.RestartGeneration != 1 {
		t.Fatalf("restart generation = %d, want 1", sandbox.RestartGeneration)
	}
	if sandbox.RestartedGeneration != sandbox.RestartGeneration {
		t.Fatalf("restarted generation = %d, want %d", sandbox.RestartedGeneration, sandbox.RestartGeneration)
	}

	if err := svc.DeleteSandbox(ctx, service.DefaultProjectID, sandbox.ID); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	sandbox = waitForSandboxPhase(ctx, t, svc, sandbox.ID, model.SandboxPhaseDeleted)
	if sandbox.DesiredState != model.SandboxDesiredStateDeleted {
		t.Fatalf("deleted desired state = %q, want %q", sandbox.DesiredState, model.SandboxDesiredStateDeleted)
	}
}

func newSandboxTestService(t *testing.T, notify func()) (*service.Service, *sandboxes.SandboxReconciler) {
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

	broker := events.NewBroker()
	appStore := store.New(db.Write, db.Read, store.WithPublisher(broker))
	var notifyContext func(context.Context)
	if notify != nil {
		notifyContext = func(context.Context) { notify() }
	}
	_ = notifyContext
	engine := newTestReconcileEngine(t, db.Write)
	svc := service.New(appStore, engine, service.JobManagerOptions{}, broker)
	if err := svc.InitializeDefaults(ctx, service.DefaultUserID, service.WithoutDefaultProviderInstallation()); err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	installDefaultSandboxProviderInstance(ctx, t, appStore, "provider-test", "test")
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("start reconcile engine: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := engine.Stop(stopCtx); err != nil {
			t.Fatalf("stop reconcile engine: %v", err)
		}
	})
	return svc, svc.NewSandboxReconciler()
}

func installDefaultSandboxProviderInstance(ctx context.Context, t *testing.T, appStore *store.Store, providerID, providerType string) {
	t.Helper()
	provider := &model.SandboxProviderInstance{
		ID:        providerID,
		ProjectID: service.DefaultProjectID,
		Type:      providerType,
		Name:      "Test",
	}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create test provider: %v", err)
	}
	project, err := appStore.GetProject(ctx, service.DefaultProjectID)
	if err != nil {
		t.Fatalf("get default project: %v", err)
	}
	project.DefaultSandboxProviderID = provider.ID
	if err := appStore.UpsertProject(ctx, project); err != nil {
		t.Fatalf("set default provider: %v", err)
	}
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

type noopSandboxProvider struct{}

func (noopSandboxProvider) Initialize(context.Context, *model.SandboxProviderInstance) error {
	return nil
}

func (noopSandboxProvider) Close() error {
	return nil
}

func (noopSandboxProvider) Definition() sandboxes.ProviderDefinition {
	return sandboxes.ProviderDefinition{Name: "noop"}
}

func (noopSandboxProvider) Status() sandboxes.ProviderStatus {
	return sandboxes.ProviderStatus{Available: true, State: "ready"}
}

func (noopSandboxProvider) Reconcile(context.Context) error {
	return nil
}

func (noopSandboxProvider) RemoveProject(context.Context, string) error {
	return nil
}

func (noopSandboxProvider) List(context.Context) ([]*sandboxes.Sandbox, error) {
	return nil, nil
}

func (noopSandboxProvider) Create(_ context.Context, ref sandboxes.SandboxRef, _ []byte, _ sandboxes.CreateOptions) (*sandboxes.Sandbox, []byte, error) {
	return runtimeSandbox(ref, sandboxes.StatusCreated), nil, nil
}

func (noopSandboxProvider) Update(_ context.Context, ref sandboxes.SandboxRef, _ []byte, _ sandboxes.UpdateOptions) (*sandboxes.Sandbox, []byte, error) {
	return runtimeSandbox(ref, sandboxes.Status("running")), nil, nil
}

func (noopSandboxProvider) Start(_ context.Context, ref sandboxes.SandboxRef, _ []byte) (*sandboxes.Sandbox, []byte, error) {
	return runtimeSandbox(ref, sandboxes.Status("running")), nil, nil
}

func (noopSandboxProvider) Stop(_ context.Context, ref sandboxes.SandboxRef, _ []byte, _ time.Duration) (*sandboxes.Sandbox, []byte, error) {
	return runtimeSandbox(ref, sandboxes.Status("stopped")), nil, nil
}

func (noopSandboxProvider) Remove(context.Context, sandboxes.SandboxRef, []byte, ...sandboxes.RemoveOption) ([]byte, error) {
	return nil, nil
}

func (noopSandboxProvider) Get(_ context.Context, ref sandboxes.SandboxRef, _ []byte) (*sandboxes.Sandbox, error) {
	return runtimeSandbox(ref, sandboxes.Status("running")), nil
}

func (noopSandboxProvider) AcquireHTTPClient(context.Context, sandboxes.SandboxRef, []byte, []string) (*sandboxes.HTTPClientLease, error) {
	return nil, nil
}

func runtimeSandbox(ref sandboxes.SandboxRef, status sandboxes.Status) *sandboxes.Sandbox {
	return &sandboxes.Sandbox{
		ID:        "runtime-" + ref.SandboxID,
		SandboxID: ref.SandboxID,
		Status:    status,
	}
}

func waitForSandboxPhase(ctx context.Context, t *testing.T, svc *service.Service, sandboxID, phase string) *model.Sandbox {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sandbox, err := svc.GetSandbox(ctx, service.DefaultProjectID, sandboxID)
		if err != nil {
			t.Fatalf("get sandbox: %v", err)
		}
		if sandbox.Phase == phase {
			return sandbox
		}
		time.Sleep(10 * time.Millisecond)
	}

	sandbox, err := svc.GetSandbox(ctx, service.DefaultProjectID, sandboxID)
	if err != nil {
		t.Fatalf("get sandbox after timeout: %v", err)
	}
	t.Fatalf("sandbox phase = %q, want %q", sandbox.Phase, phase)
	return nil
}
