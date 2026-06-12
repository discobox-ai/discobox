package server

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/joho/godotenv"

	"github.com/obot-platform/discobox/internal/config"
	"github.com/obot-platform/discobox/internal/database"
	"github.com/obot-platform/discobox/internal/model"
	"github.com/obot-platform/discobox/internal/secrets"
	"github.com/obot-platform/discobox/internal/service"
)

// Run loads configuration, initializes storage and services, and starts the HTTP server.
func Run(ctx context.Context) error {
	// Load .env file if present.
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.TenantID == "" {
		cfg.TenantID = service.DefaultTenantID
	}

	resolver := database.NewResolver(database.ResolverConfig{
		Config: database.Config{
			Driver:  cfg.DatabaseDriver,
			DSN:     cfg.DatabaseDSN,
			ReadDSN: cfg.DatabaseReadDSN,
		},
		MigrateOnOpen: true,
	})
	defer resolver.Close()

	globalDB, err := resolver.ResolveGlobal(ctx)
	if err != nil {
		return fmt.Errorf("open global database: %w", err)
	}
	if err := InitializeGlobalDefaults(ctx, globalDB, cfg.TenantID); err != nil {
		return fmt.Errorf("initialize global defaults: %w", err)
	}

	var sealer secrets.Sealer
	if cfg.EncryptionKey != "" {
		sealer, err = secrets.NewAESGCMSealerFromBase64Key(cfg.EncryptionKey)
		if err != nil {
			return fmt.Errorf("initialize encryption: %w", err)
		}
	}

	router, _, err := NewDatabaseRouter(ctx, resolver, DatabaseRouterOptions{
		TenantID:                       cfg.TenantID,
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
	})
	if err != nil {
		return fmt.Errorf("initialize app: %w", err)
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("listening on http://localhost%s", addr)
	log.Printf("openapi spec available at http://localhost%s/openapi.json", addr)
	if err := http.ListenAndServe(addr, router); err != nil {
		return fmt.Errorf("server failed: %w", err)
	}
	return nil
}

// InitializeGlobalDefaults creates global defaults shared by tenant databases.
func InitializeGlobalDefaults(ctx context.Context, db *database.DB, tenantID string) error {
	tenant := &model.Tenant{
		ID:   tenantID,
		Name: "Default Tenant",
		Slug: "default",
	}
	if err := db.Write.WithContext(ctx).Save(tenant).Error; err != nil {
		return err
	}
	user := &model.User{
		ID:       service.DefaultUserID,
		TenantID: tenant.ID,
		Email:    "local@example.com",
		Provider: "local",
		Subject:  "local",
	}
	return db.Write.WithContext(ctx).Save(user).Error
}
