// Package server wires and runs the HTTP server.
package server

import (
	"context"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/obot-platform/discobox/server/internal/auth"
	poolagentauth "github.com/obot-platform/discobox/server/internal/auth/poolagent"
	sandboxauth "github.com/obot-platform/discobox/server/internal/auth/sandbox"
	"github.com/obot-platform/discobox/server/internal/events"
	"github.com/obot-platform/discobox/server/internal/handlers"
	"github.com/obot-platform/discobox/server/internal/projectstream"
	"github.com/obot-platform/discobox/server/internal/reconcile"
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

	SecretSealer secrets.Sealer

	// DispatcherPollInterval is how often the reconcile engine looks for
	// claimable dirty rows beyond in-process wakeups.
	DispatcherPollInterval time.Duration

	SandboxReconcileJobConcurrency int
	DefaultSandboxImage            string
	// HostID identifies the machine this server runs on; see config.Config.
	HostID string

	// HarnessImages overrides built-in harness definition images, keyed by
	// definition ID (dev builds inject freshly tagged images this way).
	HarnessImages map[string]string
}

// DefaultAppOptions returns the production defaults for the app.
func DefaultAppOptions() AppOptions {
	return AppOptions{
		DispatcherPollInterval:         time.Second,
		SandboxReconcileJobConcurrency: 4,
		DefaultSandboxImage:            sandbox.DefaultSandboxImageName,
	}
}

// NewRouter creates a chi router backed by the generated OpenAPI server.
func NewRouter(svc services.Services) (*chi.Mux, error) {
	router := chi.NewRouter()
	RegisterHealthRoutes(router)
	RegisterDocsRoutes(router)
	registerProjectStreamTransports(router, svc.Events)
	registerSandboxGitRoutes(router, svc.Sandboxes)
	registerSandboxHTTPRoutes(router, svc.Sandboxes)
	registerSandboxAgentTerminalRoutes(router, svc.Sandboxes)
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
	reconcileEngine, err := reconcile.New(writeDB, reconcile.Options{
		// This deployment is single-process; the engine also supports
		// lease-based multi-node claiming — flip this when scaling out.
		SingleNode:   true,
		PollInterval: opts.DispatcherPollInterval,
	})
	if err != nil {
		return nil, err
	}
	appServices := service.New(appStore, reconcileEngine, service.JobManagerOptions{
		SandboxReconcileJobConcurrency: opts.SandboxReconcileJobConcurrency,
	}, broker)
	appServices.SetDefaultSandboxImage(opts.DefaultSandboxImage)
	appServices.SetHostID(opts.HostID)
	appServices.SetHarnessImages(opts.HarnessImages)
	if opts.SecretSealer != nil {
		appServices.SetSandboxAuthManager(sandboxauth.NewManager(appStore, opts.SecretSealer))
	}
	appServices.SetWorkerAgentAuthManager(poolagentauth.NewManager(appStore, opts.SecretSealer))
	if err := appServices.InitializeDefaults(ctx, opts.UserID); err != nil {
		return nil, err
	}
	if err := appServices.Start(ctx); err != nil {
		return nil, err
	}
	router := chi.NewRouter()
	router.Use(auth.Authentication(
		auth.PoolAuthenticator{Store: appStore},
		auth.DefaultUserAuthenticator{UserID: opts.UserID},
	))
	router.Use(auth.Authorization(
		auth.ProjectAuthorizer{Store: appStore},
		auth.PoolRouteAuthorizer{},
		auth.AuthenticatedAuthorizer{},
	))
	RegisterHealthRoutes(router)
	RegisterDocsRoutes(router)
	registerProjectStreamTransports(router, appServices)
	registerSandboxGitRoutes(router, appServices)
	registerSandboxHTTPRoutes(router, appServices)
	registerSandboxAgentTerminalRoutes(router, appServices)
	generated, err := handlers.NewServer(services.Services{
		Projects:       appServices,
		HarnessConfigs: appServices,
		Sandboxes:      appServices,
		Providers:      appServices,
		Pools:          appServices,
		Jobs:           appServices,
		Events:         appServices,
		Secrets:        appServices,
	})
	if err != nil {
		return nil, err
	}
	router.Mount("/", generated)
	return router, nil
}
