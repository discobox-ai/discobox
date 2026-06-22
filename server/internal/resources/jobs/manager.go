package jobs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/store"
)

type ManagerConfig struct {
	Enabled            bool
	QueueConfig        orchestration.QueueConfig
	PollInterval       time.Duration
	JobTimeout         time.Duration
	StaleJobTimeout    time.Duration
	ImmediateExecution bool
	DefaultConcurrency int
}

type jobRegistration struct {
	jobType  orchestration.Type
	executor orchestration.Executor
	options  []orchestration.ExecutorOption
}

type Manager struct {
	rootCtx context.Context
	store   *store.Store
	cfg     ManagerConfig

	mu            sync.Mutex
	registrations []jobRegistration
	dispatcher    *orchestration.Dispatcher
}

func NewManager(ctx context.Context, appStore *store.Store, cfg ManagerConfig) *Manager {
	return &Manager{rootCtx: ctx, store: appStore, cfg: cfg}
}

func (m *Manager) Start(_ context.Context) error {
	if m == nil || !m.cfg.Enabled {
		return nil
	}
	_, _, err := m.startDispatcher()
	return err
}

func (m *Manager) Stop(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	dispatcher := m.dispatcher
	m.dispatcher = nil
	m.mu.Unlock()
	if dispatcher == nil {
		return nil
	}
	return dispatcher.DrainAndStop(ctx)
}

func (m *Manager) Register(jobType orchestration.Type, executor orchestration.Executor, opts ...orchestration.ExecutorOption) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dispatcher != nil {
		return errors.New("cannot register job executor after dispatcher start")
	}
	options := append([]orchestration.ExecutorOption(nil), opts...)
	m.registrations = append(m.registrations, jobRegistration{jobType: jobType, executor: executor, options: options})
	return nil
}

func (m *Manager) startDispatcher() (*orchestration.Dispatcher, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dispatcher != nil {
		return m.dispatcher, false, nil
	}
	dispatcher := orchestration.NewDispatcher(m.store, orchestration.DispatcherConfig{
		SingleNode:         true,
		PollInterval:       m.cfg.PollInterval,
		JobTimeout:         m.cfg.JobTimeout,
		StaleJobTimeout:    m.cfg.StaleJobTimeout,
		ImmediateExecution: m.cfg.ImmediateExecution,
		DefaultConcurrency: m.cfg.DefaultConcurrency,
	})
	for _, registration := range m.registrations {
		if err := dispatcher.Register(registration.jobType, registration.executor, registration.options...); err != nil {
			return nil, false, err
		}
	}
	if err := dispatcher.Start(m.rootCtx); err != nil {
		return nil, false, err
	}
	m.dispatcher = dispatcher
	return dispatcher, true, nil
}

func (m *Manager) NotifyNewJob(context.Context) {
	if m == nil || !m.cfg.Enabled {
		return
	}
	m.mu.Lock()
	dispatcher := m.dispatcher
	m.mu.Unlock()
	if dispatcher != nil {
		dispatcher.NotifyNewJob()
	}
}

func (m *Manager) dispatcherForSubmit() (*orchestration.Dispatcher, error) {
	if m == nil {
		return nil, errors.New("job manager is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dispatcher == nil {
		if !m.cfg.Enabled {
			dispatcher, err := m.newDispatcherLocked()
			if err != nil {
				return nil, err
			}
			m.dispatcher = dispatcher
			return dispatcher, nil
		}
		return nil, errors.New("job manager is not started")
	}
	return m.dispatcher, nil
}

func (m *Manager) newDispatcherLocked() (*orchestration.Dispatcher, error) {
	dispatcher := orchestration.NewDispatcher(m.store, orchestration.DispatcherConfig{
		SingleNode:         true,
		PollInterval:       m.cfg.PollInterval,
		JobTimeout:         m.cfg.JobTimeout,
		StaleJobTimeout:    m.cfg.StaleJobTimeout,
		ImmediateExecution: m.cfg.ImmediateExecution,
		DefaultConcurrency: m.cfg.DefaultConcurrency,
	})
	for _, registration := range m.registrations {
		if err := dispatcher.Register(registration.jobType, registration.executor, registration.options...); err != nil {
			return nil, err
		}
	}
	return dispatcher, nil
}
