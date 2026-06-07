package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/joho/godotenv"

	"github.com/obot-platform/disco2/internal/app"
	"github.com/obot-platform/disco2/internal/config"
	"github.com/obot-platform/disco2/internal/database"
	"github.com/obot-platform/disco2/internal/secrets"
)

func main() {
	// Load .env file if present.
	_ = godotenv.Load()

	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	db, err := database.New(database.Config{
		Driver:  cfg.DatabaseDriver,
		DSN:     cfg.DatabaseDSN,
		ReadDSN: cfg.DatabaseReadDSN,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "migrate database: %v\n", err)
		os.Exit(1)
	}

	var sealer secrets.Sealer
	if cfg.EncryptionKey != "" {
		sealer, err = secrets.NewAESGCMSealerFromBase64Key(cfg.EncryptionKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "initialize encryption: %v\n", err)
			os.Exit(1)
		}
	}

	router, _, err := app.NewDatabaseRouter(ctx, db, app.DatabaseRouterOptions{
		JobMaxAttempts:                 cfg.JobMaxAttempts,
		SecretSealer:                   sealer,
		DispatcherEnabled:              cfg.DispatcherEnabled,
		DispatcherPollInterval:         cfg.DispatcherPollInterval,
		DispatcherJobTimeout:           cfg.DispatcherJobTimeout,
		DispatcherStaleJobTimeout:      cfg.DispatcherStaleJobTimeout,
		DispatcherImmediateExecution:   cfg.DispatcherImmediateExecution,
		DispatcherDefaultConcurrency:   cfg.DispatcherDefaultConcurrency,
		SandboxReconcileJobConcurrency: cfg.SandboxReconcileJobConcurrency,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "initialize app: %v\n", err)
		os.Exit(1)
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("listening on http://localhost%s", addr)
	log.Printf("openapi spec available at http://localhost%s/openapi.json", addr)
	if err := http.ListenAndServe(addr, router); err != nil {
		fmt.Fprintf(os.Stderr, "server failed: %v\n", err)
		os.Exit(1)
	}
}
