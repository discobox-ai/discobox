package server

import (
	"context"
	"sync"

	"github.com/obot-platform/disco2/internal/jobs"
	"github.com/obot-platform/disco2/internal/service"
	"github.com/obot-platform/disco2/internal/store"
	"github.com/obot-platform/disco2/internal/tenantctx"
	"github.com/obot-platform/disco2/jobqueue"
)

type tenantJobManager struct {
	rootCtx context.Context
	store   *store.Store
	svc     *service.Service
	opts    DatabaseRouterOptions

	mu          sync.Mutex
	dispatchers map[string]*jobqueue.Dispatcher
}

func newTenantJobManager(ctx context.Context, store *store.Store, opts DatabaseRouterOptions) *tenantJobManager {
	return &tenantJobManager{rootCtx: ctx, store: store, opts: opts, dispatchers: map[string]*jobqueue.Dispatcher{}}
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
	dispatcher := jobqueue.NewDispatcher(m.store, jobqueue.DispatcherConfig{
		SingleNode:         true,
		PollInterval:       m.opts.DispatcherPollInterval,
		JobTimeout:         m.opts.DispatcherJobTimeout,
		StaleJobTimeout:    m.opts.DispatcherStaleJobTimeout,
		ImmediateExecution: m.opts.DispatcherImmediateExecution,
		DefaultConcurrency: m.opts.DispatcherDefaultConcurrency,
		JobContext: func(ctx context.Context, job *jobqueue.Job) context.Context {
			if job.TenantID == "" {
				return ctx
			}
			return tenantctx.WithTenantID(ctx, job.TenantID)
		},
	})
	sandboxReconciler := m.svc.NewSandboxReconciler()
	if err := dispatcher.Register(jobs.NewSandboxReconcileExecutor(sandboxReconciler), jobqueue.WithConcurrency(m.opts.SandboxReconcileJobConcurrency)); err != nil {
		return err
	}
	dispatcherCtx := tenantctx.WithTenantID(m.rootCtx, tenantID)
	if err := dispatcher.Start(dispatcherCtx); err != nil {
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
