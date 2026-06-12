package server

import (
	"context"
	"sync"
	"time"

	"github.com/obot-platform/discobox/internal/sandbox/jobs"
	"github.com/obot-platform/discobox/internal/service"
	"github.com/obot-platform/discobox/internal/store"
	"github.com/obot-platform/discobox/internal/tenantctx"
	"github.com/obot-platform/discobox/orchestration"
)

type tenantJobManager struct {
	rootCtx context.Context
	store   *store.Store
	svc     *service.Service
	opts    DatabaseRouterOptions

	mu          sync.Mutex
	dispatchers map[string]*orchestration.Dispatcher
}

func newTenantJobManager(ctx context.Context, store *store.Store, opts DatabaseRouterOptions) *tenantJobManager {
	return &tenantJobManager{rootCtx: ctx, store: store, opts: opts, dispatchers: map[string]*orchestration.Dispatcher{}}
}

func (m *tenantJobManager) SetService(svc *service.Service) {
	m.svc = svc
}

func (m *tenantJobManager) EnsureStarted(ctx context.Context) error {
	if m == nil || !m.opts.DispatcherEnabled {
		return nil
	}
	tenantID, err := tenantctx.TenantID(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dispatchers[tenantID] != nil {
		return nil
	}
	dispatcher := orchestration.NewDispatcher(m.store, orchestration.DispatcherConfig{
		SingleNode:         true,
		PollInterval:       m.opts.DispatcherPollInterval,
		JobTimeout:         m.opts.DispatcherJobTimeout,
		StaleJobTimeout:    m.opts.DispatcherStaleJobTimeout,
		ImmediateExecution: m.opts.DispatcherImmediateExecution,
		DefaultConcurrency: m.opts.DispatcherDefaultConcurrency,
		JobContext: func(ctx context.Context, job *orchestration.Job) context.Context {
			if job.TenantID == "" {
				return ctx
			}
			return tenantctx.WithTenantID(ctx, job.TenantID)
		},
	})
	sandboxReconciler := m.svc.NewSandboxReconciler()
	if err := dispatcher.Register(jobs.NewSandboxReconcileExecutor(sandboxReconciler), orchestration.WithConcurrency(m.opts.SandboxReconcileJobConcurrency)); err != nil {
		return err
	}
	workerReconciler := m.svc.NewWorkerReconciler()
	if err := dispatcher.Register(jobs.NewWorkerReconcileExecutor(workerReconciler), orchestration.WithConcurrency(m.opts.SandboxReconcileJobConcurrency)); err != nil {
		return err
	}
	dispatcherCtx := tenantctx.WithTenantID(m.rootCtx, tenantID)
	if err := dispatcher.Start(dispatcherCtx); err != nil {
		return err
	}
	if err := m.svc.EnsureExistingSandboxProviderInstances(dispatcherCtx); err != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(dispatcherCtx), 10*time.Second)
		defer cancel()
		_ = dispatcher.DrainAndStop(stopCtx)
		return err
	}
	m.dispatchers[tenantID] = dispatcher
	return nil
}

func (m *tenantJobManager) NotifyNewJob(ctx context.Context) {
	if m == nil || !m.opts.DispatcherEnabled {
		return
	}
	if err := m.EnsureStarted(ctx); err != nil {
		return
	}
	tenantID, err := tenantctx.TenantID(ctx)
	if err != nil {
		return
	}
	m.mu.Lock()
	dispatcher := m.dispatchers[tenantID]
	m.mu.Unlock()
	if dispatcher != nil {
		dispatcher.NotifyNewJob()
	}
}
