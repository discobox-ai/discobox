// Package server wires and runs the HTTP server.
package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/obot-platform/disco2/internal/api"
	"github.com/obot-platform/disco2/internal/database"
	"github.com/obot-platform/disco2/internal/events"
	"github.com/obot-platform/disco2/internal/sandboxauth"
	"github.com/obot-platform/disco2/internal/secrets"
	"github.com/obot-platform/disco2/internal/service"
	"github.com/obot-platform/disco2/internal/store"
	"github.com/obot-platform/disco2/internal/tenantctx"
	"github.com/obot-platform/disco2/orchestration"
)

const (
	Name    = "Disco2 Sandbox Manager"
	Version = "0.1.0"
)

// DatabaseRouterOptions controls database-backed router wiring.
type DatabaseRouterOptions struct {
	TenantID string
	UserID   string

	JobMaxAttempts int

	SecretSealer secrets.Sealer

	DispatcherEnabled            bool
	DispatcherPollInterval       time.Duration
	DispatcherJobTimeout         time.Duration
	DispatcherStaleJobTimeout    time.Duration
	DispatcherImmediateExecution bool
	DispatcherDefaultConcurrency int

	SandboxReconcileJobConcurrency int
}

// DefaultDatabaseRouterOptions returns the production defaults for the
// database-backed app.
func DefaultDatabaseRouterOptions() DatabaseRouterOptions {
	return DatabaseRouterOptions{
		JobMaxAttempts:                 3,
		DispatcherEnabled:              true,
		DispatcherPollInterval:         time.Second,
		DispatcherJobTimeout:           time.Minute,
		DispatcherStaleJobTimeout:      5 * time.Minute,
		DispatcherImmediateExecution:   true,
		DispatcherDefaultConcurrency:   1,
		SandboxReconcileJobConcurrency: 4,
	}
}

// NewRouter creates a chi router with all Huma operations registered.
func NewRouter(services api.Services) (*chi.Mux, huma.API) {
	router := chi.NewRouter()
	config := huma.DefaultConfig(Name, Version)
	config.DocsRenderer = huma.DocsRendererSwaggerUI
	humaAPI := humachi.New(router, config)
	api.Register(humaAPI, services)
	return router, humaAPI
}

// NewStubbedRouter creates a router backed by in-memory stub services.
func NewStubbedRouter() (*chi.Mux, huma.API) {
	stubs := service.NewStub()
	return NewRouter(api.Services{
		Projects:  stubs,
		Sandboxes: stubs,
		Providers: stubs,
		Workers:   stubs,
		Events:    stubs,
	})
}

// NewDatabaseRouter creates a router backed by the database service.
func NewDatabaseRouter(ctx context.Context, resolver *database.Resolver, options ...DatabaseRouterOptions) (*chi.Mux, huma.API, error) {
	opts := DefaultDatabaseRouterOptions()
	if len(options) > 0 {
		opts = options[0]
	}

	if opts.UserID == "" {
		opts.UserID = service.DefaultUserID
	}

	broker := events.NewBroker()
	appStore := store.New(resolver, store.WithPublisher(broker), store.WithSealer(opts.SecretSealer))
	queueConfig := orchestration.QueueConfig{
		DefaultMaxAttempts: opts.JobMaxAttempts,
	}
	tenantJobs := newTenantJobManager(ctx, appStore, opts)
	notifyNewJob := tenantJobs.NotifyNewJob
	services := service.New(appStore, queueConfig, notifyNewJob, broker)
	tenantJobs.SetService(services)
	if opts.SecretSealer != nil {
		services.SetSandboxAuthManager(sandboxauth.NewManager(appStore, opts.SecretSealer))
	}
	if opts.TenantID != "" {
		initCtx := tenantctx.WithTenantID(ctx, opts.TenantID)
		if err := services.InitializeDefaults(initCtx, opts.TenantID, opts.UserID); err != nil {
			return nil, nil, err
		}
	}
	router := chi.NewRouter()
	router.Use(tenantMiddleware(tenantJobs))
	config := huma.DefaultConfig(Name, Version)
	config.DocsRenderer = huma.DocsRendererSwaggerUI
	humaAPI := humachi.New(router, config)
	api.Register(humaAPI, api.Services{
		Projects:  services,
		Sandboxes: services,
		Providers: services,
		Workers:   services,
		Events:    services,
	})
	return router, humaAPI, nil
}

func tenantMiddleware(jobs *tenantJobManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isTenantOptionalPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			tenantID := strings.TrimSpace(r.Header.Get("X-Disco2-Tenant-ID"))
			if tenantID == "" {
				http.Error(w, "tenant ID is required", http.StatusBadRequest)
				return
			}
			ctx := tenantctx.WithTenantID(r.Context(), tenantID)
			if err := jobs.EnsureStarted(ctx); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func isTenantOptionalPath(path string) bool {
	return path == "/openapi.json" || path == "/docs" || strings.HasPrefix(path, "/docs/")
}
