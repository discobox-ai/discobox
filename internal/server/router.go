// Package server wires and runs the HTTP server.
package server

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/obot-platform/discobox/internal/api"
	"github.com/obot-platform/discobox/internal/database"
	"github.com/obot-platform/discobox/internal/events"
	"github.com/obot-platform/discobox/internal/realtime"
	"github.com/obot-platform/discobox/internal/sandboxauth"
	"github.com/obot-platform/discobox/internal/secrets"
	"github.com/obot-platform/discobox/internal/server/defaults"
	"github.com/obot-platform/discobox/internal/server/middleware"
	"github.com/obot-platform/discobox/internal/service"
	"github.com/obot-platform/discobox/internal/store"
	"github.com/obot-platform/discobox/internal/tenantctx"
	"github.com/obot-platform/discobox/orchestration"
)

const (
	Name    = "Discobox Sandbox Manager"
	Version = "0.1.0"
)

// DatabaseRouterOptions controls database-backed router wiring.
type DatabaseRouterOptions struct {
	TenantID string
	UserID   string

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
	realtime.RegisterProjectStreamRoutes(router, services.Events)
	config := huma.DefaultConfig(Name, Version)
	config.DocsRenderer = huma.DocsRendererScalar
	humaAPI := humachi.New(router, config)
	realtime.RegisterProjectStreamSSEOperations(humaAPI, services.Events)
	api.Register(humaAPI, services)
	return router, humaAPI
}

// NewStubbedRouter creates a router backed by in-memory stub services.
func NewStubbedRouter() (*chi.Mux, huma.API) {
	stubs := service.NewStub()
	return NewRouter(api.Services{
		Projects:     stubs,
		AgentConfigs: stubs,
		Sandboxes:    stubs,
		Providers:    stubs,
		Workers:      stubs,
		Events:       stubs,
	})
}

// NewDatabaseRouter creates a router backed by the database service.
func NewDatabaseRouter(ctx context.Context, resolver *database.Resolver, options ...DatabaseRouterOptions) (*chi.Mux, huma.API, error) {
	opts := DefaultDatabaseRouterOptions()
	if len(options) > 0 {
		opts = options[0]
	}

	if opts.UserID == "" {
		opts.UserID = service.DefaultUserID
	}
	if opts.TenantID == "" {
		opts.TenantID = service.DefaultTenantID
	}
	broker := events.NewBroker()
	appStore := store.New(resolver, store.WithPublisher(broker), store.WithSealer(opts.SecretSealer), store.WithDefaultTenantID(opts.TenantID))
	queueConfig := orchestration.QueueConfig{
		DefaultMaxAttempts: opts.JobMaxAttempts,
	}
	tenantJobs := newTenantJobManager(ctx, appStore, opts)
	notifyNewJob := tenantJobs.NotifyNewJob
	services := service.New(appStore, queueConfig, notifyNewJob, broker)
	tenantJobs.SetService(services)
	if opts.SecretSealer != nil {
		services.SetSandboxAuthManager(sandboxauth.NewManager(appStore, opts.SecretSealer))
	}
	if err := defaults.InitializeIdentity(ctx, resolver, opts.TenantID, opts.UserID); err != nil {
		return nil, nil, err
	}
	initCtx := tenantctx.WithTenantID(ctx, opts.TenantID)
	if err := services.InitializeDefaults(initCtx, opts.TenantID, opts.UserID); err != nil {
		return nil, nil, err
	}
	router := chi.NewRouter()
	router.Use(middleware.Authentication(
		middleware.WorkerAuthenticator{},
		middleware.DefaultUserAuthenticator{TenantID: opts.TenantID, UserID: opts.UserID},
	))
	router.Use(middleware.Tenant(tenantJobs, opts.TenantID))
	router.Use(middleware.ProjectAuthorization(appStore))
	router.Use(middleware.GenericAuthorization)
	config := huma.DefaultConfig(Name, Version)
	config.DocsRenderer = huma.DocsRendererScalar
	humaAPI := humachi.New(router, config)
	realtime.RegisterProjectStreamRoutes(router, services)
	realtime.RegisterProjectStreamSSEOperations(humaAPI, services)
	api.Register(humaAPI, api.Services{
		Projects:     services,
		AgentConfigs: services,
		Sandboxes:    services,
		Providers:    services,
		Workers:      services,
		Events:       services,
	})
	return router, humaAPI, nil
}
