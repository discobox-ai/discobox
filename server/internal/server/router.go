// Package server wires and runs the HTTP server.
package server

import (
	"context"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/auth"
	sandboxauth "github.com/obot-platform/discobox/server/internal/auth/sandbox"
	workeragentauth "github.com/obot-platform/discobox/server/internal/auth/workeragent"
	"github.com/obot-platform/discobox/server/internal/events"
	"github.com/obot-platform/discobox/server/internal/handlers"
	"github.com/obot-platform/discobox/server/internal/projectstream"
	sandboxjobs "github.com/obot-platform/discobox/server/internal/resources/jobs"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/secrets"
	"github.com/obot-platform/discobox/server/internal/service"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
	"gorm.io/gorm"
)

const (
	Name    = "Discobox Sandbox Manager"
	Version = "0.1.0"
)

// AppOptions controls application wiring.
type AppOptions struct {
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
	DefaultSandboxImage            string
}

// DefaultAppOptions returns the production defaults for the app.
func DefaultAppOptions() AppOptions {
	return AppOptions{
		JobMaxAttempts:                 3,
		DispatcherEnabled:              true,
		DispatcherPollInterval:         time.Second,
		DispatcherJobTimeout:           time.Minute,
		DispatcherStaleJobTimeout:      5 * time.Minute,
		DispatcherImmediateExecution:   true,
		DispatcherDefaultConcurrency:   1,
		SandboxReconcileJobConcurrency: 4,
		DefaultSandboxImage:            sandbox.DefaultSandboxImageName,
	}
}

// NewRouter creates a chi router backed by the generated OpenAPI server.
func NewRouter(svc services.Services) (*chi.Mux, error) {
	router := chi.NewRouter()
	RegisterDocsRoutes(router)
	registerProjectStreamTransports(router, svc.Events)
	registerSandboxGitRoutes(router, svc.Sandboxes)
	registerSandboxHTTPRoutes(router, svc.Sandboxes)
	generated, err := handlers.NewServer(svc)
	if err != nil {
		return nil, err
	}
	router.Mount("/", generated)
	return router, nil
}

func registerProjectStreamTransports(router chi.Router, service services.ProjectEventService) {
	projectstream.RegisterProjectStreamRoutes(router, service)
	projectstream.RegisterProjectStreamSSERoutes(router, service)
}

// NewApp creates the app backed by persistent services.
func NewApp(ctx context.Context, writeDB, readDB *gorm.DB, options ...AppOptions) (*chi.Mux, error) {
	opts := DefaultAppOptions()
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
	jobManager := sandboxjobs.NewManager(ctx, appStore, sandboxjobs.ManagerConfig{
		Enabled:            opts.DispatcherEnabled,
		QueueConfig:        queueConfig,
		PollInterval:       opts.DispatcherPollInterval,
		JobTimeout:         opts.DispatcherJobTimeout,
		StaleJobTimeout:    opts.DispatcherStaleJobTimeout,
		ImmediateExecution: opts.DispatcherImmediateExecution,
		DefaultConcurrency: opts.DispatcherDefaultConcurrency,
	})
	appServices := service.New(appStore, jobManager, service.JobManagerOptions{
		SandboxReconcileJobConcurrency: opts.SandboxReconcileJobConcurrency,
	}, broker)
	appServices.SetDefaultSandboxImage(opts.DefaultSandboxImage)
	if opts.SecretSealer != nil {
		appServices.SetSandboxAuthManager(sandboxauth.NewManager(appStore, opts.SecretSealer))
	}
	appServices.SetWorkerAgentAuthManager(workeragentauth.NewManager(appStore, opts.SecretSealer))
	if err := appServices.InitializeDefaults(ctx, opts.UserID); err != nil {
		return nil, err
	}
	if err := appServices.Start(ctx); err != nil {
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
	registerProjectStreamTransports(router, appServices)
	registerSandboxGitRoutes(router, appServices)
	registerSandboxHTTPRoutes(router, appServices)
	generated, err := handlers.NewServer(services.Services{
		Projects:     appServices,
		AgentConfigs: appServices,
		Sandboxes:    appServices,
		Providers:    appServices,
		Workers:      appServices,
		Jobs:         appServices,
		Events:       appServices,
	})
	if err != nil {
		return nil, err
	}
	router.Mount("/", generated)
	return router, nil
}
