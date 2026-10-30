package handlers

import (
	"context"
	"net/http"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
)

// StreamProjectEvents is never reached: the router serves the stream itself,
// flushing each event as it is written, which the generated server cannot
// (server.registerProjectEventRoutes).
func (h *Handler) StreamProjectEvents(context.Context, serverapi.StreamProjectEventsParams) (serverapi.StreamProjectEventsRes, error) {
	return apiError(apperrors.NewStatusError(http.StatusNotImplemented, "the project event stream is served by the router")), nil
}
