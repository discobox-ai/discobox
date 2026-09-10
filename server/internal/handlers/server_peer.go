package handlers

import (
	"context"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/services"
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
	if listener, ok := irohListener(h.services.IrohListener); ok {
		body.SetIrohListener(serverapi.NewOptIrohListener(listener))
	}
	return body, nil
}

// irohListener reports what the listener is doing, and whether there is
// anything to report.
//
// Two cases answer no, and both are silence rather than a listener that is
// down: a server with no iroh endpoint has nothing to say, and one whose watch
// has not taken its first reading does not yet know. Reporting either as an
// offline listener would be a false alarm about the transport an operator is
// checking precisely because they suspect it.
func irohListener(service services.IrohListenerService) (serverapi.IrohListener, bool) {
	if service == nil {
		return serverapi.IrohListener{}, false
	}
	state, read := service()
	if !read {
		return serverapi.IrohListener{}, false
	}
	listener := serverapi.IrohListener{
		Online:      state.Online,
		Sockets:     state.Sockets,
		DirectAddrs: state.DirectAddrs,
	}
	if state.HomeRelay != "" {
		listener.SetHomeRelay(serverapi.NewOptString(state.HomeRelay))
	}
	if !state.Since.IsZero() {
		listener.SetSince(serverapi.NewOptDateTime(state.Since))
	}
	return listener, true
}
