package sshd

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
)

// RegisterConnectRoute serves SSH over the transport the API already answers
// on: the websocket's byte stream is handed to the same handleConn the TCP
// listener feeds, so both front doors are one server with one authentication
// path.
//
// This is what lets `disco tools ssh` work against a server that binds no SSH
// port. A machine-wide TCP listener is opted into (ADR 0024); reaching the
// server the way the CLI already reaches it needs no new surface, because it
// *is* the existing surface.
//
// The route is unauthenticated at the HTTP layer on purpose, exactly like the
// TCP listener it mirrors: SSH authenticates inside its own protocol, by public
// key, before any channel exists. Gating it with HTTP auth would not make it
// safer — it would only mean a second credential in front of the one that
// already decides.
func RegisterConnectRoute(router chi.Router, server *Server) {
	router.Get("/ssh/connect", func(w http.ResponseWriter, r *http.Request) {
		if server == nil {
			http.Error(w, "SSH is not configured", http.StatusServiceUnavailable)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Not the request context: it is canceled when the handler returns,
		// and an SSH session outlives the HTTP request that carried it in.
		// websocket.NetConn's own lifetime ends with the connection.
		ctx := context.WithoutCancel(r.Context())
		netConn := websocket.NetConn(ctx, conn, websocket.MessageBinary)
		server.handleConn(ctx, netConn)
	})
}
