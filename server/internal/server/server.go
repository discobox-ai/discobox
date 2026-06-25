package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
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

	listener, listenDisplay, cleanupListener, err := localipc.Listen(cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer cleanupListener()
	log.Printf("listening on %s", listenDisplay)
	log.Printf("openapi spec available at %s/openapi.yaml", listenDisplay)
	log.Printf("api docs available at %s/docs", listenDisplay)
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
	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server failed: %w", err)
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
