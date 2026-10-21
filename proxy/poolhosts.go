package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/elazarl/goproxy"
	"go.opentelemetry.io/otel/attribute"

	"github.com/discobox-ai/discobox/hostscope"
	"github.com/discobox-ai/discobox/proxy/internal/audit"
)

// OriginsConfig names the host a pool serves its sandboxes' Git origins at
// (ADR 0126 §4, ADR 26-10-08-561). A request for Host never reaches the
// internet: the proxy forwards it to Upstream, the pool's own origin listener,
// and says which client its certificate named. Empty Host is no origins host.
type OriginsConfig struct {
	// Host is the name a sandbox's origin remote names, without a port.
	Host string
	// Upstream is the base URL a request for Host is forwarded to, with the
	// request's path and query and nothing else of where the sandbox sent it.
	// It is plain HTTP on loopback: the hop that needed authenticating was
	// the sandbox's, and the proxy has done that.
	Upstream string
}

// OriginClientHeader is, on a request forwarded to OriginsConfig.Upstream, the
// client the proxy authenticated by its certificate: the sandbox ID. Whatever
// a sandbox sent under this name is removed first, so the header is only ever
// the proxy's word.
const OriginClientHeader = "X-Discobox-Origin-Client"

// originsHTTPTimeout bounds one forwarded exchange, a clone of a large
// repository included, so a stalled origin does not hold a connection forever.
const originsHTTPTimeout = 30 * time.Minute

// originForwarder answers the origins host. A nil one answers nothing.
type originForwarder struct {
	host   string
	target *url.URL
	client *http.Client
}

func newOriginForwarder(cfg OriginsConfig) (*originForwarder, error) {
	host := hostscope.Normalize(cfg.Host)
	if host == "" {
		return nil, nil
	}
	target, err := url.Parse(strings.TrimSpace(cfg.Upstream))
	if err != nil {
		return nil, fmt.Errorf("origins upstream: %w", err)
	}
	if (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return nil, fmt.Errorf("origins upstream %q is not an http(s) URL with a host", cfg.Upstream)
	}
	return &originForwarder{
		host:   host,
		target: target,
		// Straight to the upstream, never through a proxy of its own: the
		// upstream is this pool's loopback listener.
		client: &http.Client{
			Timeout:   originsHTTPTimeout,
			Transport: &http.Transport{Proxy: nil, ForceAttemptHTTP2: false},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// answers reports whether host is the origins host.
func (f *originForwarder) answers(host string) bool {
	return f != nil && hostscope.Normalize(host) == f.host
}

// targetFor is where a request goes on the upstream: its path and query, and
// nothing else of the URL the sandbox sent. A request-target that is not a
// path is refused rather than read.
func (f *originForwarder) targetFor(in *url.URL) (string, error) {
	if in == nil || in.Opaque != "" || in.User != nil || !strings.HasPrefix(in.Path, "/") {
		return "", errors.New("the request names no path on the origins host")
	}
	// The escaped path, so a segment carrying an escaped slash reaches the
	// upstream as the one segment its router will match it as.
	target := f.target.Scheme + "://" + f.target.Host + strings.TrimSuffix(f.target.EscapedPath(), "/") + in.EscapedPath()
	if in.RawQuery != "" {
		target += "?" + in.RawQuery
	}
	return target, nil
}

// serveOrigin forwards a request for the origins host to the pool's origin
// listener as the client its certificate names, and answers with what that
// listener says. The response goes back through the ordinary response path,
// which audits it like any other exchange; only a request the proxy itself
// refuses is recorded here, as blocked.
func (h *httpProxy) serveOrigin(req *http.Request, meta *requestMeta, client clientIdentity) *http.Response {
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
			BlockedReason:  "origins: " + reason,
			RequestHeaders: req.Header,
		})
		meta.span.End()
		return goproxy.NewResponse(req, goproxy.ContentTypeText, status, "blocked by proxy: "+reason+"\n")
	}
	// The certificate's common name is the sandbox; a client the proxy cannot
	// name has no origins.
	if client.ID == "" {
		return refuse(http.StatusForbidden, "the client certificate names no sandbox")
	}
	target, err := h.origins.targetFor(req.URL)
	if err != nil {
		return refuse(http.StatusBadRequest, err.Error())
	}
	out, err := http.NewRequestWithContext(meta.ctx, req.Method, target, req.Body)
	if err != nil {
		return refuse(http.StatusBadRequest, err.Error())
	}
	out.Header = req.Header.Clone()
	out.Header.Del("Proxy-Authorization")
	out.Header.Del(OriginClientHeader)
	out.Header.Set(OriginClientHeader, client.ID)
	out.ContentLength = req.ContentLength
	//nolint:bodyclose // Handed to the proxy, which writes it to the sandbox and closes it.
	resp, err := h.origins.client.Do(out)
	if err != nil {
		// The ordinary response path records this 502 as the upstream's
		// failure, the way it does a gate whose control plane did not answer.
		return goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusBadGateway, "origin unreachable: "+err.Error())
	}
	resp.Request = req
	return resp
}
