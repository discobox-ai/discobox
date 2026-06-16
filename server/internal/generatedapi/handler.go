package generatedapi

import (
	"net/http"

	serverapi "github.com/obot-platform/discobox/api/servergen"
	"github.com/obot-platform/discobox/server/internal/api"
)

// Handler adapts server services to the generated OpenAPI server interface.
//
// It intentionally embeds the generated unimplemented handler while the server
// module is migrated endpoint-by-endpoint away from Huma registration.
type Handler struct {
	serverapi.UnimplementedHandler

	services api.Services
}

var _ serverapi.Handler = (*Handler)(nil)

// NewHandler creates the generated API handler adapter.
func NewHandler(services api.Services) *Handler {
	return &Handler{services: services}
}

// NewServer creates an http.Handler from the generated OpenAPI server scaffold.
func NewServer(services api.Services, opts ...serverapi.ServerOption) (http.Handler, error) {
	return serverapi.NewServer(NewHandler(services), opts...)
}
