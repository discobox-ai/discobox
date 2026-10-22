package proxy

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/elazarl/goproxy"
	"go.opentelemetry.io/otel/attribute"

	"github.com/discobox-ai/discobox/pkg/hostscope"
	"github.com/discobox-ai/discobox/pkg/wire"
	"github.com/discobox-ai/discobox/proxy/internal/audit"
)

// PoolHostConfig names a host the pool answers its sandboxes at itself: its
// Git origins (ADR 0126 §4, ADR 26-10-08-561) and its bundle assets (ADR
// 26-10-16-423 §13). A request for Host never reaches the internet: the proxy
// forwards it to Upstream, a listener of the pool's own, and says which client
// its certificate named.
type PoolHostConfig struct {
	// Host is the name a sandbox sends to, without a port.
	Host string
	// Upstream is where a request for Host is forwarded to, with the
	// request's path and query and nothing else of where the sandbox sent it:
	// a wire URL, plain HTTP on loopback or a Unix socket of the pool's. It
	// is plain HTTP either way: the hop that needed authenticating was the
	// sandbox's, and the proxy has done that. A listener that takes
	// PoolClientHeader as the only word on who is asking belongs on a
	// socket: a loopback port is one a sandbox can send its own header to as
	// ordinary egress, from this proxy's network namespace.
	Upstream string
	// ReadOnly refuses every method but GET and HEAD at the proxy, so a write
	// never reaches the upstream at all.
	ReadOnly bool
}

// PoolClientHeader is, on a request forwarded to a PoolHostConfig's Upstream,
// the client the proxy authenticated by its certificate: the sandbox ID.
// Whatever a sandbox sent under this name is removed first, so the header is
// only ever the proxy's word.
const PoolClientHeader = "X-Discobox-Pool-Client"

// poolHostHTTPTimeout bounds one forwarded exchange, a clone of a large
// repository or a large layer included, so a stalled upstream does not hold a
// connection forever.
const poolHostHTTPTimeout = 30 * time.Minute

// poolHosts answers the hosts the pool serves itself. A nil one answers none.
type poolHosts struct {
	hosts map[string]*poolHost
}

// poolHost is one host the pool answers, and where it forwards to: base, the
// upstream's HTTP base URL, reached by client over the upstream's transport.
type poolHost struct {
	host     string
	base     string
	client   *http.Client
	readOnly bool
}

func newPoolHosts(configs []PoolHostConfig) (*poolHosts, error) {
	hosts := map[string]*poolHost{}
	for _, cfg := range configs {
		host := hostscope.Normalize(cfg.Host)
		if host == "" {
			return nil, fmt.Errorf("pool host with upstream %q names no host", cfg.Upstream)
		}
		if _, ok := hosts[host]; ok {
			return nil, fmt.Errorf("pool host %s is named twice", host)
		}
		upstream, err := wire.Parse(strings.TrimSpace(cfg.Upstream))
		if err != nil {
			return nil, fmt.Errorf("pool host %s upstream: %w", host, err)
		}
		dial, err := upstream.DialContext()
		if err != nil {
			return nil, fmt.Errorf("pool host %s upstream: %w", host, err)
		}
		hosts[host] = &poolHost{
			host: host,
			base: strings.TrimSuffix(upstream.BaseURL(), "/"),
			// Straight to the upstream, never through a proxy of its own: the
			// upstream is this pool's own listener.
			client: &http.Client{
				Timeout:   poolHostHTTPTimeout,
				Transport: &http.Transport{Proxy: nil, DialContext: dial, ForceAttemptHTTP2: false},
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			},
			readOnly: cfg.ReadOnly,
		}
	}
	if len(hosts) == 0 {
		return nil, nil
	}
	return &poolHosts{hosts: hosts}, nil
}

// answering is the pool host host names, or nil when the pool answers none.
func (p *poolHosts) answering(host string) *poolHost {
	if p == nil {
		return nil
	}
	return p.hosts[hostscope.Normalize(host)]
}

// targetFor is where a request goes on the upstream: its path and query, and
// nothing else of the URL the sandbox sent. A request-target that is not a
// path is refused rather than read.
func (h *poolHost) targetFor(in *url.URL) (string, error) {
	if in == nil || in.Opaque != "" || in.User != nil || !strings.HasPrefix(in.Path, "/") {
		return "", fmt.Errorf("the request names no path on %s", h.host)
	}
	// The escaped path, so a segment carrying an escaped slash reaches the
	// upstream as the one segment its router will match it as.
	target := h.base + in.EscapedPath()
	if in.RawQuery != "" {
		target += "?" + in.RawQuery
	}
	return target, nil
}

// servePoolHost forwards a request for a pool host to the pool's listener for
// it as the client its certificate names, and answers with what that listener
// says. The response goes back through the ordinary response path, which
// audits it like any other exchange; only a request the proxy itself refuses
// is recorded here, as blocked.
func (h *httpProxy) servePoolHost(req *http.Request, meta *requestMeta, client clientIdentity, host *poolHost) *http.Response {
	refuse := func(status int, reason string) *http.Response {
		meta.answered = true
		meta.span.SetAttributes(attribute.Bool("proxy.blocked", true), attribute.Int("http.response.status_code", status))
		h.audit.RecordHTTP(audit.HTTPEvent{
			Context:        meta.ctx,
			Time:           time.Now().UTC(),
			ClientID:       client.ID,
			ClientSubject:  client.Subject,
			ClientSerial:   client.Serial,
			Method:         req.Method,
			URL:            requestURL(req),
			Host:           req.Host,
			Status:         status,
			Blocked:        true,
			BlockedReason:  host.host + ": " + reason,
			RequestHeaders: req.Header,
		})
		meta.span.End()
		return goproxy.NewResponse(req, goproxy.ContentTypeText, status, "blocked by proxy: "+reason+"\n")
	}
	// The certificate's common name is the sandbox; a client the proxy cannot
	// name is answered nothing the pool holds.
	if client.ID == "" {
		return refuse(http.StatusForbidden, "the client certificate names no sandbox")
	}
	if host.readOnly && req.Method != http.MethodGet && req.Method != http.MethodHead {
		return refuse(http.StatusMethodNotAllowed, req.Method+" is not a read, and "+host.host+" is read-only")
	}
	target, err := host.targetFor(req.URL)
	if err != nil {
		return refuse(http.StatusBadRequest, err.Error())
	}
	out, err := http.NewRequestWithContext(meta.ctx, req.Method, target, req.Body)
	if err != nil {
		return refuse(http.StatusBadRequest, err.Error())
	}
	out.Header = req.Header.Clone()
	out.Header.Del("Proxy-Authorization")
	out.Header.Del(PoolClientHeader)
	out.Header.Set(PoolClientHeader, client.ID)
	out.ContentLength = req.ContentLength
	//nolint:bodyclose // Handed to the proxy, which writes it to the sandbox and closes it.
	resp, err := host.client.Do(out)
	if err != nil {
		// The ordinary response path records this 502 as the upstream's
		// failure, the way it does a gate whose control plane did not answer.
		return goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusBadGateway, host.host+" unreachable: "+err.Error())
	}
	resp.Request = req
	return resp
}
