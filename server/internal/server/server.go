package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/server/internal/config"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/secrets"
	"github.com/discobox-ai/discobox/server/internal/service"
	"github.com/discobox-ai/discobox/server/internal/sshd"
	"github.com/discobox-ai/discobox/server/internal/transport/carrierhub"
)

// Run loads configuration, initializes storage and services, and starts the HTTP server.
func Run(ctx context.Context) error {
	// Load .env file if present.
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// Wait until we are the only server on this data directory, asking any
	// incumbent to shut down. This is deferred first so it releases last: the
	// listener cleanup below must have removed the socket before the next
	// server takes the lock and binds it.
	releaseSingleton, err := acquireSingleton(ctx, cfg.DataDir, cfg.Listen)
	if err != nil {
		return err
	}
	defer releaseSingleton()
	shutdownTelemetry, err := initTelemetry(ctx, TelemetryOptions{
		MetricsEnabled:       cfg.OTelMetricsEnabled,
		MetricExportInterval: cfg.OTelMetricExportInterval,
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := shutdownTelemetry(context.Background()); err != nil {
			log.Printf("shutdown telemetry: %v", err)
		}
	}()

	// Guest-initiated control-plane connections are served by the same handler
	// as every other request; only the way the connection arrived differs.
	controlPlaneStreams := carrierhub.New()
	defer func() { _ = controlPlaneStreams.Close() }()

	// Bind before initializing, and answer while initializing.
	//
	// Everything below this point is slow at least once: opening the database,
	// migrating it, building the services, reaching a registry to seed the
	// built-in harness configs. All of it used to happen before anything was
	// listening, so a client had a refused connection to look at and no way to
	// tell a server still coming up from one that died on startup — two very
	// different problems that looked identical, and the reason a CLI could sit
	// out its whole start timeout with nothing to report.
	if err := configureIroh(cfg.DataDir, cfg.Listen); err != nil {
		return err
	}
	listeners, err := listenAll(ctx, cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer cleanupListeners(listeners)
	// The hub has no address to log: every connection on it was opened by a
	// pool guest through its own transport.
	listeners = append(listeners, serverListener{Listener: controlPlaneStreams, display: "pool control-plane streams"})

	startup := newStartupHandler("opening the database")
	activity := newActivityTracker()
	httpServer := &http.Server{
		Handler:           startup,
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout/WriteTimeout: those set absolute per-request conn
		// deadlines that survive protocol upgrades (exec attach websockets
		// proxied to workers) and cut long-lived streams (project events, log
		// follows) off mid-flight. Liveness comes from ReadHeaderTimeout,
		// IdleTimeout, and websocket keepalive pings on attach tunnels.
		IdleTimeout: 120 * time.Second,
	}
	// Gracefully shut down on context cancellation (e.g. SIGINT/SIGTERM) so the
	// listeners are released promptly instead of dying with the process. Armed
	// before the slow work below, so an interrupt during startup is honored.
	// ctx is already canceled by the time this fires, so the drain deadline is
	// derived from a fresh context; using ctx would abort the shutdown at once.
	go func() { //nolint:gosec // G118: intentional detached context for post-cancellation drain.
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown: %v", err)
		}
	}()
	serveErr := make(chan error, 1)
	go func() { serveErr <- serveAll(httpServer, listeners) }()

	db, err := database.New(database.Config{
		Driver:  cfg.DatabaseDriver,
		DSN:     cfg.DatabaseDSN,
		ReadDSN: cfg.DatabaseReadDSN,
	})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	startup.setPhase("migrating the database")
	if err := db.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	var sealer secrets.Sealer
	if cfg.EncryptionKey != "" {
		sealer, err = secrets.NewAESGCMSealerFromBase64Key(cfg.EncryptionKey)
		if err != nil {
			return fmt.Errorf("initialize encryption: %w", err)
		}
	}

	// Resolved before the app is built: GET /ssh is an ordinary generated
	// handler reading services.Services, so the discovery document has to
	// exist by the time the router does.
	sshIngress, sshHostKey, err := resolveSSHIngress(cfg)
	if err != nil {
		return err
	}

	startup.setPhase("starting services")
	router, appServices, appStore, shutdownApp, err := NewApp(ctx, db.Write, db.Read, AppOptions{
		SSHIngress:                     sshIngress,
		ControlPlaneStreams:            controlPlaneStreams,
		UserID:                         service.DefaultUserID,
		SecretSealer:                   sealer,
		DispatcherPollInterval:         cfg.DispatcherPollInterval,
		SandboxReconcileJobConcurrency: cfg.SandboxReconcileJobConcurrency,
		DefaultSandboxImage:            cfg.DefaultSandboxImage,
		DefaultSandboxImageDigest:      cfg.DefaultSandboxImageDigest,
		HostID:                         cfg.HostID,
		DevelopmentImages:              cfg.DevelopmentImages,
		ListenEndpoints:                cfg.Listen,
	})
	if err != nil {
		return fmt.Errorf("initialize app: %w", err)
	}

	// SSH's only front door. The route is registered here rather than inside
	// NewApp because sshd needs the services NewApp returns, and registering it
	// on the same router keeps SSH reachable wherever the API is — which is
	// what both `discobox tools ssh` and a written ssh_config rely on.
	sshServer, err := newSSHServer(cfg, sshHostKey, appServices, appStore)
	if err != nil {
		return err
	}
	sshd.RegisterConnectRoute(router, sshServer)

	router.Post("/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		go func() {
			time.Sleep(50 * time.Millisecond)
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := httpServer.Shutdown(shutdownCtx); err != nil {
				log.Printf("shutdown request: %v", err)
			}
		}()
	})
	if cfg.AutoShutdownTimeout > 0 {
		go activity.ShutdownWhenIdle(ctx, httpServer, cfg.AutoShutdownTimeout)
	}
	// Runs however serve returns, not only on cancellation, so a listener error
	// tears the backends down too. The HTTP server has already stopped
	// accepting by then, so nothing new arrives mid-teardown.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := shutdownApp(shutdownCtx); err != nil {
			log.Printf("shut down services: %v", err)
		}
	}()

	handler := otelhttp.NewHandler(router, "discobox-server")
	if cfg.AutoShutdownTimeout > 0 {
		handler = activity.Wrap(handler)
	}
	// From here the endpoints serve the API instead of a startup status. The
	// listeners have been accepting since before the database was opened, so
	// nothing rebinds and no client is dropped.
	startup.setReady(handler)
	for _, listener := range listeners {
		log.Printf("listening on %s", listener.display)
		// An iroh endpoint is also printed as a ticket. The URL is the form to
		// read — its endpoint ID is what goes in a peer's authorized_ids — and
		// the ticket is the form to paste, with no query string for a shell or
		// a chat client to mangle. Both dial the same server.
		if ticket, err := irohTicket(listener.display); err == nil {
			log.Printf("or dial it with the ticket %s", ticket)
		}
		log.Printf("openapi spec available at %s/openapi.yaml", listener.display)
		log.Printf("api docs available at %s/docs", listener.display)
	}
	return <-serveErr
}

type serverListener struct {
	net.Listener
	display string
	cleanup func()
}

const (
	// reclaimTimeout bounds how long listenWithReclaim waits for a previous
	// server to release an endpoint before giving up.
	reclaimTimeout = 5 * time.Second
	// reclaimRetryInterval is the backoff between bind attempts while waiting
	// for an endpoint to be released.
	reclaimRetryInterval = 100 * time.Millisecond
)

func listenAll(ctx context.Context, endpoints []string) ([]serverListener, error) {
	listeners := make([]serverListener, 0, len(endpoints))
	for _, endpoint := range endpoints {
		listener, err := listenWithReclaim(ctx, endpoint)
		if err != nil {
			cleanupListeners(listeners)
			return nil, err
		}
		listeners = append(listeners, listener)
	}
	return listeners, nil
}

// listenWithReclaim binds a single endpoint, retrying while the address is still
// held by a previous server. On each in-use failure it re-requests shutdown of
// whoever holds the endpoint, then retries until reclaimTimeout elapses. This
// replaces a fixed post-shutdown sleep with a verified reclaim: we bind as soon
// as the address is actually free rather than guessing a drain time.
func listenWithReclaim(ctx context.Context, raw string) (serverListener, error) {
	deadline := time.Now().Add(reclaimTimeout)
	for {
		listener, display, cleanup, err := endpoint.Listen(raw)
		if err == nil {
			return serverListener{Listener: listener, display: display, cleanup: cleanup}, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) || !time.Now().Before(deadline) {
			return serverListener{}, err
		}
		requestEndpointShutdown(ctx, raw)
		select {
		case <-ctx.Done():
			return serverListener{}, ctx.Err()
		case <-time.After(reclaimRetryInterval):
		}
	}
}

func cleanupListeners(listeners []serverListener) {
	for _, listener := range listeners {
		if listener.cleanup != nil {
			listener.cleanup()
		}
	}
}

// shutdownExistingLocalServer asks any server currently listening on the given
// endpoints to shut down. It is best-effort: unreachable endpoints are ignored.
// Every endpoint is contacted (not just the first per scheme) so a stray server
// holding only one of them is still told to leave.
func shutdownExistingLocalServer(ctx context.Context, endpoints []string) {
	for _, endpoint := range endpoints {
		requestEndpointShutdown(ctx, endpoint)
	}
}

// requestEndpointShutdown POSTs /shutdown to a single endpoint, ignoring any
// error (including an unreachable endpoint).
func requestEndpointShutdown(ctx context.Context, raw string) {
	shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	baseURL, client, err := endpoint.HTTPClient(raw, nil)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(shutdownCtx, http.MethodPost, baseURL+"/shutdown", nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	log.Printf("requested shutdown of existing server at %s", raw)
}

func serveAll(server *http.Server, listeners []serverListener) error {
	errCh := make(chan error, len(listeners))
	for _, listener := range listeners {
		go func() {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
				return
			}
			errCh <- nil
		}()
	}
	var serveErr error
	for range listeners {
		if err := <-errCh; err != nil && serveErr == nil {
			serveErr = err
			_ = server.Shutdown(context.Background())
		}
	}
	if serveErr != nil {
		return fmt.Errorf("server failed: %w", serveErr)
	}
	return nil
}

type activityTracker struct {
	active       int64
	lastNano     int64
	shutdownOnce sync.Once
}

func newActivityTracker() *activityTracker {
	t := &activityTracker{}
	t.mark(time.Now())
	return t
}

func (t *activityTracker) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.mark(time.Now())
		atomic.AddInt64(&t.active, 1)
		defer func() {
			t.mark(time.Now())
			atomic.AddInt64(&t.active, -1)
		}()
		next.ServeHTTP(w, r)
	})
}

func (t *activityTracker) ShutdownWhenIdle(ctx context.Context, server *http.Server, timeout time.Duration) {
	interval := min(timeout/4, time.Second)
	if interval <= 0 {
		interval = time.Nanosecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if atomic.LoadInt64(&t.active) > 0 {
				continue
			}
			last := time.Unix(0, atomic.LoadInt64(&t.lastNano))
			if time.Since(last) < timeout {
				continue
			}
			t.shutdownOnce.Do(func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := server.Shutdown(shutdownCtx); err != nil {
					log.Printf("idle shutdown: %v", err)
				}
			})
			return
		}
	}
}

func (t *activityTracker) mark(now time.Time) {
	atomic.StoreInt64(&t.lastNano, now.UnixNano())
}
