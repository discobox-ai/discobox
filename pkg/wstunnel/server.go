package wstunnel

import (
	"context"
	"net"
	"net/http"

	"github.com/coder/websocket"
)

// Handler accepts tunnels and hands each stream they carry to serve, in a
// goroutine of its own. serve owns the stream until it closes it; closing it
// ends the stream and readies the tunnel for the client's next one. Every
// tunnel stops when ctx ends.
func Handler(ctx context.Context, serve func(net.Conn)) http.Handler {
	lim := currentLimits()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:    []string{Subprotocol},
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			return // Accept answered the request
		}
		if ws.Subprotocol() != Subprotocol {
			_ = ws.Close(websocket.StatusPolicyViolation, "the "+Subprotocol+" subprotocol is required")
			return
		}
		t := newTunnel(ctx, lim, ws, true, addr(r.Host), addr(r.RemoteAddr))
		t.onStream = func(s *Stream) { go serve(s) }
		t.armIdle(lim.firstStreamTimeout)
		t.run()
	})
}
