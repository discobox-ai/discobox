package ports

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"strconv"
	"time"
)

// Protocol is what a listening port turned out to speak.
type Protocol string

const (
	// ProtocolHTTP means the port answered an HTTP request with an HTTP
	// response — it can be proxied as a web endpoint.
	ProtocolHTTP Protocol = "http"
	// ProtocolHTTPS means the same, inside a TLS session.
	ProtocolHTTPS Protocol = "https"
	// ProtocolTCP means the port was reached and speaks something that is not
	// HTTP: a database, an SSH daemon, an HTTP/2-only server. Forwardable as
	// raw bytes, not as a web endpoint.
	ProtocolTCP Protocol = "tcp"
	// ProtocolUnknown means the port is listening but has not been classified:
	// it was discovered this tick, or the probe could not connect. Retried on
	// the next tick.
	ProtocolUnknown Protocol = "unknown"
)

const (
	probeDialTimeout = time.Second
	probeIOTimeout   = time.Second
	// probeReplyLimit is well past the longest status line worth reading; the
	// probe only ever looks at the first few bytes.
	probeReplyLimit = 512
)

// httpReplyPrefix is what every HTTP/1.x response begins with, and the only
// evidence accepted for calling a port http or https.
var httpReplyPrefix = []byte("HTTP/")

// httpBadRequest is the status an HTTPS server answers a plaintext request
// with — in plaintext, helpfully, which is why a reply that begins "HTTP/" is
// not on its own proof that the port is not TLS.
const httpBadRequest = 400

// Probe classifies a listening port by connecting to it and asking it a
// question only an HTTP server answers. There is no declaration to read
// instead: a port number implies nothing about what is behind it, so the wire
// is the only source (ADR 0046). One connection for the common case, a second
// only when the plaintext answer leaves TLS open as a possibility.
//
// It writes an HTTP request line at whatever is listening, which a non-HTTP
// service will log as a malformed request. That happens once per socket, not
// once per poll, which is the whole reason results are cached.
func Probe(ctx context.Context, target netip.AddrPort) Protocol {
	reply, reached := probeOnce(ctx, target, nil)
	if !reached {
		return ProtocolUnknown
	}
	plaintext := ProtocolTCP
	if bytes.HasPrefix(reply, httpReplyPrefix) {
		plaintext = ProtocolHTTP
	}
	if !mayBeTLS(reply, plaintext) {
		return plaintext
	}
	if tlsReply, reached := probeOnce(ctx, target, tlsProbeConfig()); reached && bytes.HasPrefix(tlsReply, httpReplyPrefix) {
		return ProtocolHTTPS
	}
	// The handshake settled it: whatever the plaintext exchange suggested
	// stands, including an HTTP server that really does answer GET / with 400.
	return plaintext
}

// mayBeTLS reports whether the plaintext exchange left it open that this port
// is really an HTTPS server, and so is worth one more connection to settle.
func mayBeTLS(reply []byte, plaintext Protocol) bool {
	if plaintext == ProtocolHTTP {
		// A server that answered in HTTP parsed the request as HTTP — unless
		// it called it malformed, which is exactly what an HTTPS server does
		// with plaintext (Go's net/http, nginx, and Apache all answer 400).
		// Any other status, 404 very much included, proves plain HTTP.
		return statusCode(reply) == httpBadRequest
	}
	// An alert record, a handshake record, or a hang-up without a word is what
	// a TLS server does with plaintext. Any other bytes are some plaintext
	// protocol that is simply not HTTP, and asking again through a handshake it
	// will not complete buys nothing.
	return len(reply) == 0 || reply[0] == 0x15 || reply[0] == 0x16
}

// statusCode reads the status out of an HTTP status line ("HTTP/1.0 400 Bad
// Request"), or 0 when the reply does not carry one.
func statusCode(reply []byte) int {
	line, _, _ := bytes.Cut(reply, []byte("\n"))
	fields := bytes.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	code, err := strconv.Atoi(string(fields[1]))
	if err != nil {
		return 0
	}
	return code
}

// tlsProbeConfig verifies nothing on purpose: this is classification, not
// trust. A sandbox dev server's certificate is self-signed by definition, and
// rejecting it would report "not https" for exactly the case this exists to
// find. No ALPN is offered either, so a server that also speaks h2 falls back
// to the HTTP/1.1 the probe can actually read.
func tlsProbeConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // classification only; see doc comment
		MinVersion:         tls.VersionTLS10,
	}
}

// probeOnce runs one request/response exchange. reached distinguishes "the port
// is not answering" (unknown, retry later) from "the port answered something",
// including answering nothing at all after accepting the connection.
func probeOnce(ctx context.Context, target netip.AddrPort, tlsConfig *tls.Config) (reply []byte, reached bool) {
	dialCtx, cancel := context.WithTimeout(ctx, probeDialTimeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(dialCtx, "tcp", target.String())
	if err != nil {
		return nil, false
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(probeIOTimeout))

	if tlsConfig != nil {
		tlsConn := tls.Client(conn, tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, false
		}
		conn = tlsConn
	}
	if _, err := conn.Write(probeRequest(target)); err != nil {
		// It accepted the connection and then would not take a request. That is
		// an answer — it is not HTTP — not a failure to reach it.
		return nil, true
	}
	return readReply(conn), true
}

// readReply reads until the first line is complete, which is as far as
// classification ever looks: an HTTP status line carries both the version
// prefix and the status, and a banner protocol has said who it is by then.
func readReply(conn net.Conn) []byte {
	var reply []byte
	buf := make([]byte, probeReplyLimit)
	for len(reply) < probeReplyLimit && !bytes.Contains(reply, []byte("\n")) {
		n, err := conn.Read(buf)
		reply = append(reply, buf[:n]...)
		if err != nil {
			break
		}
	}
	return reply
}

func probeRequest(target netip.AddrPort) []byte {
	return []byte("GET / HTTP/1.1\r\n" +
		"Host: " + target.String() + "\r\n" +
		"User-Agent: discobox-sandbox-agent (port probe)\r\n" +
		"Accept: */*\r\n" +
		"Connection: close\r\n\r\n")
}
