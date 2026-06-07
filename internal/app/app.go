// Package app wires the HTTP router and API operations.
package app

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/obot-platform/disco2/internal/api"
	"github.com/obot-platform/disco2/internal/database"
	"github.com/obot-platform/disco2/internal/events"
	"github.com/obot-platform/disco2/internal/jobs"
	"github.com/obot-platform/disco2/internal/orchestration"
	"github.com/obot-platform/disco2/internal/sandboxauth"
	"github.com/obot-platform/disco2/internal/secrets"
	"github.com/obot-platform/disco2/internal/service"
	"github.com/obot-platform/disco2/internal/store"
	"github.com/obot-platform/disco2/jobqueue"
	"github.com/obot-platform/disco2/jobqueue/gormstore"
)

const (
	Name    = "Disco2 Sandbox Manager"
	Version = "0.1.0"
)

// DatabaseRouterOptions controls database-backed router wiring.
type DatabaseRouterOptions struct {
	JobMaxAttempts int

	SecretSealer secrets.Sealer

	DispatcherEnabled            bool
	DispatcherPollInterval       time.Duration
	DispatcherJobTimeout         time.Duration
	DispatcherStaleJobTimeout    time.Duration
	DispatcherImmediateExecution bool
	DispatcherDefaultConcurrency int

	SandboxReconcileJobConcurrency int
}

// DefaultDatabaseRouterOptions returns the production defaults for the
// database-backed app.
func DefaultDatabaseRouterOptions() DatabaseRouterOptions {
	return DatabaseRouterOptions{
		JobMaxAttempts:                 3,
		DispatcherEnabled:              true,
		DispatcherPollInterval:         time.Second,
		DispatcherJobTimeout:           time.Minute,
		DispatcherStaleJobTimeout:      5 * time.Minute,
		DispatcherImmediateExecution:   true,
		DispatcherDefaultConcurrency:   1,
		SandboxReconcileJobConcurrency: 4,
	}
}

// NewRouter creates a chi router with all Huma operations registered.
func NewRouter(services api.Services) (*chi.Mux, huma.API) {
	router := chi.NewRouter()
	config := huma.DefaultConfig(Name, Version)
	config.DocsRenderer = huma.DocsRendererSwaggerUI
	humaAPI := humachi.New(router, config)
	api.Register(humaAPI, services)
	return router, humaAPI
}

// NewStubbedRouter creates a router backed by in-memory stub services.
func NewStubbedRouter() (*chi.Mux, huma.API) {
	stubs := service.NewStub()
	return NewRouter(api.Services{
		Projects:  stubs,
		Sandboxes: stubs,
		Events:    stubs,
	})
}

// NewDatabaseRouter creates a router backed by the database service.
func NewDatabaseRouter(ctx context.Context, db *database.DB, options ...DatabaseRouterOptions) (*chi.Mux, huma.API, error) {
	opts := DefaultDatabaseRouterOptions()
	if len(options) > 0 {
		opts = options[0]
	}

	broker := events.NewBroker()
	appStore := store.New(db.Write, db.Read, broker).WithSealer(opts.SecretSealer)
	jobStore := gormstore.New(db.Write, db.Read)
	queueConfig := jobqueue.QueueConfig{
		DefaultMaxAttempts: opts.JobMaxAttempts,
	}
	var notifyNewJob func()
	var dispatcher *jobqueue.Dispatcher
	if opts.DispatcherEnabled {
		dispatcher = jobqueue.NewDispatcher(jobStore, jobqueue.DispatcherConfig{
			SingleNode:         true,
			PollInterval:       opts.DispatcherPollInterval,
			JobTimeout:         opts.DispatcherJobTimeout,
			StaleJobTimeout:    opts.DispatcherStaleJobTimeout,
			ImmediateExecution: opts.DispatcherImmediateExecution,
			DefaultConcurrency: opts.DispatcherDefaultConcurrency,
		})
		notifyNewJob = dispatcher.NotifyNewJob
	}
	ensureJob := func(ctx context.Context, txDB *gorm.DB, payload jobqueue.Payload) (*jobqueue.Job, bool, error) {
		return gormstore.New(txDB, txDB).EnsureActiveJobForPayload(ctx, payload, queueConfig)
	}
	services := service.New(appStore, orchestration.New(appStore, ensureJob, notifyNewJob), broker)
	if opts.SecretSealer != nil {
		services.SetSandboxAuthManager(sandboxauth.NewManager(appStore, opts.SecretSealer))
	}
	if dispatcher != nil {
		sandboxReconciler := service.NewSandboxReconciler(appStore, services.NewSandboxOperations())
		if err := dispatcher.Register(jobs.NewSandboxReconcileExecutor(sandboxReconciler), jobqueue.WithConcurrency(opts.SandboxReconcileJobConcurrency)); err != nil {
			return nil, nil, err
		}
	}

	if err := services.InitializeDefaults(ctx); err != nil {
		return nil, nil, err
	}
	if dispatcher != nil {
		if err := dispatcher.Start(ctx); err != nil {
			return nil, nil, err
		}
	}
	router, api := NewRouter(api.Services{
		Projects:  services,
		Sandboxes: services,
		Events:    services,
	})
	return router, api, nil
}
