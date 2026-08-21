package server

import (
	"github.com/discobox-ai/discobox/server/internal/handlers"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/go-chi/chi/v5"
)

// NewOpenAPIRouter returns a chi router backed by generated OpenAPI server
// scaffolding plus hand-wired transports that generated handlers cannot own
// behavior-compatibly.
func NewOpenAPIRouter(services services.Services) (*chi.Mux, error) {
	router := chi.NewRouter()
	RegisterDocsRoutes(router)
	registerSandboxGitRoutes(router, services.Sandboxes)
	registerSandboxHTTPRoutes(router, services.Sandboxes)
	registerSandboxAgentTerminalRoutes(router, services.Sandboxes)

	generated, err := handlers.NewServer(services)
	if err != nil {
		return nil, err
	}
	router.Mount("/", generated)
	return router, nil
}
