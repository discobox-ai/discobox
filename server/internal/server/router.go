// Package server wires and runs the HTTP server.
package server

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"time"

	"github.com/discobox-ai/discobox/server/internal/devbundles"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/pkg/devimage"
	"github.com/discobox-ai/discobox/server/internal/auth"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	sandboxauth "github.com/discobox-ai/discobox/server/internal/auth/sandbox"
	"github.com/discobox-ai/discobox/server/internal/config"
	"github.com/discobox-ai/discobox/server/internal/handlers"
	"github.com/discobox-ai/discobox/server/internal/harnessdefs"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/resources/gateways"
	"github.com/discobox-ai/discobox/server/internal/secrets"
	"github.com/discobox-ai/discobox/server/internal/service"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/server/internal/transport/carrierhub"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"gorm.io/gorm"
)

const (
	Name    = "Discobox Sandbox Manager"
	Version = "0.1.0"
)

// AppOptions controls application wiring.
type AppOptions struct {
	// Harnesses is what the built-in harnesses resolve against.
	Harnesses harnessdefs.References
	UserID    string
	// DiscobotPublicKey verifies discobot's assertions of who a request is for
	// (ADR 26-10-07-005 §2). Nil refuses every assertion.
	DiscobotPublicKey ed25519.PublicKey
	// AuthRequired leaves the default user out of authentication altogether
	// (authRequired), so every request authenticates as someone.
	AuthRequired bool
	// IrohAdmission is the enrolled-peer gate the iroh listener admits by,
	// checked again per request by IrohPeerAuthenticator. Nil when this
	// server does not listen on iroh.
	IrohAdmission auth.IrohAdmission

	// SSHIngress is what GET /ssh serves: the endpoint SSH clients should dial
	// and the host key to pin. It is resolved by the caller because the
	// advertised address is configuration and the host key is loaded from the
	// data directory, neither of which this constructor owns.
	SSHIngress services.SSHIngress

	// ServerPeer is what GET /peer serves: this server's own peer ID, which
	// every server has (ADR 0117). Resolved by the caller because the identity
	// is loaded with the iroh configuration, which happens before this
	// constructor runs (ADR 0098).
	ServerPeer services.ServerPeer

	// ServerInfo is what GET /server serves: the name this server offers a
	// client registering it (ADR 0116 §2).
	ServerInfo services.ServerInfo

	// IrohListener reports what this server's iroh listener is doing now, as
	// opposed to who it is. Nil on a server with no iroh endpoint.
	IrohListener services.IrohListenerService

	SecretSealer secrets.Sealer

	// DispatcherPollInterval is how often the reconcile engine looks for
	// claimable dirty rows beyond in-process wakeups.
	DispatcherPollInterval time.Duration

	SandboxReconcileJobConcurrency int

	// DevelopmentImages is the watcher-built image set synchronized to every
	// Docker daemon used by a pool provider, and DevelopmentBundles its
	// bundles, which no daemon holds: exported as OCI image layouts, or in
	// build-mode built by a pool's daemon and saved as layouts under
	// DevelopmentBundleDir.
	DevelopmentImages    []devimage.Image
	DevelopmentBundles   []devimage.Bundle
	DevelopmentBundleDir string

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

	// ServerDefaults are the provider settings the server configures rather
	// than any one provider instance (ADR 0096 §5, configuration file).
	ServerDefaults dockerworker.ServerDefaults

	// WSLCCommand overrides the WSL Containers program the Windows host is
	// checked for.
	WSLCCommand string
	// OverlayDir is the one directory a harness's file:// manifest reference
	// may name a file in. Empty refuses every such reference.
	OverlayDir string
	// AssetDir is the blob store bundles are served to pools from (ADR
	// 26-10-16-423 §13).
	AssetDir string

	// ArchiveRetention is how long an archived sandbox is kept before it is
	// purged, for projects that have not set their own. Zero is left zero rather
	// than defaulted here: DefaultAppOptions is production wiring, and the
	// package default is the production answer. See config.Config.
	ArchiveRetention time.Duration

	// ConfiguredGateways is the server configuration file's gateways
	// (ADR 26-10-17-271).
	ConfiguredGateways gateways.Configured
}

// DefaultAppOptions returns the production defaults for the app.
func DefaultAppOptions() AppOptions {
	return AppOptions{
		DispatcherPollInterval:         time.Second,
		SandboxReconcileJobConcurrency: 4,
	}
}

// NewRouter creates a chi router backed by the generated OpenAPI server.
func NewRouter(svc services.Services) (*chi.Mux, error) {
	router := chi.NewRouter()
	RegisterHealthRoutes(router)
	RegisterDocsRoutes(router)
	registerSandboxGitRoutes(router, svc.Sandboxes)
	registerSandboxTransferRoutes(router, svc.Sandboxes)
	registerSandboxHTTPRoutes(router, svc.Sandboxes)
	registerSandboxAgentTerminalRoutes(router, svc.Sandboxes)
	registerSandboxEventRoutes(router, svc.Sandboxes)
	registerSandboxTunnelRoutes(router, svc.Sandboxes)
	registerPoolConsoleRoutes(router, svc.Pools)
	registerPoolLogsRoutes(router, svc.Pools)
	registerPoolHTTPAuditRoutes(router, svc.Pools)
	registerPoolGuestImageRoutes(router, svc.Pools)
	generated, err := handlers.NewServer(svc)
	if err != nil {
		return nil, err
	}
	router.Mount("/", boundInventoryReports(generated))
	return router, nil
}

// NewApp creates the app backed by persistent services. Alongside the router
// it returns the assembled services and store so an in-process, non-HTTP
// caller — the SSH ingress (ADR 0024) — can drive the same
// AcquireSandboxHTTPClient choke point and username resolution an HTTP
// caller would, without a wrapper or a second service assembly.
// The returned shutdown function stops the reconcile engine and closes every
// provider. It is separate from the router because a provider owns resources
// outside this process -- a wslc pool VM, for one -- that outlive the HTTP
// server unless something closes them.
func NewApp(ctx context.Context, writeDB, readDB *gorm.DB, options ...AppOptions) (*chi.Mux, services.Services, *store.Store, func(context.Context) error, error) {
	opts := DefaultAppOptions()
	if len(options) > 0 {
		opts = options[0]
	}

	if opts.UserID == "" {
		opts.UserID = service.DefaultUserID
	}
	appStore := store.New(writeDB, readDB, store.WithSealer(opts.SecretSealer))
	reconcileEngine, err := reconcile.New(writeDB, reconcile.Options{
		// This deployment is single-process; the engine also supports
		// lease-based multi-node claiming — flip this when scaling out.
		SingleNode:   true,
		PollInterval: opts.DispatcherPollInterval,
	})
	if err != nil {
		return nil, services.Services{}, nil, nil, err
	}
	developmentBundles := devbundles.New(opts.DevelopmentBundles, opts.DevelopmentBundleDir)
	developmentImageSync, err := dockerworker.NewDevelopmentImageSynchronizer(opts.DevelopmentImages, developmentBundles)
	if err != nil {
		return nil, services.Services{}, nil, nil, fmt.Errorf("configure development image synchronization: %w", err)
	}
	// Pool backends that cannot be dialed inward hand their guest-initiated
	// control-plane connections to this hub; Serve treats it as one more
	// listener for the ordinary handler, so routing and authentication are
	// unchanged.
	controlPlaneStreams := opts.ControlPlaneStreams
	// A sandbox is told its discobox:// address only when that address
	// reaches this server: every server has a peer ID (ADR 0117), but a peer
	// ID is dialed only over iroh, and a server not listening on it answers
	// nothing there.
	var addressPeerID string
	if config.HasIrohEndpoint(opts.ListenEndpoints) {
		addressPeerID = opts.ServerPeer.ID
	}
	appServices := service.New(appStore, reconcileEngine, service.Options{
		SandboxReconcileJobConcurrency: opts.SandboxReconcileJobConcurrency,
		DevelopmentImageSync:           developmentImageSync,
		DevelopmentBundles:             developmentBundles,
		Harnesses:                      opts.Harnesses,
		ControlPlaneStreams:            controlPlaneStreams,
		ListenEndpoints:                opts.ListenEndpoints,
		ArchiveRetention:               opts.ArchiveRetention,
		ServerPeerID:                   addressPeerID,
		ConfiguredGateways:             opts.ConfiguredGateways,
		ServerDefaults:                 opts.ServerDefaults,
		WSLCCommand:                    opts.WSLCCommand,
		OverlayDir:                     opts.OverlayDir,
		AssetDir:                       opts.AssetDir,
	})
	if opts.SecretSealer != nil {
		appServices.SetSandboxAuthManager(sandboxauth.NewManager(appStore, opts.SecretSealer))
	}
	appServices.SetWorkerAgentAuthManager(poolagentauth.NewManager(appStore, opts.SecretSealer))
	if _, err := appServices.InitializeDefaults(ctx, opts.UserID); err != nil {
		return nil, services.Services{}, nil, nil, err
	}
	if err := appServices.Start(ctx); err != nil {
		return nil, services.Services{}, nil, nil, err
	}
	svc := services.Services{
		SSH:            opts.SSHIngress,
		ServerPeer:     opts.ServerPeer,
		ServerInfo:     opts.ServerInfo,
		IrohListener:   opts.IrohListener,
		Projects:       appServices,
		HarnessConfigs: appServices,
		Sandboxes:      appServices,
		Providers:      appServices,
		Pools:          appServices,
		Jobs:           appServices,
		Secrets:        appServices,
		HostTrusts:     appServices,
		SSHKeys:        appServices,
		SkillsSources:  appServices,
		Gateways:       appServices,
		Peers:          appServices,
		Assets:         appServices.Assets(),
	}
	authenticators := []auth.Authenticator{
		// First, so a forwarded sandbox call can never reach the default user.
		auth.SandboxForwardAuthenticator{Store: appStore},
		auth.PoolAuthenticator{Store: appStore},
		// Before the default user, so an assertion is never answered as it.
		auth.DiscobotAuthenticator{PublicKey: opts.DiscobotPublicKey},
		// Last before the default user: a request over iroh that nothing else
		// claimed is its enrolled peer's.
		auth.IrohPeerAuthenticator{Admission: opts.IrohAdmission, UserID: opts.UserID},
	}
	if !opts.AuthRequired {
		authenticators = append(authenticators, auth.DefaultUserAuthenticator{UserID: opts.UserID})
	}
	router := chi.NewRouter()
	router.Use(auth.Authentication(authenticators...))
	router.Use(auth.Authorization(
		// First, so a sandbox is held to its role before any authorizer that
		// admits every authenticated principal.
		auth.SandboxRoleAuthorizer{Store: appStore},
		auth.ProjectAuthorizer{Store: appStore},
		auth.PoolRouteAuthorizer{},
		auth.SSHConnectAuthorizer{},
		auth.AuthenticatedAuthorizer{},
	))
	RegisterHealthRoutes(router)
	RegisterDocsRoutes(router)
	registerSandboxGitRoutes(router, appServices)
	registerSandboxTransferRoutes(router, appServices)
	registerSandboxHTTPRoutes(router, appServices)
	registerSandboxAgentTerminalRoutes(router, appServices)
	registerSandboxEventRoutes(router, appServices)
	registerSandboxTunnelRoutes(router, appServices)
	registerPoolConsoleRoutes(router, appServices)
	registerPoolLogsRoutes(router, appServices)
	registerPoolHTTPAuditRoutes(router, appServices)
	registerPoolGuestImageRoutes(router, appServices)
	generated, err := handlers.NewServer(svc)
	if err != nil {
		return nil, services.Services{}, nil, nil, err
	}
	router.Mount("/", boundInventoryReports(generated))
	return router, svc, appStore, appServices.Stop, nil
}
