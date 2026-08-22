package service_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/internal/originkey"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/events"
	"github.com/discobox-ai/discobox/server/internal/harnessdefs"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/resources/sandboxes"
	"github.com/discobox-ai/discobox/server/internal/service"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/x/id"
	"gorm.io/gorm"
)

func TestSandboxReconcileCancelsWhenGenerationChanges(t *testing.T) {
	ctx := context.Background()
	svc, executor, _, projectID := newSandboxTestService(t, nil)

	sandbox, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{HarnessName: serverapi.NewOptString("shell"), Config: serverapi.SandboxCreateConfig{Name: "alpha"}})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if sandbox.Generation != 1 {
		t.Fatalf("create generation = %d, want 1", sandbox.Generation)
	}

	// Existence and spec intent is what bumps the generation now; power
	// operations do not (ADR 0017 §9).
	if err := svc.DeleteSandbox(ctx, projectID, sandbox.ID); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	deleted, err := svc.GetSandbox(ctx, projectID, sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if deleted.Generation != 2 {
		t.Fatalf("delete generation = %d, want 2", deleted.Generation)
	}

	// The in-memory sandbox still carries generation 1; the store moved on to 2.
	// The reconcile's generation-guarded writes must supersede, not clobber.
	_, err = executor.ReconcileSandbox(ctx, sandbox)
	if !errors.Is(err, reconcile.ErrSuperseded) {
		t.Fatalf("stale reconcile error = %v, want ErrJobCanceled", err)
	}
}

func TestSandboxIntentCreatesGenerationScopedJobs(t *testing.T) {
	ctx := context.Background()
	svc, _, _, projectID := newSandboxTestService(t, nil)
	svc.RegisterSandboxProvider("test", noopSandboxProvider{})

	created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{HarnessName: serverapi.NewOptString("shell"), Config: serverapi.SandboxCreateConfig{Name: "alpha"}})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	// Power operations write nothing, so the generation must not move: they are
	// instructions, not intent (ADR 0017 §9).
	started, err := svc.StartSandbox(ctx, projectID, created.ID, services.StartSandboxBody{})
	if err != nil {
		t.Fatalf("start sandbox: %v", err)
	}
	if started.Generation != created.Generation {
		t.Fatalf("start generation = %d, want %d unchanged", started.Generation, created.Generation)
	}
}

func TestReconcileSandboxDoesNotChangeIntent(t *testing.T) {
	ctx := context.Background()
	svc, _, _, projectID := newSandboxTestService(t, nil)

	created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{HarnessName: serverapi.NewOptString("shell"), Config: serverapi.SandboxCreateConfig{Name: "alpha"}})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	reconciled, err := svc.ReconcileSandbox(ctx, projectID, created.ID)
	if err != nil {
		t.Fatalf("reconcile sandbox: %v", err)
	}
	if reconciled.Generation != created.Generation {
		t.Fatalf("reconcile generation = %d, want %d", reconciled.Generation, created.Generation)
	}
	if reconciled.DesiredState != created.DesiredState {
		t.Fatalf("reconcile intent = %q, want %q", reconciled.DesiredState, created.DesiredState)
	}
}

func TestCreateSandboxRecordsOriginAndDerivesKey(t *testing.T) {
	ctx := context.Background()
	svc, _, _, projectID := newSandboxTestService(t, nil)

	created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.SandboxCreateConfig{Name: "alpha"},
		Origin: serverapi.NewOptOrigin(serverapi.Origin{
			HostId:      "host_aaaaaaaaaaaaaaaa",
			Hostname:    serverapi.NewOptString("laptop"),
			ProjectPath: "/src/alpha",
			User:        serverapi.NewOptString("darren"),
		}),
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	if created.Origin == nil {
		t.Fatal("origin was not recorded")
	}
	if created.Origin.HostID != "host_aaaaaaaaaaaaaaaa" || created.Origin.ProjectPath != "/src/alpha" {
		t.Fatalf("origin = %+v, want the client's host and project path", created.Origin)
	}
	if created.Origin.Hostname != "laptop" || created.Origin.User != "darren" {
		t.Fatalf("origin display fields = %+v, want them recorded verbatim", created.Origin)
	}
	want := originkey.Of("host_aaaaaaaaaaaaaaaa", "/src/alpha")
	if created.OriginKey == nil || *created.OriginKey != want {
		t.Fatalf("origin key = %v, want %q", derefString(created.OriginKey), want)
	}
}

// A client that reports no origin still creates a sandbox; it simply cannot be
// listed by project directory.
func TestCreateSandboxWithoutOriginLeavesKeyUnset(t *testing.T) {
	ctx := context.Background()
	svc, _, _, projectID := newSandboxTestService(t, nil)

	created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.SandboxCreateConfig{Name: "alpha"},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if created.Origin != nil || created.OriginKey != nil {
		t.Fatalf("origin = %+v, key = %v, want both unset", created.Origin, derefString(created.OriginKey))
	}
}

func TestCreateSandboxDefaultsGitSourceSlugs(t *testing.T) {
	ctx := context.Background()
	svc, _, _, projectID := newSandboxTestService(t, nil)

	created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
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
	svc, _, _, projectID := newSandboxTestService(t, nil)

	remoteURL, err := url.Parse("https://github.com/discobox-ai/discobox.git")
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
			want: "https://github.com/discobox-ai/discobox.git",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
				HarnessName: serverapi.NewOptString("shell"),
				Config: serverapi.SandboxCreateConfig{
					// Distinct per case: sandbox names are unique within a
					// project, and both cases create one in the same project.
					Name:   "alpha-" + strings.ReplaceAll(tc.name, " ", "-"),
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

	created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.SandboxCreateConfig{Name: "sourceless"},
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
	svc, _, st, projectID := newSandboxTestService(t, nil)

	harness := &model.HarnessConfig{
		ProjectID: projectID,
		Slug:      "codex",
		Name:      "Codex CLI",
		Image:     "discobox-harness-codex:local",
		// Only configured harnesses are selectable, so a sandbox can pin this one.
		Configured: true,
		RunCommand: []string{
			"codex",
		},
	}
	if err := st.CreateHarnessConfig(ctx, harness); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	if _, err := svc.SetDefaultHarnessConfig(ctx, projectID, harness.ID); err != nil {
		t.Fatalf("set default harness config: %v", err)
	}
	project, err := svc.GetProject(ctx, projectID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if project.DefaultHarnessConfigID != harness.ID {
		t.Fatalf("default harness config = %q, want %q", project.DefaultHarnessConfigID, harness.ID)
	}

	// With no explicit harness selector, the sandbox pins the project default so its
	// required-secret gate and binding materialization resolve at create time.
	created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{Config: serverapi.SandboxCreateConfig{Name: "alpha"}})
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

func TestCreateSandboxRequiresResolvedPool(t *testing.T) {
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
	svc := service.New(appStore, engine, service.Options{})
	project, err := svc.InitializeDefaults(ctx, service.DefaultUserID, service.WithoutDefaultProviderInstallation())
	if err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	projectID := project.ID
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

	_, err = svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{HarnessName: serverapi.NewOptString("shell"), Config: serverapi.SandboxCreateConfig{Name: "alpha"}})
	var statusErr apperrors.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("create sandbox error = %v, want status error", err)
	}
	if statusErr.Status != http.StatusBadRequest || statusErr.Message != "sandbox pool is required" {
		t.Fatalf("status error = %d %q, want 400 pool required", statusErr.Status, statusErr.Message)
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
	svc := service.New(appStore, engine, service.Options{}, broker)

	project, err := svc.InitializeDefaults(ctx, service.DefaultUserID, service.WithoutDefaultProviderInstallation())
	if err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	projectID := project.ID
	svc.RegisterSandboxProvider("test", noopSandboxProvider{})
	installDefaultSandboxProviderInstance(ctx, t, appStore, projectID, "provider-test", "test")
	seedTestHarnessConfig(ctx, t, appStore, projectID)
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

	sandbox, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{HarnessName: serverapi.NewOptString("shell"), Config: serverapi.SandboxCreateConfig{Name: "alpha"}})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	// The reconciler brings the sandbox into existence and issues one start; it
	// does not record that the sandbox is running, because it cannot see that.
	// Converged is the whole of what the control plane knows here (ADR 0017 §9).
	sandbox = waitForSandboxConverged(ctx, t, svc, projectID, sandbox.ID)
	if sandbox.DesiredState != model.DesiredStatePresent {
		t.Fatalf("created desired state = %q, want %q", sandbox.DesiredState, model.DesiredStatePresent)
	}
	if sandbox.DesiredState != model.DesiredStatePresent {
		t.Fatalf("created desired state = %q, want present", sandbox.DesiredState)
	}

	// Power operations are delivered to the runtime and change nothing here:
	// no state, no desired state, no generation. The runtime reports what
	// became of them on its own channel, and there is no runtime in this test
	// (ADR 0017 §§9-10).
	for _, instruct := range []func() error{
		func() error {
			_, err := svc.StopSandbox(ctx, projectID, sandbox.ID, services.StopSandboxBody{})
			return err
		},
		func() error {
			_, err := svc.StartSandbox(ctx, projectID, sandbox.ID, services.StartSandboxBody{})
			return err
		},
	} {
		before, err := svc.GetSandbox(ctx, projectID, sandbox.ID)
		if err != nil {
			t.Fatalf("get sandbox: %v", err)
		}
		if err := instruct(); err != nil {
			t.Fatalf("instruct sandbox: %v", err)
		}
		after, err := svc.GetSandbox(ctx, projectID, sandbox.ID)
		if err != nil {
			t.Fatalf("get sandbox: %v", err)
		}
		if after.Generation != before.Generation || after.DesiredState != before.DesiredState || after.State != before.State {
			t.Fatalf("an instruction wrote state: generation %d->%d, desired %q->%q, state %q->%q",
				before.Generation, after.Generation, before.DesiredState, after.DesiredState, before.State, after.State)
		}
	}
	sandbox, err = svc.GetSandbox(ctx, projectID, sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}

	if _, err := svc.RestartSandbox(ctx, projectID, sandbox.ID, services.RestartSandboxBody{}); err != nil {
		t.Fatalf("restart sandbox: %v", err)
	}
	// Restart is an instruction, not intent: it bumps no counter and leaves the
	// recorded state to the runtime's own report (ADR 0017 §§5, 9).
	restarted, err := svc.GetSandbox(ctx, projectID, sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if restarted.Generation != sandbox.Generation {
		t.Fatalf("restart moved the generation %d -> %d", sandbox.Generation, restarted.Generation)
	}

	// Delete is intent like any other: it archives, and the engine converges it.
	// The row survives, which is what makes the sandbox restorable (ADR 0022 §2).
	if err := svc.DeleteSandbox(ctx, projectID, sandbox.ID); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	waitForSandboxState(ctx, t, svc, projectID, sandbox.ID, model.SandboxStateArchived)

	// Purge does not go through the queue: it converges in the request and does
	// not return until the removal is confirmed (ADR 0022 §3).
	if err := svc.PurgeSandbox(ctx, projectID, sandbox.ID); err != nil {
		t.Fatalf("purge sandbox: %v", err)
	}
	if _, err := svc.GetSandbox(ctx, projectID, sandbox.ID); !isNotFoundStatus(err) {
		t.Fatalf("get purged sandbox = %v, want not found", err)
	}
}

// seedTestHarnessConfig gives the project the one harness these tests select by
// name, written straight to the store.
//
// SeedBuiltIns cannot supply it here: it reads each harness's metadata off the
// image's label, so it needs a Docker daemon holding images this checkout may
// never have built, and on Windows it cannot reach a Linux image at all. What
// these tests need from a harness config is that it exists, is configured, and
// answers to "shell" — none of which is a statement about an image. ADR 0066 §7
// names constructing the config directly as the end state this replaces the
// label-only stand-in images with.
func seedTestHarnessConfig(ctx context.Context, t *testing.T, appStore *store.Store, projectID string) {
	t.Helper()
	// Idempotent: on a machine that does have the images built, SeedBuiltIns got
	// there first and this only has to guarantee the config is selectable.
	if existing, err := appStore.GetHarnessConfigBySlug(ctx, projectID, harnessdefs.ShellSlug); err == nil && existing != nil {
		if !existing.Configured {
			existing.Configured = true
			if err := appStore.UpdateHarnessConfig(ctx, existing); err != nil {
				t.Fatalf("configure seeded harness config: %v", err)
			}
		}
		return
	}
	config := &model.HarnessConfig{
		ProjectID:  projectID,
		Slug:       harnessdefs.ShellSlug,
		Name:       "Shell",
		BuiltIn:    true,
		Configured: true,
		Image:      "discobox-harness-shell:test",
		RunCommand: []string{"sh"},
	}
	if err := appStore.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("seed harness config: %v", err)
	}
}

func newSandboxTestService(t *testing.T, notify func()) (*service.Service, *sandboxes.SandboxReconciler, *store.Store, string) {
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
	svc := service.New(appStore, engine, service.Options{}, broker)
	project, err := svc.InitializeDefaults(ctx, service.DefaultUserID, service.WithoutDefaultProviderInstallation())
	if err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	installDefaultSandboxProviderInstance(ctx, t, appStore, project.ID, "provider-test", "test")
	seedTestHarnessConfig(ctx, t, appStore, project.ID)
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
	return svc, svc.NewSandboxReconciler(), appStore, project.ID
}

func installDefaultSandboxProviderInstance(ctx context.Context, t *testing.T, appStore *store.Store, projectID, providerID, providerType string) {
	t.Helper()
	provider := &model.SandboxProviderInstance{
		ID:        providerID,
		ProjectID: projectID,
		Type:      providerType,
		Name:      "Test",
	}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create test provider: %v", err)
	}
	pool := &model.Pool{
		ID:        id.NewString(id.PrefixPool),
		ProjectID: projectID,
		PoolManifest: model.PoolManifest{
			Name:               "Default",
			ProviderInstanceID: provider.ID,
		},
	}
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create test pool: %v", err)
	}
	project, err := appStore.GetProject(ctx, projectID)
	if err != nil {
		t.Fatalf("get default project: %v", err)
	}
	project.DefaultPoolID = pool.ID
	if err := appStore.UpsertProject(ctx, project); err != nil {
		t.Fatalf("set default pool: %v", err)
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

func (noopSandboxProvider) Start(context.Context, sandboxes.SandboxRef, []byte) ([]byte, error) {
	return nil, nil
}

func (noopSandboxProvider) Stop(context.Context, sandboxes.SandboxRef, []byte, time.Duration) ([]byte, error) {
	return nil, nil
}

func (noopSandboxProvider) Restart(context.Context, sandboxes.SandboxRef, []byte, time.Duration) ([]byte, error) {
	return nil, nil
}

func (noopSandboxProvider) Archive(context.Context, sandboxes.SandboxRef, []byte) ([]byte, error) {
	return nil, nil
}

func (noopSandboxProvider) Remove(context.Context, sandboxes.SandboxRef, []byte) ([]byte, error) {
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

func waitForSandboxState(ctx context.Context, t *testing.T, svc *service.Service, projectID, sandboxID, want string) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	last := ""
	for time.Now().Before(deadline) {
		sandbox, err := svc.GetSandbox(ctx, projectID, sandboxID)
		if err != nil {
			t.Fatalf("get sandbox: %v", err)
		}
		if sandbox.State == want {
			return
		}
		last = sandbox.State
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sandbox state = %q, want %q", last, want)
}

func isNotFoundStatus(err error) bool {
	var statusErr apperrors.StatusError
	return errors.As(err, &statusErr) && statusErr.StatusCode() == http.StatusNotFound
}

// parkedCommit is the commit a parked sandbox's source names, fixed at create.
var parkedCommit = "0123456789abcdef0123456789abcdef01234567"

// park puts an existing sandbox into the state the reconciler leaves it in
// after provisioning a push-delivered source: waiting for the client's push.
func park(t *testing.T, st *store.Store, projectID, sandboxID string) {
	t.Helper()
	ctx := context.Background()
	sb, err := st.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	primarySlug := "primary"
	sb.Source = &model.GitSource{
		Kind:     "git",
		Slug:     &primarySlug,
		Delivery: model.GitSourceDeliveryPush,
		// The commit is resolved by the client at create, before any push.
		Checkout: &model.GitSourceCheckout{Commit: &parkedCommit},
	}
	sb.State = model.SandboxStateAwaitingSource
	if err := st.UpdateSandbox(ctx, sb); err != nil {
		t.Fatalf("park sandbox: %v", err)
	}
}

func TestCompleteSandboxSourcePushRecordsCommitAndResumes(t *testing.T) {
	ctx := context.Background()
	svc, _, st, projectID := newSandboxTestService(t, nil)

	created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.SandboxCreateConfig{Name: "alpha"},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	park(t, st, projectID, created.ID)

	resumed, err := svc.CompleteSandboxSourcePush(ctx, projectID, created.ID,
		services.CompleteSandboxSourcePushBody{Sources: map[string]string{"primary": strings.ToUpper(parkedCommit)}})
	if err != nil {
		t.Fatalf("complete source push: %v", err)
	}
	if resumed.SourceDeliveredAt == nil {
		t.Fatal("push was not recorded as delivered")
	}
	// The commit is confirmed, never rewritten: what to check out was decided
	// at create.
	if resumed.Source == nil || resumed.Source.Checkout == nil || derefString(resumed.Source.Checkout.Commit) != parkedCommit {
		t.Fatalf("source commit = %v, want it unchanged at %q", resumed.Source, parkedCommit)
	}
	// Completing the push records new intent; the state still reads
	// awaiting_source because nothing has reconciled yet, and reporting a state
	// the sandbox has not reached would be exactly the lie this model removes.
	// What must be true here is that the sandbox is no longer converged, so the
	// reconciler will resume it.
	if resumed.Converged() {
		t.Fatal("completing the push recorded no new intent; the sandbox would park forever")
	}
	if resumed.DesiredState != model.DesiredStatePresent {
		t.Fatalf("desired state = %q, want running", resumed.DesiredState)
	}
}

// A completion is only meaningful for a sandbox that is actually waiting.
// Accepting one otherwise would restart a sandbox out from under whatever is
// already running in it.
func TestCompleteSandboxSourcePushRejectsSandboxNotAwaitingSource(t *testing.T) {
	ctx := context.Background()
	svc, _, st, projectID := newSandboxTestService(t, nil)

	created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.SandboxCreateConfig{Name: "alpha"},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	// A clone-delivered sandbox has nothing to push.
	_, err = svc.CompleteSandboxSourcePush(ctx, projectID, created.ID,
		services.CompleteSandboxSourcePushBody{Sources: map[string]string{"primary": parkedCommit}})
	if err == nil {
		t.Fatal("completing a push for a clone-delivered sandbox: got nil error, want conflict")
	}
	assertStatusError(t, err, http.StatusConflict)

	// A push-delivered sandbox that already started is past the point of
	// accepting a source.
	park(t, st, projectID, created.ID)
	sb, err := st.GetSandbox(ctx, projectID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	sb.State = model.SandboxStateReady
	if err := st.UpdateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	_, err = svc.CompleteSandboxSourcePush(ctx, projectID, created.ID,
		services.CompleteSandboxSourcePushBody{Sources: map[string]string{"primary": parkedCommit}})
	if err == nil {
		t.Fatal("completing a push for a running sandbox: got nil error, want conflict")
	}
	assertStatusError(t, err, http.StatusConflict)
}

func assertStatusError(t *testing.T, err error, want int) {
	t.Helper()
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) {
		t.Fatalf("error %v is not a status error, want %d", err, want)
	}
	if got := statusErr.StatusCode(); got != want {
		t.Fatalf("status = %d, want %d (%v)", got, want, err)
	}
}

// The commit is a confirmation, not an instruction. A client that pushed
// something other than what the source names must not be able to resume the
// sandbox onto it.
func TestCompleteSandboxSourcePushRejectsMismatchedCommit(t *testing.T) {
	ctx := context.Background()
	svc, _, st, projectID := newSandboxTestService(t, nil)

	created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.SandboxCreateConfig{Name: "alpha"},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	park(t, st, projectID, created.ID)

	_, err = svc.CompleteSandboxSourcePush(ctx, projectID, created.ID,
		services.CompleteSandboxSourcePushBody{Sources: map[string]string{"primary": "fedcba9876543210fedcba9876543210fedcba98"}})
	if err == nil {
		t.Fatal("completing a push with a different commit: got nil error, want conflict")
	}
	assertStatusError(t, err, http.StatusConflict)

	// The sandbox must stay parked rather than resume on the wrong source.
	sb, err := st.GetSandbox(ctx, projectID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.SourceDeliveredAt != nil || sb.State != model.SandboxStateAwaitingSource {
		t.Fatalf("sandbox resumed on a mismatched commit: delivered=%v phase=%q", sb.SourceDeliveredAt, sb.State)
	}
}

// waitForSandboxConverged waits until the reconciler has finished acting on the
// sandbox's current generation.
func waitForSandboxConverged(ctx context.Context, t *testing.T, svc *service.Service, projectID, sandboxID string) *model.Sandbox {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sandbox, err := svc.GetSandbox(ctx, projectID, sandboxID)
		if err != nil {
			t.Fatalf("get sandbox: %v", err)
		}
		if sandbox.Converged() {
			return sandbox
		}
		if time.Now().After(deadline) {
			t.Fatalf("sandbox generations = %d/%d, want converged", sandbox.ObservedGeneration, sandbox.Generation)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
