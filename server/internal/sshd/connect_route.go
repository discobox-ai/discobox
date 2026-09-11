package sshd

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
)

// RegisterConnectRoute serves SSH over the transport the API already answers
// on: the websocket's byte stream is handed to handleConn. It is the only way
// into the SSH server — the server binds no SSH port and opens no TCP
// listener (ADR 0057) — so `discobox tools ssh` reaches it the way the CLI
// already reaches the server, and needs no new surface.
//
// The route is unauthenticated at the HTTP layer on purpose: SSH authenticates
// inside its own protocol, by public key, before any channel exists. Gating it
// with HTTP auth would not make it safer — it would only mean a second
// credential in front of the one that already decides.
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
