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

func (m *jobManager) Start(ctx context.Context) error {
	if m == nil || !m.opts.DispatcherEnabled {
		return nil
	}
	dispatcher, started, err := m.startDispatcher()
	if err != nil {
		return err
	}
	if !started {
		return nil
	}
	if err := m.svc.EnsureExistingSandboxProviderInstances(ctx); err != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = dispatcher.DrainAndStop(stopCtx)
		m.mu.Lock()
		if m.dispatcher == dispatcher {
			m.dispatcher = nil
		}
		m.mu.Unlock()
		return err
	}
	return nil
}

func (m *jobManager) startDispatcher() (*orchestration.Dispatcher, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dispatcher != nil {
		return m.dispatcher, false, nil
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
		return nil, false, err
	}
	if err := dispatcher.Register(jobs.NewProviderReconcileExecutor(m.svc), orchestration.WithConcurrency(m.opts.SandboxReconcileJobConcurrency)); err != nil {
		return nil, false, err
	}
	workerReconciler := m.svc.NewWorkerReconciler()
	if err := dispatcher.Register(jobs.NewWorkerReconcileExecutor(workerReconciler, m.svc.ProviderSubmitter()), orchestration.WithConcurrency(m.opts.SandboxReconcileJobConcurrency)); err != nil {
		return nil, false, err
	}
	if err := dispatcher.Start(m.rootCtx); err != nil {
		return nil, false, err
	}
	m.dispatcher = dispatcher
	return dispatcher, true, nil
}

func (m *jobManager) NotifyNewJob(context.Context) {
	if m == nil || !m.opts.DispatcherEnabled {
		return
	}
	m.mu.Lock()
	dispatcher := m.dispatcher
	m.mu.Unlock()
	if dispatcher != nil {
		dispatcher.NotifyNewJob()
	}
}
