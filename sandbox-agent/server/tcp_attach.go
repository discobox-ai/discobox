package server

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/execstream/frame"
)

// tcpDialTimeout bounds how long attachTCPTunnelHTTP waits to connect before
// rejecting the upgrade, so a caller gets a prompt, clean failure instead of a
// silently-closed websocket.
const tcpDialTimeout = 10 * time.Second

// attachTCPTunnelHTTP serves an SSH direct-tcpip channel's tunnel (ADR 0024
// §3): dial host:port from inside the sandbox-agent process, which shares the
// sandbox's network namespace, so "localhost" resolves the way the user
// meant it — unlike the pool-agent's container-IP-only /http/{port} route.
//
// Unlike the exec attach websocket (which relays a shim's own already-framed
// socket byte-for-byte), a TCP connection carries no framing of its own: this
// handler actively wraps/unwraps execstream/frame Input/Stdout/CloseInput
// around the raw bytes, which is also what lets it express a TCP half-close
// (ADR 0024 §4) that a raw-bytes websocket could not.
func (h *handler) attachTCPTunnelHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	portText := r.URL.Query().Get("port")
	port, err := strconv.Atoi(portText)
	if host == "" || err != nil || port < 1 || port > 65535 {
		writeJSON(w, http.StatusBadRequest, sandboxapi.ErrorResponse{Error: "host and a valid port query parameter are required"})
		return
	}

	dialCtx, cancel := context.WithTimeout(r.Context(), tcpDialTimeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(dialCtx, "tcp", net.JoinHostPort(host, portText))
	if err != nil {
		// Reject before upgrading, matching what a real sshd does for a
		// refused -L target: a clean error, not a silently-closed websocket.
		writeJSON(w, http.StatusBadGateway, sandboxapi.ErrorResponse{Error: "dial " + net.JoinHostPort(host, portText) + ": " + err.Error()})
		return
	}
	defer conn.Close()

	wsConn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer wsConn.Close(websocket.StatusNormalClosure, "done")
	ctx := r.Context()
	wsNetConn := websocket.NetConn(ctx, wsConn, websocket.MessageBinary)
	defer wsNetConn.Close()

	pumpTCPToFrames(conn, wsNetConn)
}

// pumpTCPToFrames blocks until either side is done. The websocket side is
// read in one goroutine (Input/CloseInput -> the TCP conn) while this
// goroutine reads the TCP conn and writes Stdout frames; a TCP conn read EOF
// ends the tunnel outright, since a TCP pipe has no exit code to report and
// therefore no Exit frame to send.
func pumpTCPToFrames(tcpConn net.Conn, wsConn net.Conn) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			f, err := frame.Read(wsConn)
			if err != nil {
				_ = tcpConn.Close()
				return
			}
			switch f.Type {
			case frame.Input:
				if _, err := tcpConn.Write(f.Payload); err != nil {
					return
				}
			case frame.CloseInput:
				// The SSH client is done sending, not done reading: a TCP
				// half-close, not a full close (ADR 0024 §4's reason for
				// reusing execstream/frame instead of raw bytes).
				if cw, ok := tcpConn.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
			}
		}
	}()

	buf := make([]byte, 32*1024)
	for {
		n, err := tcpConn.Read(buf)
		if n > 0 {
			if werr := frame.Write(wsConn, frame.Stdout, buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	_ = wsConn.Close()
	<-done
}
