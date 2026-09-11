package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"

	"github.com/coder/websocket"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/execstream/frame"
)

// udpDatagramLimit is the largest datagram the tunnel carries in either
// direction: the most a UDP payload can be (65507 over IPv4, 65527 over IPv6),
// rounded up. It sizes the read buffer, so nothing the target sends is
// truncated, and the websocket's message limit, so nothing the client sends is
// refused — a frame's payload arrives as one message of its own.
const udpDatagramLimit = 64 * 1024

// udpDialTimeout bounds name resolution, the only part of a UDP dial that can
// wait on anything.
const udpDialTimeout = 10 * time.Second

// attachUDPTunnelHTTP is tcp/attach's datagram twin (ADR 0109 §4): it opens a
// UDP socket connected to host:port from inside the sandbox's network
// namespace and bridges it to execstream/frame, one datagram per frame. Input
// frames are datagrams sent; Stdout frames are datagrams received.
//
// The socket is connected rather than bound, so the kernel delivers only the
// target's replies and every tunnel has a source port of its own — which is
// what lets a server here tell the clients of a forward apart.
//
// Unlike a TCP dial, nothing here can learn whether anything is listening:
// connecting a UDP socket sends nothing. So the only dial failure is one that
// never reaches the network — a name that does not resolve — and a target that
// is not there shows up later, as refused datagrams (see pumpUDPToFrames).
func (h *handler) attachUDPTunnelHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	portText := r.URL.Query().Get("port")
	port, err := strconv.Atoi(portText)
	if host == "" || err != nil || port < 1 || port > 65535 {
		writeJSON(w, http.StatusBadRequest, sandboxapi.ErrorResponse{Error: "host and a valid port query parameter are required"})
		return
	}

	dialCtx, cancel := context.WithTimeout(r.Context(), udpDialTimeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(dialCtx, "udp", net.JoinHostPort(host, portText))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, sandboxapi.ErrorResponse{Error: "dial udp " + net.JoinHostPort(host, portText) + ": " + err.Error()})
		return
	}
	defer conn.Close()

	wsConn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer wsConn.Close(websocket.StatusNormalClosure, "done")
	// A flow is a client of this sandbox the way a TCP tunnel is, and no shim
	// counts it: hold the idle stop off for as long as it is open (ADR 0108 §2).
	// The forwarder closes a flow that has gone quiet, so a hold here outlives
	// the traffic by that timeout and no longer.
	release := h.autostop.Hold("udp tunnel to " + net.JoinHostPort(host, portText))
	defer release()
	wsConn.SetReadLimit(udpDatagramLimit)
	wsNetConn := websocket.NetConn(r.Context(), wsConn, websocket.MessageBinary)
	defer wsNetConn.Close()

	pumpUDPToFrames(conn, wsNetConn)
}

// pumpUDPToFrames blocks until the tunnel is done. The websocket side is read
// in one goroutine (each Input frame -> one datagram), while this goroutine
// reads datagrams and writes each as one Stdout frame.
//
// A datagram has no half-close to carry, so CloseInput is ignored and nothing
// here ever sends CloseOutput: the tunnel ends when the websocket does, which
// is the client deciding the flow is over.
//
// A refused datagram does not end it either. ICMP port unreachable comes back
// as ECONNREFUSED on the socket's next read or write, and for UDP that is the
// normal state of a server that is restarting, or not up yet: the next
// datagram may well be answered. Any other read failure is reported with an
// Error frame, since a silent close is one the client can only guess at.
func pumpUDPToFrames(udpConn net.Conn, wsConn net.Conn) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Closing the socket is what ends the read loop below once the client
		// has gone.
		defer udpConn.Close()
		for {
			f, err := frame.Read(wsConn)
			if err != nil {
				return
			}
			if f.Type != frame.Input {
				continue
			}
			// A datagram that could not be sent is lost, which is a thing UDP
			// does; only a socket that is gone ends the tunnel.
			if _, err := udpConn.Write(f.Payload); errors.Is(err, net.ErrClosed) {
				return
			}
		}
	}()

	buf := make([]byte, udpDatagramLimit)
	for {
		n, err := udpConn.Read(buf)
		if err != nil {
			if errors.Is(err, syscall.ECONNREFUSED) {
				continue
			}
			if !errors.Is(err, net.ErrClosed) {
				_ = frame.Write(wsConn, frame.Error, []byte(err.Error()))
			}
			break
		}
		if werr := frame.Write(wsConn, frame.Stdout, buf[:n]); werr != nil {
			break
		}
	}
	_ = wsConn.Close()
	<-done
}
