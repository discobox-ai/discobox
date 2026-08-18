// Package server wires and runs the HTTP server.
package server

import (
	"context"
	"fmt"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/obot-platform/discobox/devimage"
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
	"github.com/obot-platform/discobox/server/internal/transport/carrierhub"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
	"gorm.io/gorm"
)

const (
	Name    = "Discobox Sandbox Manager"
	Version = "0.1.0"
)

// AppOptions controls application wiring.
type AppOptions struct {
	UserID string

	// SSHIngress is what GET /ssh serves: the endpoint SSH clients should dial
	// and the host key to pin. It is resolved by the caller because the
	// advertised address is configuration and the host key is loaded from the
	// data directory, neither of which this constructor owns.
	SSHIngress services.SSHIngress

	SecretSealer secrets.Sealer

	// DispatcherPollInterval is how often the reconcile engine looks for
	// claimable dirty rows beyond in-process wakeups.
	DispatcherPollInterval time.Duration

	SandboxReconcileJobConcurrency int
	DefaultSandboxImage            string
	// DefaultSandboxImageDigest identifies which build DefaultSandboxImage's
	// tag currently is, so a sandbox running the default image can tell whether
	// it is already on it. See config.Config.
	DefaultSandboxImageDigest string
	// HostID identifies the machine this server runs on; see config.Config.
	HostID string

	// HarnessImages overrides built-in harness definition images, keyed by
	// definition ID (dev builds inject freshly tagged images this way).
	HarnessImages map[string]string
	// DevelopmentImages is the watcher-built image set synchronized to every
	// Docker daemon used by a pool provider.
	DevelopmentImages []devimage.Image

	// ControlPlaneStreams receives control-plane connections opened by pool
	// guests whose transport cannot be dialed inward. The caller owns it because
	// it must also be served (see Serve); when nil such backends still start but
	// their agents cannot register.
	ControlPlaneStreams *carrierhub.Hub

	// ListenEndpoints are the endpoints this server is listening on, in
	// config.Config.Listen form. A pool backend that dials the control plane
	// inward derives its address from them, so it offers the agent a transport
	// the server actually answers on rather than assuming one.
	ListenEndpoints []string
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
	registerSandboxTCPRoutes(router, svc.Sandboxes)
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

// NewApp creates the app backed by persistent services. Alongside the router
// it returns the assembled services and store so an in-process, non-HTTP
// caller — the SSH ingress (ADR 0024) — can drive the same
// AcquireSandboxHTTPClient choke point and username resolution an HTTP
// caller would, without a wrapper or a second service assembly.
func NewApp(ctx context.Context, writeDB, readDB *gorm.DB, options ...AppOptions) (*chi.Mux, services.Services, *store.Store, error) {
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
		return nil, services.Services{}, nil, err
	}
	developmentImageSync, err := dockerworker.NewDevelopmentImageSynchronizer(opts.DevelopmentImages)
	if err != nil {
		return nil, services.Services{}, nil, fmt.Errorf("configure development image synchronization: %w", err)
	}
	// Pool backends that cannot be dialed inward hand their guest-initiated
	// control-plane connections to this hub; Serve treats it as one more
	// listener for the ordinary handler, so routing and authentication are
	// unchanged.
	controlPlaneStreams := opts.ControlPlaneStreams
	appServices := service.New(appStore, reconcileEngine, service.Options{
		SandboxReconcileJobConcurrency: opts.SandboxReconcileJobConcurrency,
		DevelopmentImageSync:           developmentImageSync,
		DevelopmentImages:              opts.DevelopmentImages,
		ControlPlaneStreams:            controlPlaneStreams,
		ListenEndpoints:                opts.ListenEndpoints,
	}, broker)
	appServices.SetDefaultSandboxImage(opts.DefaultSandboxImage, opts.DefaultSandboxImageDigest)
	appServices.SetHostID(opts.HostID)
	appServices.SetHarnessImages(opts.HarnessImages)
	if opts.SecretSealer != nil {
		appServices.SetSandboxAuthManager(sandboxauth.NewManager(appStore, opts.SecretSealer))
	}
	appServices.SetWorkerAgentAuthManager(poolagentauth.NewManager(appStore, opts.SecretSealer))
	if _, err := appServices.InitializeDefaults(ctx, opts.UserID); err != nil {
		return nil, services.Services{}, nil, err
	}
	if err := appServices.Start(ctx); err != nil {
		return nil, services.Services{}, nil, err
	}
	svc := services.Services{
		SSH:            opts.SSHIngress,
		Projects:       appServices,
		HarnessConfigs: appServices,
		Sandboxes:      appServices,
		Providers:      appServices,
		Pools:          appServices,
		Jobs:           appServices,
		Events:         appServices,
		Secrets:        appServices,
		SSHKeys:        appServices,
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
	registerSandboxTCPRoutes(router, appServices)
	generated, err := handlers.NewServer(svc)
	if err != nil {
		return nil, services.Services{}, nil, err
	}
	router.Mount("/", generated)
	return router, svc, appStore, nil
}
