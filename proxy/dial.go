package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"

	"github.com/elazarl/goproxy"
	"go.opentelemetry.io/otel/attribute"

	"github.com/discobox-ai/discobox/proxy/internal/audit"
)

// errOwnLoopback is a destination that resolved to this proxy's own loopback.
// The proxy runs in the pool's network namespace, beside services that listen
// there for the pool alone — the build registry among them — so a sandbox's
// egress never reaches it, whatever the allowlist says and however the name
// was spelled (ADR 26-10-29-160 §3). The pool's own services a sandbox may
// reach are its pool hosts, which the proxy forwards to itself.
var errOwnLoopback = errors.New("the destination is the pool's own loopback, which no sandbox reaches through its proxy")

// destinationDialer dials every upstream a client's request goes to, and
// refuses a connection whose resolved address is this host's own: loopback
// or unspecified, in either family or IPv4-mapped. It decides on the address
// the dialer actually connects to, after resolution, so a name that resolves
// to 127.0.0.1 is refused as surely as the literal.
//
// The upstream proxy (via) is the exception: it is this proxy's own next hop,
// not a client's destination, and in a pool nested in a sandbox it is that
// sandbox's forwarder on loopback. What goes through it is resolved there.
type destinationDialer struct {
	// allowLoopback turns the refusal off. Only tests set it, through
	// Config.AllowLoopbackDestinations, for upstreams they serve on loopback.
	allowLoopback bool
	// via is the upstream proxy's address as the transport dials it, or "".
	via string
}

// DialContext is the transport's, CONNECT's and SOCKS's dialer.
func (d *destinationDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var dialer net.Dialer
	if !d.allowLoopback && addr != d.via {
		dialer.Control = refuseOwnAddress
	}
	return dialer.DialContext(ctx, network, addr)
}

// refuseOwnAddress is a net.Dialer Control: it sees the address each attempt
// connects to.
func refuseOwnAddress(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("dial %s: %w", address, err)
	}
	if ownAddress(addrPort.Addr()) {
		return fmt.Errorf("%s: %w", address, errOwnLoopback)
	}
	return nil
}

// ownAddress reports whether addr is this host's own: loopback, or the
// unspecified address, which a connect reaches the local host by.
func ownAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	return addr.IsLoopback() || addr.IsUnspecified()
}

// proxyDialAddress is the address a transport dials to reach the proxy at
// upstream: its host and port, with the scheme's default port when it names
// none.
func proxyDialAddress(upstream *url.URL) string {
	if upstream == nil {
		return ""
	}
	if upstream.Port() != "" {
		return upstream.Host
	}
	port := "80"
	switch upstream.Scheme {
	case "https":
		port = "443"
	case "socks5", "socks5h":
		port = "1080"
	}
	return net.JoinHostPort(upstream.Hostname(), port)
}

// refuseOwnLoopback answers a request whose destination resolved to the
// pool's own loopback with a 403, audited once as blocked. Nothing was sent:
// the connection is what was refused.
//
// req is the request as the sandbox sent it, which is what is recorded;
// outbound is what was sent on, whose host is the one refused.
func (h *httpProxy) refuseOwnLoopback(req, outbound *http.Request, meta *requestMeta, cause error) *http.Response {
	meta.answered = true
	meta.span.SetAttributes(attribute.Bool("proxy.blocked", true), attribute.Int("http.response.status_code", http.StatusForbidden))
	h.audit.RecordHTTP(audit.HTTPEvent{
		Context:              meta.ctx,
		Time:                 time.Now().UTC(),
		ClientID:             meta.client.ID,
		ClientSubject:        meta.client.Subject,
		ClientSerial:         meta.client.Serial,
		Method:               req.Method,
		URL:                  meta.url(req),
		Host:                 req.Host,
		Status:               http.StatusForbidden,
		Blocked:              true,
		BlockedReason:        "destination is the pool's own loopback: " + cause.Error(),
		AppliedRuleID:        meta.appliedRuleID,
		AppliedPattern:       meta.appliedPattern,
		AppliedHeaders:       meta.appliedHeaders,
		ForwardedURL:         meta.forwardedURL,
		SwappedSecretIDs:     meta.swappedSecretIDs,
		RedactRequestHeaders: meta.redactRequestHeaders(),
		RequestHeaders:       req.Header,
	})
	meta.span.End()
	return goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusForbidden,
		fmt.Sprintf("blocked by proxy: %s resolves to the pool's own loopback, which no sandbox reaches through its proxy\n", outbound.Host))
}
