package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	workerapi "github.com/discobox-ai/discobox/pool-agent/api/gen"
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
	Port            int
	// Listener, when set, is served instead of a TCP listener on Port. The
	// pool agent always sets it, binding DISCOBOX_AGENT_LISTEN_URL with
	// wire.Listen: the URL's scheme picks the transport, VSOCK on libkrun and
	// vz pools (so the agent opens no IP port) and TCP on the rest.
	Listener net.Listener
	// OriginListener, when set, serves the sandboxes' own fetches of their
	// origins, which reach it through the pool proxy (ADR 26-10-08-561). It
	// is loopback, and serves the git-origins route alone, to the sandbox
	// token alone, for the sandbox the proxy says is asking.
	OriginListener net.Listener
}

func NewRouter(cfg Config) (*chi.Mux, error) {
	router := chi.NewRouter()
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
		registerSandboxGitRoutes(protected, handler)
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
// client certificate (proxy.OriginClientHeader). The control plane's tokens
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
	port := cfg.Port
	if port == 0 {
		port = envInt("PORT", 3002)
	}

	router, err := NewRouter(cfg)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout/WriteTimeout: those set absolute per-request conn
		// deadlines that survive protocol upgrades (exec attach websockets
		// proxied to the sandbox agent) and cut long-lived streams off
		// mid-flight. Liveness comes from ReadHeaderTimeout, IdleTimeout, and
		// websocket keepalive pings on attach tunnels.
		IdleTimeout: 120 * time.Second,
	}
	errCh := make(chan error, 2)
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
	go func() {
		if cfg.Listener != nil {
			logger.Info("pool agent serving", "addr", cfg.Listener.Addr())
			errCh <- httpServer.Serve(cfg.Listener)
			return
		}
		logger.Info("pool agent serving", "addr", httpServer.Addr)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		if originServer != nil {
			_ = originServer.Shutdown(shutdownCtx)
		}
		return ctx.Err()
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
