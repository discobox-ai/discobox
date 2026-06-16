// Package server wires and runs the HTTP server.
package server

import (
	"context"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/api"
	"github.com/obot-platform/discobox/server/internal/auth"
	"github.com/obot-platform/discobox/server/internal/events"
	"github.com/obot-platform/discobox/server/internal/generatedapi"
	"github.com/obot-platform/discobox/server/internal/projectstream"
	"github.com/obot-platform/discobox/server/internal/sandboxauth"
	"github.com/obot-platform/discobox/server/internal/secrets"
	"github.com/obot-platform/discobox/server/internal/service"
	"github.com/obot-platform/discobox/server/internal/store"
	"gorm.io/gorm"
)

const (
	Name    = "Discobox Sandbox Manager"
	Version = "0.1.0"
)

// ApplicationRouterOptions controls application router wiring.
type ApplicationRouterOptions struct {
	UserID string

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

// DefaultApplicationRouterOptions returns the production defaults for the
// application router.
func DefaultApplicationRouterOptions() ApplicationRouterOptions {
	return ApplicationRouterOptions{
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

// NewRouter creates a chi router backed by the generated OpenAPI server.
func NewRouter(services api.Services) (*chi.Mux, error) {
	router := chi.NewRouter()
	RegisterDocsRoutes(router)
	registerProjectStreamTransports(router, services.Events)
	generated, err := generatedapi.NewServer(services)
	if err != nil {
		return nil, err
	}
	router.Mount("/", generated)
	return router, nil
}

// NewApplicationRouter creates the application router backed by persistent services.

func registerProjectStreamTransports(router chi.Router, service api.ProjectEventService) {
	projectstream.RegisterProjectStreamRoutes(router, service)
	projectstream.RegisterProjectStreamSSERoutes(router, service)
}

func NewApplicationRouter(ctx context.Context, writeDB, readDB *gorm.DB, options ...ApplicationRouterOptions) (*chi.Mux, error) {
	opts := DefaultApplicationRouterOptions()
	if len(options) > 0 {
		opts = options[0]
	}

	if opts.UserID == "" {
		opts.UserID = service.DefaultUserID
	}
	broker := events.NewBroker()
	appStore := store.New(writeDB, readDB, store.WithPublisher(broker), store.WithSealer(opts.SecretSealer))
	queueConfig := orchestration.QueueConfig{
		DefaultMaxAttempts: opts.JobMaxAttempts,
	}
	jobs := newJobManager(ctx, appStore, opts)
	notifyNewJob := jobs.NotifyNewJob
	services := service.New(appStore, queueConfig, notifyNewJob, broker)
	jobs.SetService(services)
	if opts.SecretSealer != nil {
		services.SetSandboxAuthManager(sandboxauth.NewManager(appStore, opts.SecretSealer))
	}
	if err := services.InitializeDefaults(ctx, opts.UserID); err != nil {
		return nil, err
	}
	if err := jobs.Start(ctx); err != nil {
		return nil, err
	}
	router := chi.NewRouter()
	router.Use(auth.Authentication(
		auth.WorkerAuthenticator{Store: appStore},
		auth.DefaultUserAuthenticator{UserID: opts.UserID},
	))
	router.Use(auth.Authorization(
		auth.ProjectAuthorizer{Store: appStore},
		auth.WorkerRouteAuthorizer{},
		auth.AuthenticatedAuthorizer{},
	))
	RegisterDocsRoutes(router)
	registerProjectStreamTransports(router, services)
	generated, err := generatedapi.NewServer(api.Services{
		Projects:     services,
		AgentConfigs: services,
		Sandboxes:    services,
		Providers:    services,
		Workers:      services,
		Events:       services,
	})
	if err != nil {
		return nil, err
	}
	router.Mount("/", generated)
	return router, nil
}
