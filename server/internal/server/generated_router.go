package server

import (
	"github.com/go-chi/chi/v5"
	"github.com/obot-platform/discobox/server/internal/api"
	"github.com/obot-platform/discobox/server/internal/generatedapi"
	"github.com/obot-platform/discobox/server/internal/projectstream"
)

// NewGeneratedRouter returns a chi router backed by generated OpenAPI server
// scaffolding. Project stream SSE remains hand-wired because ogen skips
// text/event-stream operations.
func NewGeneratedRouter(services api.Services) (*chi.Mux, error) {
	router := chi.NewRouter()
	RegisterDocsRoutes(router)
	projectstream.RegisterProjectStreamRoutes(router, services.Events)

	generated, err := generatedapi.NewServer(services)
	if err != nil {
		return nil, err
	}
	router.Mount("/", generated)
	return router, nil
}
