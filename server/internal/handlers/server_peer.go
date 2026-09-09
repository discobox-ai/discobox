package handlers

import (
	"context"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// GetServerPeer serves this server's own peer ID (ADR 0098): the value a
// client dials as `discobox://<peer-id>`, resolved once when the iroh endpoint
// was configured.
//
// It is the same shape as GetSSHIngress — a server telling a client who it is,
// over the transport that client already has — and it exists for the same
// reason: the alternative is grepping a startup log line.
//
// An absent ID is the answer for a server that does not listen for peers,
// rather than an error. Such a server has no peer identity at all: the key is
// loaded only for an endpoint that is bound, so there is nothing to report and
// nothing wrong.
func (h *Handler) GetServerPeer(context.Context) (serverapi.GetServerPeerRes, error) {
	body := &apimodel.ServerPeer{}
	if id := h.services.ServerPeer.ID; id != "" {
		body.SetPeerId(serverapi.NewOptString(id))
	}
	return body, nil
}
