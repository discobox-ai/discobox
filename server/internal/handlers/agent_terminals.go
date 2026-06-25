package handlers

import (
	"context"
	"net/http"

	serverapi "github.com/obot-platform/discobox/api/gen"
)

func (h *Handler) AttachAgentTerminal(context.Context, serverapi.AttachAgentTerminalParams) (serverapi.AttachAgentTerminalRes, error) {
	return sandboxAgentTerminalNotImplemented(), nil
}

func (h *Handler) CreateAgentTerminal(context.Context, *serverapi.CreateAgentTerminalRequest, serverapi.CreateAgentTerminalParams) (serverapi.CreateAgentTerminalRes, error) {
	return sandboxAgentTerminalNotImplemented(), nil
}

func (h *Handler) DeleteAgentTerminal(context.Context, serverapi.DeleteAgentTerminalParams) (serverapi.DeleteAgentTerminalRes, error) {
	return sandboxAgentTerminalNotImplemented(), nil
}

func (h *Handler) GetAgentTerminalResources(context.Context, serverapi.GetAgentTerminalResourcesParams) (serverapi.GetAgentTerminalResourcesRes, error) {
	return sandboxAgentTerminalNotImplemented(), nil
}

func (h *Handler) ListAgentTerminalEvents(context.Context, serverapi.ListAgentTerminalEventsParams) (serverapi.ListAgentTerminalEventsRes, error) {
	return sandboxAgentTerminalNotImplemented(), nil
}

func (h *Handler) ListAgentTerminalResourceHistory(context.Context, serverapi.ListAgentTerminalResourceHistoryParams) (serverapi.ListAgentTerminalResourceHistoryRes, error) {
	return sandboxAgentTerminalNotImplemented(), nil
}

func (h *Handler) ListAgentTerminals(context.Context, serverapi.ListAgentTerminalsParams) (serverapi.ListAgentTerminalsRes, error) {
	return sandboxAgentTerminalNotImplemented(), nil
}

func (h *Handler) StreamAgentTerminalResources(context.Context, serverapi.StreamAgentTerminalResourcesParams) (serverapi.StreamAgentTerminalResourcesRes, error) {
	return sandboxAgentTerminalNotImplemented(), nil
}

func sandboxAgentTerminalNotImplemented() *serverapi.ErrorResponseStatusCode {
	return &serverapi.ErrorResponseStatusCode{
		StatusCode: http.StatusNotImplemented,
		Response: serverapi.ErrorResponse{
			Error: "sandbox agent terminal operations are handled by the reverse proxy",
		},
	}
}
