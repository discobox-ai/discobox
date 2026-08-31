package server

import (
	"context"
	"errors"
	"io"
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

// pumpTCPToFrames blocks until both sides are done. The websocket side is read
// in one goroutine (Input/CloseInput -> the TCP conn) while this goroutine
// reads the TCP conn and writes Stdout frames.
//
// Each direction ends on its own. A TCP conn read EOF means the target is done
// sending and says so with CloseOutput, the mirror of the CloseInput the client
// sends when it is done sending; the tunnel closes when the *websocket* side
// ends, because until then the client may still be sending data the target can
// still receive. Ending the whole tunnel on the target's EOF — which is what
// this did — cuts off exactly that.
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
			if errors.Is(err, io.EOF) {
				// Half-close, not a close: the client is told the target is
				// finished, and goes on sending until it is finished too.
				_ = frame.Write(wsConn, frame.CloseOutput, nil)
			} else {
				// A read that failed is not a read that ended. Saying so
				// beats a silent close the client can only guess at.
				_ = frame.Write(wsConn, frame.Error, []byte(err.Error()))
			}
			break
		}
	}
	<-done
	_ = wsConn.Close()
}
