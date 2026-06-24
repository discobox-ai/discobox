package server

import (
	"github.com/go-chi/chi/v5"
	"github.com/obot-platform/discobox/server/internal/handlers"
	services "github.com/obot-platform/discobox/server/internal/services"
)

// NewGeneratedRouter returns a chi router backed by generated OpenAPI server
// scaffolding. Project stream SSE remains hand-wired because ogen skips
// text/event-stream operations.
func NewGeneratedRouter(services services.Services) (*chi.Mux, error) {
	router := chi.NewRouter()
	RegisterDocsRoutes(router)
	registerProjectStreamTransports(router, services.Events)
	registerSandboxGitRoutes(router, services.Sandboxes)
	registerSandboxHTTPRoutes(router, services.Sandboxes)

	generated, err := handlers.NewServer(services)
	if err != nil {
		return nil, err
	}
	router.Mount("/", generated)
	return router, nil
}
