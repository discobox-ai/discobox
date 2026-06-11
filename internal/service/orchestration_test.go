package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/obot-platform/disco2/internal/api"
	"github.com/obot-platform/disco2/internal/database"
	"github.com/obot-platform/disco2/internal/events"
	"github.com/obot-platform/disco2/internal/model"
	"github.com/obot-platform/disco2/internal/sandbox"
	"github.com/obot-platform/disco2/internal/sandbox/jobs"
	"github.com/obot-platform/disco2/internal/service"
	"github.com/obot-platform/disco2/internal/store"
	"github.com/obot-platform/disco2/orchestration"
)

func TestSandboxReconcileCancelsWhenGenerationChanges(t *testing.T) {
	ctx := context.Background()
	svc, reconciler := newSandboxTestService(t, nil)

	sandbox, err := svc.CreateSandbox(ctx, service.DefaultProjectID, api.CreateSandboxBody{Name: "alpha"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if sandbox.Generation != 1 {
		t.Fatalf("create generation = %d, want 1", sandbox.Generation)
	}

	stopped, err := svc.StopSandbox(ctx, service.DefaultProjectID, sandbox.ID, api.StopSandboxBody{})
	if err != nil {
		t.Fatalf("stop sandbox: %v", err)
	}
	if stopped.Generation != 2 {
		t.Fatalf("stop generation = %d, want 2", stopped.Generation)
	}

	err = reconciler.ReconcileSandboxJob(ctx, service.DefaultProjectID, sandbox.ID, "stale-job", sandbox.Generation)
	if !errors.Is(err, orchestration.ErrJobCanceled) {
		t.Fatalf("stale reconcile error = %v, want ErrJobCanceled", err)
	}
}

func TestSandboxIntentCreatesGenerationScopedJobs(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSandboxTestService(t, nil)

	created, err := svc.CreateSandbox(ctx, service.DefaultProjectID, api.CreateSandboxBody{Name: "alpha"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if created.LastJobID == nil {
		t.Fatal("create last job ID is nil")
	}

	started, err := svc.StartSandbox(ctx, service.DefaultProjectID, created.ID, api.StartSandboxBody{})
	if err != nil {
		t.Fatalf("start sandbox: %v", err)
	}
	if started.LastJobID == nil {
		t.Fatal("start last job ID is nil")
	}
	if started.Generation != created.Generation+1 {
		t.Fatalf("start generation = %d, want %d", started.Generation, created.Generation+1)
	}
	if *started.LastJobID == *created.LastJobID {
		t.Fatalf("start reused create job ID %s; want a generation-scoped job", *started.LastJobID)
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
	if err := db.MigrateTenant(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	broker := events.NewBroker()
	appStore := store.New(database.StaticResolver{DB: db}, store.WithPublisher(broker), store.WithDefaultTenantID(service.DefaultTenantID))
	queueConfig := orchestration.QueueConfig{DefaultMaxAttempts: 3}
	dispatcher := orchestration.NewDispatcher(appStore, orchestration.DispatcherConfig{
		SingleNode:         true,
		PollInterval:       10 * time.Millisecond,
		JobTimeout:         time.Second,
		StaleJobTimeout:    time.Minute,
		ImmediateExecution: true,
		DefaultConcurrency: 1,
	})
	svc := service.New(appStore, queueConfig, func(context.Context) { dispatcher.NotifyNewJob() }, broker)
	reconciler := sandbox.NewSandboxReconciler(appStore)
	if err := dispatcher.Register(jobs.NewSandboxReconcileExecutor(reconciler)); err != nil {
		t.Fatalf("register executor: %v", err)
	}

	if err := svc.InitializeDefaults(ctx, service.DefaultTenantID, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	if err := dispatcher.Start(ctx); err != nil {
		t.Fatalf("start dispatcher: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		if err := dispatcher.DrainAndStop(stopCtx); err != nil {
			t.Fatalf("stop dispatcher: %v", err)
		}
	})

	sandbox, err := svc.CreateSandbox(ctx, service.DefaultProjectID, api.CreateSandboxBody{Name: "alpha"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	sandbox = waitForSandboxPhase(t, ctx, svc, sandbox.ID, model.SandboxPhaseRunning)
	if sandbox.DesiredState != model.SandboxDesiredStateRunning {
		t.Fatalf("created desired state = %q, want %q", sandbox.DesiredState, model.SandboxDesiredStateRunning)
	}
	if sandbox.ActiveOperation != nil {
		t.Fatalf("created active operation = %v, want nil", *sandbox.ActiveOperation)
	}

	if _, err := svc.StopSandbox(ctx, service.DefaultProjectID, sandbox.ID, api.StopSandboxBody{}); err != nil {
		t.Fatalf("stop sandbox: %v", err)
	}
	sandbox = waitForSandboxPhase(t, ctx, svc, sandbox.ID, model.SandboxPhaseStopped)
	if sandbox.DesiredState != model.SandboxDesiredStateStopped {
		t.Fatalf("stopped desired state = %q, want %q", sandbox.DesiredState, model.SandboxDesiredStateStopped)
	}

	if _, err := svc.StartSandbox(ctx, service.DefaultProjectID, sandbox.ID, api.StartSandboxBody{}); err != nil {
		t.Fatalf("start sandbox: %v", err)
	}
	sandbox = waitForSandboxPhase(t, ctx, svc, sandbox.ID, model.SandboxPhaseRunning)
	if sandbox.DesiredState != model.SandboxDesiredStateRunning {
		t.Fatalf("started desired state = %q, want %q", sandbox.DesiredState, model.SandboxDesiredStateRunning)
	}

	if _, err := svc.RestartSandbox(ctx, service.DefaultProjectID, sandbox.ID, api.RestartSandboxBody{}); err != nil {
		t.Fatalf("restart sandbox: %v", err)
	}
	sandbox = waitForSandboxPhase(t, ctx, svc, sandbox.ID, model.SandboxPhaseRunning)
	if sandbox.RestartGeneration != 1 {
		t.Fatalf("restart generation = %d, want 1", sandbox.RestartGeneration)
	}
	if sandbox.RestartedGeneration != sandbox.RestartGeneration {
		t.Fatalf("restarted generation = %d, want %d", sandbox.RestartedGeneration, sandbox.RestartGeneration)
	}

	if err := svc.DeleteSandbox(ctx, service.DefaultProjectID, sandbox.ID); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	sandbox = waitForSandboxPhase(t, ctx, svc, sandbox.ID, model.SandboxPhaseDeleted)
	if sandbox.DesiredState != model.SandboxDesiredStateDeleted {
		t.Fatalf("deleted desired state = %q, want %q", sandbox.DesiredState, model.SandboxDesiredStateDeleted)
	}
}

func newSandboxTestService(t *testing.T, notify func()) (*service.Service, *sandbox.SandboxReconciler) {
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

	broker := events.NewBroker()
	appStore := store.New(database.StaticResolver{DB: db}, store.WithPublisher(broker), store.WithDefaultTenantID(service.DefaultTenantID))
	queueConfig := orchestration.QueueConfig{DefaultMaxAttempts: 3}
	var notifyContext func(context.Context)
	if notify != nil {
		notifyContext = func(context.Context) { notify() }
	}
	svc := service.New(appStore, queueConfig, notifyContext, broker)
	if err := svc.InitializeDefaults(ctx, service.DefaultTenantID, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}
	return svc, svc.NewSandboxReconciler()
}

func waitForSandboxPhase(t *testing.T, ctx context.Context, svc *service.Service, sandboxID, phase string) *model.Sandbox {
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
