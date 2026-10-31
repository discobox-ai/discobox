// Package cpchannel is the control-plane channel a server opens to a remote
// pool's agent, so the pool reaches its control plane without the server
// being reachable at all (ADR 26-10-30-875 §§1–2).
//
// The server dials the agent at the URL every other server-to-agent call
// uses, and opens a binary WebSocket with a signed request on the agent's
// API (Path), so any front that forwards WebSockets carries it (ADR
// 26-10-31-152). Over the WebSocket the two ends run TLS 1.3 of their own,
// mutual and pinned to keys each already holds — the pool's enrolled key, and
// the server's pool-agent issuer key the agent verifies every server request
// with — and a cpmux session inside it. The agent then opens a stream on the
// session for every connection its own processes make to the control plane
// (Channel.Serve), and the server hands each one to its carrier hub.
//
// The TLS is the channel's own whatever the agent's front is: a droplet's
// agent port is plain HTTP, and an edge that terminates HTTPS would see
// cleartext. No CA is involved; each end accepts exactly one key.
package cpchannel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Subprotocol names the channel in its WebSocket handshake. Both ends require
// it, so a channel never runs against something that speaks another protocol.
const Subprotocol = "discobox-control-plane.v1"

// Action is the last segment of the channel's route on the agent's API.
const Action = "control-plane-channel"

// handshakeTimeout bounds the TLS handshake on a fresh WebSocket. Both ends
// are already talking; anything slower is a peer that is not going to finish.
const handshakeTimeout = 30 * time.Second

// Path is the channel's route on pool poolID's agent API, under the prefix
// the agent authorizes every server request against.
func Path(projectID, poolID string) string {
	return "/api/project/" + url.PathEscape(projectID) + "/pool/" + url.PathEscape(poolID) + "/" + Action
}

// certificate is a self-signed certificate for key: something to carry the
// public key through the handshake, which the far end pins. Its other fields
// mean nothing and nobody reads them.
func certificate(key ed25519.PrivateKey) (tls.Certificate, error) {
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "discobox control-plane channel"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("make the channel's certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// pinned accepts a peer whose leaf certificate carries want, and nothing
// else. It is the whole of the channel's peer authentication: with no CA,
// everything else in the certificate is the peer's own say-so. It is a
// VerifyConnection, which runs on every handshake, a resumed one included;
// the configurations also turn resumption off, since a channel is one
// long-lived connection with nothing to resume.
func pinned(want ed25519.PublicKey, who string) func(tls.ConnectionState) error {
	return func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return fmt.Errorf("the %s presented no certificate", who)
		}
		got, ok := state.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
		if !ok || !got.Equal(want) {
			return fmt.Errorf("the %s's certificate is not for its key", who)
		}
		return nil
	}
}

// poolTLS is the pool end's configuration: the TLS server, presenting the
// pool's key and accepting only the control plane's.
func poolTLS(poolKey ed25519.PrivateKey, controlPlane ed25519.PublicKey) (*tls.Config, error) {
	cert, err := certificate(poolKey)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		Certificates:           []tls.Certificate{cert},
		ClientAuth:             tls.RequireAnyClientCert,
		VerifyConnection:       pinned(controlPlane, "control plane"),
		SessionTicketsDisabled: true,
	}, nil
}

// serverTLS is the server end's configuration: the TLS client, presenting the
// server's issuer key and accepting only the pool's.
func serverTLS(serverKey ed25519.PrivateKey, pool ed25519.PublicKey) (*tls.Config, error) {
	cert, err := certificate(serverKey)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		// The chain is not verified against any CA because there is none:
		// VerifyConnection pins the pool's key, which is stricter.
		InsecureSkipVerify:     true, //nolint:gosec // G402: pinned to the pool's enrolled key below.
		VerifyConnection:       pinned(pool, "pool"),
		SessionTicketsDisabled: true,
	}, nil
}

// Dial opens a channel to a pool's agent: the WebSocket at baseURL on
// transport, authorized with token (a server token that may open the
// channel), then the pinned TLS over it. The connection it returns is the TLS
// one, which the caller runs its cpmux client over; closing it closes the
// WebSocket.
//
// The transport is taken rather than a client because a client's timeout
// would cut the channel off at its deadline: the channel lives as long as
// the pool.
func Dial(ctx context.Context, transport http.RoundTripper, baseURL, projectID, poolID, token string, serverKey ed25519.PrivateKey, pool ed25519.PublicKey) (net.Conn, error) {
	config, err := serverTLS(serverKey, pool)
	if err != nil {
		return nil, err
	}
	ws, resp, err := websocket.Dial(ctx, strings.TrimRight(baseURL, "/")+Path(projectID, poolID), &websocket.DialOptions{
		HTTPClient:      &http.Client{Transport: http1(transport)},
		HTTPHeader:      http.Header{"Authorization": []string{"Bearer " + token}},
		Subprotocols:    []string{Subprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			var body []byte
			if resp.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(resp.Body, 512))
				_ = resp.Body.Close()
			}
			return nil, fmt.Errorf("open the control-plane channel: the pool agent answered %s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		return nil, fmt.Errorf("open the control-plane channel: %w", err)
	}
	if ws.Subprotocol() != Subprotocol {
		_ = ws.CloseNow()
		return nil, fmt.Errorf("open the control-plane channel: the pool agent does not speak %s", Subprotocol)
	}
	// The channel outlives the dial's context: it is closed through the
	// connection returned, and the caller's session ends with it.
	conn := tls.Client(websocket.NetConn(context.Background(), ws, websocket.MessageBinary), config)
	handshake, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	if err := conn.HandshakeContext(handshake); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("the control-plane channel's TLS: %w", err)
	}
	return conn, nil
}

// http1 is transport speaking HTTP/1.1 alone. A WebSocket opens with an
// HTTP/1.1 Upgrade, which HTTP/2 does not have, so over an HTTPS front that
// offers HTTP/2 (boxd's edge) a transport that negotiated h2 could never open
// the channel. A transport that is not an *http.Transport is the caller's to
// have made HTTP/1.1.
func http1(transport http.RoundTripper) http.RoundTripper {
	t, ok := transport.(*http.Transport)
	if !ok {
		return transport
	}
	t = t.Clone()
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	t.Protocols = &protocols
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	// A TLS configuration naming its own protocols is what ALPN offers,
	// whatever Protocols says; offering h2 there lets the front choose it.
	if t.TLSClientConfig != nil {
		t.TLSClientConfig = t.TLSClientConfig.Clone()
		t.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	return t
}

// accept opens the WebSocket of one request on the pool end and completes
// the pinned TLS over it, returning the TLS connection. The request has
// already been authorized as the server's; the TLS proves it is the server at
// the other end of this connection too, not only who asked for it. A request
// that is not a WebSocket is answered 426 by websocket.Accept.
func accept(w http.ResponseWriter, r *http.Request, config *tls.Config) (net.Conn, error) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{Subprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, fmt.Errorf("open the channel's WebSocket: %w", err)
	}
	if ws.Subprotocol() != Subprotocol {
		_ = ws.Close(websocket.StatusPolicyViolation, "the "+Subprotocol+" subprotocol is required")
		return nil, fmt.Errorf("the channel's WebSocket did not ask for %s", Subprotocol)
	}
	// The request's context ends when the handler returns, and the channel
	// outlives it: it is closed through the connection returned.
	tlsConn := tls.Server(websocket.NetConn(context.Background(), ws, websocket.MessageBinary), config)
	handshake, cancel := context.WithTimeout(r.Context(), handshakeTimeout)
	defer cancel()
	if err := tlsConn.HandshakeContext(handshake); err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("the control-plane channel's TLS: %w", err)
	}
	return tlsConn, nil
}
