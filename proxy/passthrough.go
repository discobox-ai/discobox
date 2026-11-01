package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/elazarl/goproxy"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/discobox-ai/discobox/proxy/internal/audit"
)

// A passthrough tunnel (ADR 26-10-31-819) is an allowed CONNECT to a host the
// client's passthrough names, spliced to the origin instead of intercepted.
// Nothing inside it is read: no swap, no raw-value watch, no 421, no cache,
// no spool, no pin. What is visible from outside — the dial, the client's
// ClientHello, the bytes each way — is audited as an open row and a close
// row sharing a tunnel ID.

// helloPeekWait bounds how long a tunnel waits for the client's first flight
// before its open row is written without it. The origin's bytes already flow
// to the client meanwhile, so a protocol whose server speaks first is not
// held up; it only goes unrecorded.
const helloPeekWait = 5 * time.Second

// passthroughConnect is the CONNECT action that splices host unread for the
// passthrough entry that matched it.
func (h *httpProxy) passthroughConnect(entry string) *goproxy.ConnectAction {
	return &goproxy.ConnectAction{
		Action: goproxy.ConnectHijack,
		Hijack: func(req *http.Request, client net.Conn, _ *goproxy.ProxyCtx) {
			h.servePassthrough(req, client, entry)
		},
	}
}

// passthroughTunnel is one spliced CONNECT, for its audit rows and span.
type passthroughTunnel struct {
	ctx       context.Context
	span      trace.Span
	client    clientIdentity
	authority string
	start     time.Time
	record    audit.Tunnel
}

func (h *httpProxy) servePassthrough(req *http.Request, client net.Conn, entry string) {
	defer func() { _ = client.Close() }()
	authority := req.URL.Host
	if authority == "" {
		authority = req.Host
	}
	addr := authority
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(strings.Trim(addr, "[]"), "443")
	}
	identity := h.clientIdentity(req)
	ctx, span := proxyTracer().Start(req.Context(), "proxy.http.passthrough", trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(append(clientAttrs(identity),
			attribute.String("proxy.http.connect_host", authority),
			attribute.Bool("proxy.http.passthrough", true),
			attribute.String("proxy.http.passthrough_entry", entry),
		)...),
	)
	defer span.End()
	t := &passthroughTunnel{
		ctx: ctx, span: span, client: identity, authority: authority, start: time.Now(),
		record: audit.Tunnel{ID: newTunnelID(), Entry: entry, UpstreamProxy: h.upstreamProxyFor(addr)},
	}

	target, err := h.dialPassthrough(ctx, addr)
	if err != nil {
		recordSpanError(span, err)
		t.record.Phase = audit.TunnelPhaseFailed
		t.record.Error = err.Error()
		writeConnectStatus(req, client, http.StatusBadGateway, t.record.UpstreamProxy,
			fmt.Sprintf("passthrough to %s failed: %v\n", authority, err))
		h.recordTunnel(t, http.StatusBadGateway)
		return
	}
	defer func() { _ = target.Close() }()
	if remote := target.RemoteAddr(); remote != nil {
		t.record.DialedAddress = remote.String()
	}
	if !writeConnectStatus(req, client, http.StatusOK, "", "") {
		return
	}

	var c2s, s2c atomic.Int64
	// Each direction reports which side's stream it read to its end, and
	// why. Only the first says how the tunnel ended: the second ends
	// because of it, often writing to a side that already hung up.
	type end struct {
		side string
		err  error
	}
	ended := make(chan end, 2)

	// The origin's bytes flow from the start, so the peek below never holds
	// up a server that speaks first.
	go func() {
		_, err := io.Copy(countingWriter{client, &s2c}, target)
		closeWrite(client)
		ended <- end{side: "server", err: err}
	}()

	hello, first := peekClientHello(ctx, client, helloPeekWait)
	t.record.TLS = hello != nil
	if hello != nil {
		t.record.ServerName = hello.ServerName
		t.record.ALPN = strings.Join(hello.SupportedProtos, ",")
		versions := make([]string, 0, len(hello.SupportedVersions))
		for _, v := range hello.SupportedVersions {
			versions = append(versions, tls.VersionName(v))
		}
		t.record.TLSVersions = strings.Join(versions, ",")
		t.record.ServerNameMismatch = hello.ServerName != "" && authorityHostname(hello.ServerName) != authorityHostname(authority)
	}
	t.record.Phase = audit.TunnelPhaseOpen
	h.recordTunnel(t, http.StatusOK)

	go func() {
		_, err := io.Copy(countingWriter{target, &c2s}, io.MultiReader(bytes.NewReader(first), client))
		closeWrite(target)
		ended <- end{side: "client", err: err}
	}()

	finished := <-ended
	<-ended
	t.record.Phase = audit.TunnelPhaseClose
	t.record.ClosedBy = finished.side
	t.record.C2SBytes = c2s.Load()
	t.record.S2CBytes = s2c.Load()
	if finished.err != nil && !errors.Is(finished.err, net.ErrClosed) {
		t.record.Error = finished.err.Error()
	}
	h.recordTunnel(t, http.StatusOK)
}

// internalHost reports whether authority names the gate or a pool host
// however it is spelled — a trailing dot, another case — which the checks
// before the filter match only as written. Passthrough never splices to one:
// what reaches them is the gate's, or the pool listener's, to decide.
func (h *httpProxy) internalHost(authority string) bool {
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		host, port = authority, ""
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	canonical := host
	if port != "" {
		canonical = net.JoinHostPort(host, port)
	}
	return h.swapper.IsGate(canonical) || h.poolHosts.answering(canonical) != nil
}

// dialPassthrough dials a tunnel's origin the way an accepted CONNECT is
// dialed: through the upstream proxy's ConnectDial when one is configured
// (which honors its exemptions), otherwise by the destination dialer, which
// refuses the pool's own loopback.
func (h *httpProxy) dialPassthrough(ctx context.Context, addr string) (net.Conn, error) {
	if h.proxy.ConnectDial != nil {
		return h.proxy.ConnectDial("tcp", addr)
	}
	return h.destinations.DialContext(ctx, "tcp", addr)
}

// upstreamProxyFor is the upstream proxy a tunnel to addr goes through,
// redacted, or "" when it goes direct.
func (h *httpProxy) upstreamProxyFor(addr string) string {
	if h.proxy.Tr == nil || h.proxy.Tr.Proxy == nil {
		return ""
	}
	upstream, err := h.proxy.Tr.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
	if err != nil || upstream == nil {
		return ""
	}
	return upstream.Redacted()
}

func (h *httpProxy) recordTunnel(t *passthroughTunnel, status int) {
	t.span.SetAttributes(
		attribute.String("proxy.http.tunnel_id", t.record.ID),
		attribute.String("proxy.http.tunnel_phase", t.record.Phase),
		// The ClientHello's own text (SNI, ALPN) is in the audit row alone,
		// where the recorder redacts it; a span has no redactor.
		attribute.Bool("proxy.http.tunnel_tls", t.record.TLS),
		attribute.Bool("proxy.http.tunnel_server_name_mismatch", t.record.ServerNameMismatch),
		attribute.Int64("proxy.http.tunnel_c2s_bytes", t.record.C2SBytes),
		attribute.Int64("proxy.http.tunnel_s2c_bytes", t.record.S2CBytes),
		attribute.Int("http.response.status_code", status),
	)
	var duration time.Duration
	if t.record.Phase == audit.TunnelPhaseClose {
		duration = time.Since(t.start)
	}
	h.audit.RecordHTTP(audit.HTTPEvent{
		Context:       t.ctx,
		Time:          time.Now().UTC(),
		ClientID:      t.client.ID,
		ClientSubject: t.client.Subject,
		ClientSerial:  t.client.Serial,
		Method:        http.MethodConnect,
		Host:          t.authority,
		URL:           t.authority,
		Status:        status,
		Duration:      duration,
		Tunnel:        t.record,
	})
}

// writeConnectStatus answers a hijacked CONNECT. Over HTTP/1 the status line
// is written to the connection; over HTTP/2 the stream's ResponseWriter
// carries it. It reports whether the answer was sent.
func writeConnectStatus(req *http.Request, client net.Conn, status int, upstream, body string) bool {
	if req.ProtoMajor == 2 {
		stream, ok := client.(interface{ ResponseWriter() http.ResponseWriter })
		if !ok {
			return false
		}
		w := stream.ResponseWriter()
		if upstream != "" {
			w.Header().Set(UpstreamProxyHeader, upstream)
		}
		w.WriteHeader(status)
		if body != "" {
			_, _ = io.WriteString(w, body)
		}
		return http.NewResponseController(w).Flush() == nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	if status != http.StatusOK {
		if upstream != "" {
			fmt.Fprintf(&b, "%s: %s\r\n", UpstreamProxyHeader, upstream)
		}
		fmt.Fprintf(&b, "Content-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n", len(body))
	}
	b.WriteString("\r\n")
	b.WriteString(body)
	_, err := io.WriteString(client, b.String())
	return err == nil
}

// peekClientHello reads the client's first flight for up to wait and parses
// it as a TLS ClientHello. It returns the hello, nil when the flight is not
// one or did not arrive in time, and every byte it read, which the caller
// sends on first: nothing is consumed and nothing is written to the client.
func peekClientHello(ctx context.Context, conn net.Conn, wait time.Duration) (*tls.ClientHelloInfo, []byte) {
	var read bytes.Buffer
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	var hello *tls.ClientHelloInfo
	_ = tls.Server(readOnlyConn{Conn: conn, read: &read}, &tls.Config{
		GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
			copied := *info
			copied.Conn = nil
			hello = &copied
			return nil, errHelloPeeked
		},
	}).HandshakeContext(ctx)
	return hello, read.Bytes()
}

var errHelloPeeked = errors.New("client hello peeked")

// readOnlyConn records what is read from Conn, and drops what is written, so
// the TLS server that parses a ClientHello never answers the client.
type readOnlyConn struct {
	net.Conn
	read *bytes.Buffer
}

func (c readOnlyConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Write(p[:n])
	return n, err
}

func (readOnlyConn) Write(p []byte) (int, error) { return len(p), nil }

func (readOnlyConn) Close() error { return nil }

type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// closeWrite ends one direction of a tunnel: a half-close where the
// connection has one, so the other direction can still finish, and a full
// close where it does not.
func closeWrite(conn net.Conn) {
	if half, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()
		return
	}
	_ = conn.Close()
}

func newTunnelID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "tun_" + hex.EncodeToString(b[:])
}
