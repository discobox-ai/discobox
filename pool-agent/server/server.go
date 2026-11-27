package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	workerapi "github.com/discobox-ai/discobox/pool-agent/api/gen"
	"github.com/discobox-ai/discobox/pool-agent/handover"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	"github.com/discobox-ai/discobox/pool-agent/sandboxtoken"
)

type Registration struct {
	PublicKey string
}

type Config struct {
	Identity     Identity
	Registration *Registration
	Runtime      sandboxruntime.Runtime
	// Audit reads the pool proxy's audit trail over its control API.
	Audit                 AuditReader
	ControlPlanePublicKey string
	// SandboxTokenKey verifies the tokens this pool issues to its own
	// sandboxes (sandboxtoken), which the git-origins route accepts beside
	// the control plane's. It is the public half of the pool's identity key;
	// nil accepts control-plane tokens alone.
	SandboxTokenKey ed25519.PublicKey
	// Front serves the agent's own listener, which the agent binds with
	// wire.Listen from DISCOBOX_AGENT_LISTEN_URL: the URL's scheme picks the
	// transport, VSOCK on libkrun and vz pools (so the agent opens no IP
	// port) and TCP on the rest. It is started before the agent's first
	// report, so a control-plane channel can be opened before it (StartFront).
	Front *Front
	// OriginListener, when set, serves the sandboxes' own fetches of their
	// origins, which reach it through the pool proxy (ADR 26-10-08-561). It
	// is loopback, and serves the git-origins route alone, to the sandbox
	// token alone, for the sandbox the proxy says is asking.
	OriginListener net.Listener
	// AssetListener, when set, serves Assets: the sandboxes' fetches of
	// their bundles, which reach it through the pool proxy as origins do
	// (ADR 26-10-16-423 §13). Assets decides on the sandbox the proxy names
	// alone, so the listener is a socket no box can name, never a loopback
	// port (layout.ProxyAssetsSocket).
	AssetListener net.Listener
	Assets        http.Handler
	// Tunnels, when set, serves /tunnel/ on Listener: the tunnels of a
	// machine pool's sandboxes that reach it only through an edge, which
	// forwards to this listener alone, carried on to the proxy unit's tunnel
	// listener (ADR 26-10-29-481 §2). Nothing names a caller until the mTLS
	// inside a stream completes there, so it takes no token here.
	Tunnels http.Handler
	// Drain, when it closes, has the agent drain rather than stop (ADR
	// 26-11-26-536 §3): it stops accepting on every listener, calls
	// StoppedAccepting, and serves what it holds to its end, and waits out
	// the boots and snapshot builds its runtime began, all within one
	// handover.DrainBound, before Serve returns handover.ErrDrained. Nil
	// never drains.
	Drain <-chan struct{}
	// StoppedAccepting is called once a drain has closed every listener here,
	// before it waits on what they accepted.
	StoppedAccepting func()
}

func NewRouter(cfg Config) (*chi.Mux, error) {
	router := chi.NewRouter()
	if cfg.Tunnels != nil {
		router.Handle("/tunnel/*", cfg.Tunnels)
	}
	router.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	router.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": true, "schedulable": true})
	})
	router.HandleFunc("/metadata", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		metadata := map[string]any{
			"projectId": cfg.Identity.ProjectID,
			"poolId":    cfg.Identity.PoolID,
		}
		if cfg.Registration != nil {
			metadata["publicKey"] = cfg.Registration.PublicKey
		}
		_ = json.NewEncoder(w).Encode(metadata)
	})
	authenticator, err := NewSignedTokenAuthenticator(cfg.Identity, cfg.ControlPlanePublicKey)
	if err != nil {
		return nil, err
	}
	var sandboxTokens *sandboxtoken.Verifier
	if cfg.SandboxTokenKey != nil {
		if sandboxTokens, err = sandboxtoken.NewVerifier(cfg.SandboxTokenKey); err != nil {
			return nil, err
		}
	}
	handler := newSandboxService(cfg.Identity, cfg.Runtime, cfg.Audit)
	generated, err := workerapi.NewServer(handler, handler)
	if err != nil {
		return nil, err
	}
	router.Group(func(origins chi.Router) {
		origins.Use(authenticator.OriginMiddleware(sandboxTokens))
		registerSandboxOriginRoutes(origins, handler)
	})
	router.Group(func(protected chi.Router) {
		protected.Use(authenticator.Middleware)
		registerCreateProtocol(protected, cfg.Identity, authenticator.Middleware(generated))
		registerSandboxGitRoutes(protected, handler)
		registerSandboxCacheRoutes(protected, handler)
		registerSandboxTreeRoutes(protected, handler)
		registerAuditRoutes(protected, handler)
		registerSandboxProxyRoutes(protected, handler)
		protected.Mount("/", generated)
	})
	return router, nil
}

// NewOriginRouter serves the git-origins route to sandboxes, as the pool proxy
// forwards their fetches: only for a request whose token is one this pool
// issued, and whose path names the sandbox the proxy authenticated by its
// client certificate (proxy.PoolClientHeader). The control plane's tokens
// are not taken here; it reaches the same route on the agent's own listener.
func NewOriginRouter(cfg Config) (*chi.Mux, error) {
	if cfg.SandboxTokenKey == nil {
		return nil, errors.New("the origin listener needs the pool's sandbox token key")
	}
	sandboxTokens, err := sandboxtoken.NewVerifier(cfg.SandboxTokenKey)
	if err != nil {
		return nil, err
	}
	authenticator, err := NewSignedTokenAuthenticator(cfg.Identity, cfg.ControlPlanePublicKey)
	if err != nil {
		return nil, err
	}
	handler := newSandboxService(cfg.Identity, cfg.Runtime, cfg.Audit)
	router := chi.NewRouter()
	router.Use(authenticator.SandboxOriginMiddleware(sandboxTokens))
	registerSandboxOriginRoutes(router, handler)
	return router, nil
}

func Serve(ctx context.Context, logger *slog.Logger, cfg Config) error {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Front == nil {
		return errors.New("the pool agent's listener is not being served")
	}
	router, err := NewRouter(cfg)
	if err != nil {
		return err
	}
	errCh := make(chan error, 3)
	var originServer *http.Server
	if cfg.OriginListener != nil {
		originRouter, err := NewOriginRouter(cfg)
		if err != nil {
			return err
		}
		// Long-lived like the agent's own: a clone of a large repository is
		// one response.
		originServer = &http.Server{Handler: originRouter, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
		go func() {
			logger.Info("pool agent serving sandbox origins", "addr", cfg.OriginListener.Addr())
			errCh <- originServer.Serve(cfg.OriginListener)
		}()
	}
	var assetServer *http.Server
	if cfg.AssetListener != nil && cfg.Assets != nil {
		// Long-lived like the origins': a layer can be large.
		assetServer = &http.Server{Handler: cfg.Assets, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
		go func() {
			logger.Info("pool agent serving sandbox assets", "addr", cfg.AssetListener.Addr())
			errCh <- assetServer.Serve(cfg.AssetListener)
		}()
	}
	go func() { errCh <- cfg.Front.serve(ctx, router) }()

	select {
	case <-cfg.Drain:
		drain(ctx, logger, cfg, originServer, assetServer)
		return handover.ErrDrained
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = cfg.Front.Shutdown(shutdownCtx)
		if originServer != nil {
			_ = originServer.Shutdown(shutdownCtx)
		}
		if assetServer != nil {
			_ = assetServer.Shutdown(shutdownCtx)
		}
		return ctx.Err()
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// drain stops accepting on every listener the agent serves, and serves what
// they accepted to its end, and waits out the boots and snapshot builds the
// runtime began, until handover.DrainBound has passed or ctx ends, which cuts
// what is left.
func drain(ctx context.Context, logger *slog.Logger, cfg Config, servers ...*http.Server) {
	cfg.Front.StopAccepting()
	for _, listener := range []net.Listener{cfg.OriginListener, cfg.AssetListener} {
		if listener != nil {
			_ = listener.Close()
		}
	}
	if cfg.StoppedAccepting != nil {
		cfg.StoppedAccepting()
	}
	logger.Info("pool agent draining", "bound", handover.DrainBound)
	bounded, cancel := context.WithTimeout(ctx, handover.DrainBound)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := cfg.Front.Drain(bounded); err != nil {
			logger.Warn("pool agent drain cut what was left", "error", err)
		}
	})
	if cfg.Runtime != nil {
		wg.Go(func() {
			if err := cfg.Runtime.AwaitBackground(bounded); err != nil {
				logger.Warn("pool agent exiting with sandbox boots or snapshot builds under way", "error", err)
			}
		})
	}
	for _, server := range servers {
		if server == nil {
			continue
		}
		wg.Go(func() {
			if err := server.Shutdown(bounded); err != nil {
				_ = server.Close()
			}
		})
	}
	wg.Wait()
	logger.Info("pool agent drained")
}
