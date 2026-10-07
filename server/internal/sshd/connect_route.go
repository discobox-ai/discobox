package sshd

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/server/internal/auth"
)

// RegisterConnectRoute serves SSH over the transport the API already answers
// on: the websocket's byte stream is handed to handleConn. It is the only way
// into the SSH server — the server binds no SSH port and opens no TCP
// listener (ADR 0057) — so `discobox tools ssh` reaches it the way the CLI
// already reaches the server, and needs no new surface.
//
// The route is authenticated at the HTTP layer like any other, and admitted
// only for the CLI's own user (auth.SSHConnectAuthorizer); SSH then
// authenticates the key inside its own protocol before any channel exists.
func RegisterConnectRoute(router chi.Router, server *Server) {
	router.Get(auth.SSHConnectPath, func(w http.ResponseWriter, r *http.Request) {
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
