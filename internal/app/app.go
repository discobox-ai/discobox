// Package app wires the HTTP router and API operations.
package app

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/obot-platform/disco2/internal/api"
	"github.com/obot-platform/disco2/internal/database"
	"github.com/obot-platform/disco2/internal/events"
	"github.com/obot-platform/disco2/internal/jobs"
	"github.com/obot-platform/disco2/internal/orchestration"
	"github.com/obot-platform/disco2/internal/service"
	"github.com/obot-platform/disco2/internal/store"
	"github.com/obot-platform/disco2/jobqueue"
	"github.com/obot-platform/disco2/jobqueue/gormstore"
)

const (
	Name    = "Disco2 Sandbox Manager"
	Version = "0.1.0"
)

// NewRouter creates a chi router with all Huma operations registered.
func NewRouter(services api.Services) (*chi.Mux, huma.API) {
	router := chi.NewRouter()
	humaAPI := humachi.New(router, huma.DefaultConfig(Name, Version))
	api.Register(humaAPI, services)
	return router, humaAPI
}

// NewStubbedRouter creates a router backed by in-memory stub services.
func NewStubbedRouter() (*chi.Mux, huma.API) {
	stubs := service.NewStub()
	return NewRouter(api.Services{
		Projects:  stubs,
		Sandboxes: stubs,
		Events:    stubs,
	})
}

// NewDatabaseRouter creates a router backed by the database service.
func NewDatabaseRouter(ctx context.Context, db *database.DB) (*chi.Mux, huma.API, error) {
	broker := events.NewBroker()
	appStore := store.New(db.Write, db.Read, broker)
	jobStore := gormstore.New(db.Write, db.Read)
	queueConfig := jobqueue.QueueConfig{
		DefaultMaxAttempts: 3,
	}
	dispatcher := jobqueue.NewDispatcher(jobStore, jobqueue.DispatcherConfig{
		SingleNode:         true,
		PollInterval:       time.Second,
		JobTimeout:         time.Minute,
		StaleJobTimeout:    5 * time.Minute,
		ImmediateExecution: true,
		DefaultConcurrency: 1,
	})
	ensureJob := func(ctx context.Context, txDB *gorm.DB, payload jobqueue.Payload) (*jobqueue.Job, bool, error) {
		return gormstore.New(txDB, txDB).EnsureActiveJobForPayload(ctx, payload, queueConfig)
	}
	services := service.New(appStore, orchestration.New(appStore, ensureJob, dispatcher.NotifyNewJob), broker)
	sandboxReconciler := service.NewSandboxReconciler(appStore, service.NewSandboxOperations())
	if err := dispatcher.Register(jobs.NewSandboxReconcileExecutor(sandboxReconciler), jobqueue.WithConcurrency(4)); err != nil {
		return nil, nil, err
	}

	if err := services.InitializeDefaults(ctx); err != nil {
		return nil, nil, err
	}
	if err := dispatcher.Start(ctx); err != nil {
		return nil, nil, err
	}
	router, api := NewRouter(api.Services{
		Projects:  services,
		Sandboxes: services,
		Events:    services,
	})
	return router, api, nil
}
