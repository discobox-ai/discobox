package server

import (
	"github.com/go-chi/chi/v5"
	"github.com/obot-platform/discobox/server/internal/handlers"
	services "github.com/obot-platform/discobox/server/internal/services"
)

// NewOpenAPIRouter returns a chi router backed by generated OpenAPI server
// scaffolding plus hand-wired transports that generated handlers cannot own
// behavior-compatibly.
func NewOpenAPIRouter(services services.Services) (*chi.Mux, error) {
	router := chi.NewRouter()
	RegisterDocsRoutes(router)
	registerProjectStreamTransports(router, services.Events)
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
