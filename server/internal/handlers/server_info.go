package handlers

import (
	"context"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// GetServerInfo serves what this server calls itself (ADR 0116 §2): the name
// a client offers when it registers this server, which the client may replace
// with one of its own.
//
// An absent name is a server with no hostname and no name setting, and the
// client names it after its address — the same thing it does for a server
// that predates this route.
func (h *Handler) GetServerInfo(context.Context) (serverapi.GetServerInfoRes, error) {
	body := &apimodel.ServerInfo{}
	if name := h.services.ServerInfo.Name; name != "" {
		body.SetName(serverapi.NewOptString(name))
	}
	return body, nil
}
