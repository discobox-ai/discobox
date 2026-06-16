package server

import (
	"context"
	"sync"
	"time"

	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/sandbox/jobs"
	"github.com/obot-platform/discobox/server/internal/service"
	"github.com/obot-platform/discobox/server/internal/store"
)

type jobManager struct {
	rootCtx context.Context
	store   *store.Store
	svc     *service.Service
	opts    ApplicationRouterOptions

	mu         sync.Mutex
	dispatcher *orchestration.Dispatcher
}

func newJobManager(ctx context.Context, store *store.Store, opts ApplicationRouterOptions) *jobManager {
	return &jobManager{rootCtx: ctx, store: store, opts: opts}
}

func (m *jobManager) SetService(svc *service.Service) {
	m.svc = svc
}

func (m *jobManager) EnsureStarted(ctx context.Context) error {
	if m == nil || !m.opts.DispatcherEnabled {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dispatcher != nil {
		return nil
	}
	dispatcher := orchestration.NewDispatcher(m.store, orchestration.DispatcherConfig{
		SingleNode:         true,
		PollInterval:       m.opts.DispatcherPollInterval,
		JobTimeout:         m.opts.DispatcherJobTimeout,
		StaleJobTimeout:    m.opts.DispatcherStaleJobTimeout,
		ImmediateExecution: m.opts.DispatcherImmediateExecution,
		DefaultConcurrency: m.opts.DispatcherDefaultConcurrency,
	})
	sandboxReconciler := m.svc.NewSandboxReconciler()
	if err := dispatcher.Register(jobs.NewSandboxReconcileExecutor(sandboxReconciler), orchestration.WithConcurrency(m.opts.SandboxReconcileJobConcurrency)); err != nil {
		return err
	}
	workerReconciler := m.svc.NewWorkerReconciler()
	if err := dispatcher.Register(jobs.NewWorkerReconcileExecutor(workerReconciler), orchestration.WithConcurrency(m.opts.SandboxReconcileJobConcurrency)); err != nil {
		return err
	}
	if err := dispatcher.Start(m.rootCtx); err != nil {
		return err
	}
	m.dispatcher = dispatcher
	if err := m.svc.EnsureExistingSandboxProviderInstances(ctx); err != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = dispatcher.DrainAndStop(stopCtx)
		m.dispatcher = nil
		return err
	}
	return nil
}

func (m *jobManager) NotifyNewJob(ctx context.Context) {
	if m == nil || !m.opts.DispatcherEnabled {
		return
	}
	if err := m.EnsureStarted(ctx); err != nil {
		return
	}
	m.mu.Lock()
	dispatcher := m.dispatcher
	m.mu.Unlock()
	if dispatcher != nil {
		dispatcher.NotifyNewJob()
	}
}
