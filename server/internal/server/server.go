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

	"github.com/obot-platform/discobox/localipc"
	"github.com/obot-platform/discobox/server/internal/config"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/secrets"
	"github.com/obot-platform/discobox/server/internal/service"
)

// Run loads configuration, initializes storage and services, and starts the HTTP server.
func Run(ctx context.Context) error {
	// Load .env file if present.
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// Best-effort: ask any server currently holding our endpoints to shut down.
	// This is our only chance to reach it over the unix socket before listenAll
	// rebinds that path; the remaining reclaim happens in listenAll.
	shutdownExistingLocalServer(ctx, cfg.Listen)
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

	db, err := database.New(database.Config{
		Driver:  cfg.DatabaseDriver,
		DSN:     cfg.DatabaseDSN,
		ReadDSN: cfg.DatabaseReadDSN,
	})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
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

	router, err := NewApp(ctx, db.Write, db.Read, AppOptions{
		UserID:                         service.DefaultUserID,
		JobMaxAttempts:                 cfg.JobMaxAttempts,
		SecretSealer:                   sealer,
		DispatcherEnabled:              cfg.DispatcherEnabled,
		DispatcherPollInterval:         cfg.DispatcherPollInterval,
		DispatcherJobTimeout:           cfg.DispatcherJobTimeout,
		DispatcherStaleJobTimeout:      cfg.DispatcherStaleJobTimeout,
		DispatcherImmediateExecution:   cfg.DispatcherImmediateExecution,
		DispatcherDefaultConcurrency:   cfg.DispatcherDefaultConcurrency,
		SandboxReconcileJobConcurrency: cfg.SandboxReconcileJobConcurrency,
		DefaultSandboxImage:            cfg.DefaultSandboxImage,
	})
	if err != nil {
		return fmt.Errorf("initialize app: %w", err)
	}

	listeners, err := listenAll(ctx, cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer cleanupListeners(listeners)
	for _, listener := range listeners {
		log.Printf("listening on %s", listener.display)
		log.Printf("openapi spec available at %s/openapi.yaml", listener.display)
		log.Printf("api docs available at %s/docs", listener.display)
	}
	handler := otelhttp.NewHandler(router, "discobox-server")
	activity := newActivityTracker()
	if cfg.AutoShutdownTimeout > 0 {
		handler = activity.Wrap(handler)
	}
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
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
	// Gracefully shut down on context cancellation (e.g. SIGINT/SIGTERM) so the
	// listeners are released promptly instead of dying with the process.
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
	return serveAll(httpServer, listeners)
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
func listenWithReclaim(ctx context.Context, endpoint string) (serverListener, error) {
	deadline := time.Now().Add(reclaimTimeout)
	for {
		listener, display, cleanup, err := localipc.Listen(endpoint)
		if err == nil {
			return serverListener{Listener: listener, display: display, cleanup: cleanup}, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) || !time.Now().Before(deadline) {
			return serverListener{}, err
		}
		requestEndpointShutdown(ctx, endpoint)
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
func requestEndpointShutdown(ctx context.Context, endpoint string) {
	shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	baseURL, client, err := localipc.HTTPClient(endpoint, nil)
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
	log.Printf("requested shutdown of existing server at %s", endpoint)
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
