package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
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
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if err := server.Serve(listener); err != nil {
		return fmt.Errorf("server failed: %w", err)
	}
	return nil
}
