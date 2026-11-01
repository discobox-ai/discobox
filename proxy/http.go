package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/discobox/proxy/gateway"
	"github.com/discobox-ai/discobox/proxy/internal/audit"
	"github.com/discobox-ai/discobox/proxy/internal/cache"
	"github.com/discobox-ai/discobox/proxy/internal/filter"
	"github.com/discobox-ai/discobox/proxy/internal/secrets"
	"github.com/elazarl/goproxy"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type httpProxy struct {
	proxy *goproxy.ProxyHttpServer
	// destinations dials every upstream a client's request goes to, and
	// refuses the pool's own loopback (destinationDialer).
	destinations *destinationDialer
	certs        *CertificateBundle
	filter       *filter.Filter
	// gateways is each client's gateways (ADR 26-10-16-470), replaced whole
	// by ApplyConfig.
	gateways map[string]*gateway.Table
	// resolver is how a gateway's header values are asked of the control
	// plane, and gatewayValues what it answered; nil asks nothing.
	resolver      SecretResolver
	gatewayValues *gatewayValues
	// swapper is the proxy's one Swapper, set at construction and
	// reconfigured in place (ADR 26-10-14-482 §1); nil swaps nothing.
	swapper       *secrets.Swapper
	cache         *cache.Cache
	audit         *audit.Recorder
	mitmConnect   *goproxy.ConnectAction
	rejectConnect *goproxy.ConnectAction
	// reports carries a refused credential back to the resolver; see
	// credentialreport.go.
	reports *credentialReporter
	mu      sync.RWMutex
	ids     map[string]clientIdentity
	// trusts is the pins in force (ADR 0149), replaced whole by ApplyConfig.
	trusts *trustTable
	// h2c is the transport for requests the sandbox sent as cleartext HTTP/2;
	// see cleartextHTTP2Transport.
	h2c *http.Transport
	// poolHosts answers the hosts the pool serves its sandboxes itself — its
	// Git origins and its bundle assets; nil answers none. Set once, before
	// the proxy serves.
	poolHosts *poolHosts
}

type requestMeta struct {
	ctx      context.Context
	span     trace.Span
	start    time.Time
	client   clientIdentity
	cacheHit bool
	// poolHost is the pool host that answered the request (servePoolHost),
	// whose response is never cached, and whose body is not spooled when
	// the host's bodies are unspooled.
	poolHost *poolHost
	// answered is a request the proxy refused and answered itself, and has
	// already recorded as blocked. Its response is the proxy's own, so the
	// response path neither records it again nor treats it as the upstream's
	// word on a credential.
	answered   bool
	cacheKey   string
	cacheError string
	// gateway is the gateway the request falls under (ADR 26-10-16-470), and
	// the applied fields its name, match, and the headers it touches, for the
	// audit row; forwardedURL is where it sent the request.
	gateway *appliedGateway
	// credentialWithheld is a swapped credential the gateway removed or
	// replaced before sending (appliedGateway.withholds).
	credentialWithheld   bool
	appliedRuleID        string
	appliedPattern       string
	appliedHeaders       []string
	forwardedURL         string
	requestBodyRecord    *audit.BodyRecord
	requestBodySpool     *audit.BodySpool
	requestBodyError     string
	requestBodyCloseOnce sync.Once
	requestBodyBytes     int64
	swappedHeaders       []string
	// swappedSecretIDs names the secrets this request carried (ADR
	// 26-10-01-240). The unauthorized retry (ADR 0059) does not add to it: it
	// re-swaps the same sentinels, which stand for the same secrets, and the
	// 401 and the retry are audited as two rows off this one meta — so
	// writing the retry's result here would backdate it onto the 401's row,
	// which is written after the retry is chosen.
	swappedSecretIDs []string
	// raw is what the request spent of raw bindings (rawwatch.go), and
	// rawSet the client's raw values, which span attributes are redacted of.
	raw    rawSpend
	rawSet *secrets.RawSet
	// swappedSentinels is which sentinels this request carried, kept beside the
	// header names they were found in because a rejection has to name the
	// credential and a header name cannot (ADR 0132 §1).
	swappedSentinels []string
	// reportSentinels is the subset of swappedSentinels this request answers
	// for, and the only ones a refusal or an acceptance of it is reported
	// against (ADR 26-10-13-595). The retry does not rewrite it: it goes to the
	// same URL with the same sentinels.
	reportSentinels []string
	auditURL        string
	// preSwapHeader is the request's headers as the sandbox sent them, with the
	// sentinels still in place. It is what a retry re-swaps from; re-swapping
	// the outbound headers would look for a sentinel that is no longer there.
	preSwapHeader http.Header
	// retryBody holds the request body when the request is eligible for an
	// unauthorized retry, which is the only reason it is in memory at all.
	retryBody []byte
	// retryCopy is what a body of undeclared length leaves behind as it is
	// sent, for a retry to resend once it has all been sent; see
	// bufferRetryBody.
	retryCopy *retainedBody
	retryable bool
	retried   bool
	// trust is the client's pin for this request's endpoint, when it holds
	// one: the request goes out on the trust's own transport.
	trust *pinnedTrust
}

type responseStream struct {
	source       io.ReadCloser
	req          *http.Request
	meta         *requestMeta
	audit        *audit.Recorder
	cacheStore   *cache.StreamingPut
	cacheMatcher *cache.Matcher
	// cacheBody records the body as a reference to its cache entry, the one
	// served on a hit or being stored on a miss, rather than as a spool file.
	cacheBody    bool
	bodyRecord   *audit.BodyRecord
	bodySpool    *audit.BodySpool
	bodyError    string
	status       int
	headers      http.Header
	finalizeOnce sync.Once
	sawEOF       bool
	bytesRead    int64
}

type upgradedResponseStream struct {
	source       io.ReadWriteCloser
	req          *http.Request
	meta         *requestMeta
	audit        *audit.Recorder
	stream       *audit.StreamSession
	streamRecord *audit.StreamRecord
	status       int
	headers      http.Header
	upgradeType  string
	finalizeOnce sync.Once
	c2sBytes     atomic.Int64
	s2cBytes     atomic.Int64
}

// connectTunnel is the CONNECT a MITM'd request arrived through. goproxy
// hands the CONNECT's UserData to the context of every request read from the
// tunnel, which is how the request handler knows the tunnel's authority.
type connectTunnel struct {
	authority string
}

// misdirected reports whether req names a host other than the tunnel's. The
// tunnel's authority is where the request goes; policy — the filter, header
// rules, and the secret swap — judges the Host header. A request whose Host
// names another host would have a credential bound to that host sent to the
// tunnel's.
func (t *connectTunnel) misdirected(req *http.Request) bool {
	want := authorityHostname(t.authority)
	return authorityHostname(req.Host) != want || authorityHostname(req.URL.Host) != want
}

// upgrade sends a plaintext request for port 443 inside a tunnel upstream
// over TLS. Zig's HTTP client (ziglang/zig#19878) opens a CONNECT to :443
// through an https_proxy and then speaks plaintext HTTP inside it; the origin
// would otherwise receive plain HTTP on its TLS port. It reports whether it
// did. A request's URL port is where it goes; one that names none — an
// absolute-form URL, an h2c :authority — goes to the tunnel's. One that
// names another port stays plaintext.
func (t *connectTunnel) upgrade(req *http.Request) bool {
	if req.URL.Scheme != "http" {
		return false
	}
	port := req.URL.Port()
	if port == "" {
		_, port, _ = net.SplitHostPort(t.authority)
	}
	if port != "443" {
		return false
	}
	req.URL.Scheme = "https"
	return true
}

// authorityHostname is the host of a host[:port] authority, compared the way
// policy compares it: case-insensitive, without a trailing dot, and an IPv6
// literal without its brackets whether or not a port follows.
func authorityHostname(authority string) string {
	host, _, err := net.SplitHostPort(authority)
	if err != nil {
		host = strings.Trim(authority, "[]")
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

type requestBodyStream struct {
	source io.ReadCloser
	meta   *requestMeta
}

func newHTTPProxy(certs *CertificateBundle, flt *filter.Filter, gateways map[string]*gateway.Table, resolver SecretResolver, swapper *secrets.Swapper, c *cache.Cache, recorder *audit.Recorder) *httpProxy {
	p := goproxy.NewProxyHttpServer()
	p.Verbose = false
	enableHTTP2(p)
	h := &httpProxy{
		proxy: p, certs: certs, filter: flt, gateways: gateways, resolver: resolver, gatewayValues: newGatewayValues(),
		swapper: swapper, cache: c, audit: recorder, ids: map[string]clientIdentity{},
		destinations: &destinationDialer{},
	}
	// Every upstream a client's request goes to is dialed here, the upstream
	// proxy's hop aside (applyUpstreamProxy). The pinned and h2c transports
	// are clones of this one, so they dial the same way.
	p.Tr.DialContext = h.destinations.DialContext
	// goproxy's constructor points ConnectDial at HTTPS_PROXY on its own,
	// ignoring NO_PROXY. This proxy decides its upstream itself
	// (applyUpstreamProxy), so a passthrough tunnel (ADR 26-10-31-819) to an
	// exempt destination is dialed directly, by the destination dialer.
	p.ConnectDial = nil
	h.reports = newCredentialReporter(swapper.Report)
	h.setupMITM()
	h.setupHandlers()
	return h
}

func (h *httpProxy) setPolicy(flt *filter.Filter, gateways map[string]*gateway.Table) {
	h.mu.Lock()
	h.filter = flt
	h.gateways = gateways
	h.mu.Unlock()
	h.gatewayValues.retain(gateways)
}

func (h *httpProxy) policy() *filter.Filter {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.filter
}

func (h *httpProxy) setupMITM() {
	tlsConfig := func(host string, _ *goproxy.ProxyCtx) (*tls.Config, error) {
		hostname := host
		if split, _, err := net.SplitHostPort(host); err == nil {
			hostname = split
		}
		cert, err := h.certs.SignHost(hostname)
		if err != nil {
			return nil, err
		}
		return &tls.Config{
			Certificates:       []tls.Certificate{cert},
			InsecureSkipVerify: true, //nolint:gosec // Required: MITM re-encrypts upstream TLS.
			MinVersion:         tls.VersionTLS12,
		}, nil
	}
	h.mitmConnect = &goproxy.ConnectAction{Action: goproxy.ConnectMitm, TLSConfig: tlsConfig}
	h.rejectConnect = &goproxy.ConnectAction{Action: goproxy.ConnectReject}
}

func (h *httpProxy) setupHandlers() {
	h.proxy.OnRequest().HandleConnectFunc(func(host string, proxyCtx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		ctx := context.Background()
		if proxyCtx != nil && proxyCtx.Req != nil {
			ctx = proxyCtx.Req.Context()
		}
		ctx, span := proxyTracer().Start(ctx, "proxy.http.connect", trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("proxy.http.connect_host", host)),
		)
		defer span.End()
		var req *http.Request
		if proxyCtx != nil {
			req = proxyCtx.Req
		}
		client := h.clientIdentity(req)
		flt := h.policy()
		if proxyCtx != nil {
			proxyCtx.UserData = &connectTunnel{authority: host}
		}
		// The gate host and the pool's own hosts are intercepted whatever the
		// allowlist says: none reaches the internet, and what may reach them
		// is the gate's, or the pool listener's, to decide.
		if h.swapper.IsGate(host) || h.poolHosts.answering(host) != nil {
			return h.mitmConnect, host
		}
		if !flt.AllowHostForClient(host, client.ID) {
			span.SetAttributes(append(clientAttrs(client), attribute.Bool("proxy.blocked", true))...)
			h.audit.RecordHTTP(audit.HTTPEvent{
				Context:       ctx,
				Time:          time.Now().UTC(),
				ClientID:      client.ID,
				ClientSubject: client.Subject,
				ClientSerial:  client.Serial,
				Method:        http.MethodConnect,
				Host:          host,
				URL:           host,
				Status:        http.StatusForbidden,
				Blocked:       true,
				BlockedReason: "host denied",
			})
			return h.rejectConnect, host
		}
		// A host the client's passthrough names is spliced unread (ADR
		// 26-10-31-819); it was allowed above, which passthrough never
		// decides.
		if entry, ok := flt.PassthroughForClient(host, client.ID); ok && !h.internalHost(host) {
			span.SetAttributes(attribute.Bool("proxy.http.passthrough", true), attribute.String("proxy.http.passthrough_entry", entry))
			return h.passthroughConnect(entry), host
		}
		return h.mitmConnect, host
	})

	h.proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		client := h.clientIdentity(req)
		traceCtx, span := proxyTracer().Start(req.Context(), "proxy.http.request", trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(append(clientAttrs(client),
				attribute.String("http.request.method", req.Method),
				attribute.String("url.full", h.traceURL(req, client.ID)),
				attribute.String("server.address", req.Host),
			)...),
		)
		*req = *req.WithContext(traceCtx)
		meta := &requestMeta{ctx: traceCtx, span: span, start: time.Now(), client: client}
		tunnel, _ := ctx.UserData.(*connectTunnel)
		ctx.UserData = meta
		// Every request leaves through roundTrip, which picks the transport a
		// pin calls for and answers a refused upstream certificate itself.
		ctx.RoundTripper = goproxy.RoundTripperFunc(h.roundTrip)

		// goproxy's reader hands on a request line it could make no URL of —
		// "PRI *", an HTTP/2 preface it did not take for one — and everything
		// below reads the URL. Such a line names nowhere to send anything.
		if req.URL == nil {
			meta.answered = true
			span.SetAttributes(attribute.Bool("proxy.blocked", true), attribute.Int("http.response.status_code", http.StatusBadRequest))
			span.End()
			resp := goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusBadRequest, "bad request: the request line names no URL\n")
			resp.Close = true
			return req, resp
		}

		dropHopByHopHeaders(req.Header)

		if tunnel != nil {
			// A request with no Host (HTTP/1.0) is for the tunnel's host; say
			// so, or policy would judge it by an empty name.
			if req.Host == "" {
				req.Host = tunnel.authority
			}
			if tunnel.upgrade(req) {
				span.SetAttributes(
					attribute.Bool("proxy.http.tunnel_upgraded", true),
					attribute.String("user_agent.original", req.UserAgent()),
					attribute.String("url.full", h.traceURL(req, client.ID)),
				)
			}
			if tunnel.misdirected(req) {
				meta.answered = true
				span.SetAttributes(attribute.Bool("proxy.blocked", true), attribute.Int("http.response.status_code", http.StatusMisdirectedRequest))
				h.audit.RecordHTTP(audit.HTTPEvent{
					Context:        traceCtx,
					Time:           time.Now().UTC(),
					ClientID:       client.ID,
					ClientSubject:  client.Subject,
					ClientSerial:   client.Serial,
					Method:         req.Method,
					URL:            requestURL(req),
					Host:           req.Host,
					Status:         http.StatusMisdirectedRequest,
					Blocked:        true,
					BlockedReason:  "host is not the tunnel's",
					RequestHeaders: req.Header,
				})
				span.End()
				return req, goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusMisdirectedRequest,
					fmt.Sprintf("misdirected: this tunnel is to %s; its requests must name that host\n", tunnel.authority))
			}
		}

		if swapper := h.swapper; swapper.IsGate(req.Host) {
			return req, h.serveGate(req, meta, client, swapper)
		}
		// The pool's own hosts: never the internet's, and never the
		// allowlist's to refuse, like the gate host above.
		if poolHost := h.poolHosts.answering(req.Host); poolHost != nil {
			return req, h.servePoolHost(req, meta, client, poolHost)
		}

		// Every policy judges the request as the sandbox sent it; a gateway
		// is only how it is forwarded (ADR 26-10-16-470 §3).
		if !h.policy().AllowHostForClient(req.Host, client.ID) {
			meta.answered = true
			span.SetAttributes(attribute.Bool("proxy.blocked", true), attribute.Int("http.response.status_code", http.StatusForbidden))
			h.audit.RecordHTTP(audit.HTTPEvent{
				Context:        traceCtx,
				Time:           time.Now().UTC(),
				ClientID:       client.ID,
				ClientSubject:  client.Subject,
				ClientSerial:   client.Serial,
				Method:         req.Method,
				URL:            requestURL(req),
				Host:           req.Host,
				Status:         http.StatusForbidden,
				Blocked:        true,
				BlockedReason:  "host denied",
				RequestHeaders: req.Header,
			})
			span.End()
			return req, goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusForbidden, "blocked by proxy")
		}
		// A raw secret is judged on the request as the sandbox sent it, before
		// a cache may answer it or a gateway forward it (ADR 26-10-27-840).
		if refused := h.watchRaw(req, meta, client); refused != nil {
			span.End()
			return req, refused
		}
		endpoint := requestEndpoint(req)
		applied, viaGateway, err := h.matchGateway(req, client.ID)
		if err != nil {
			meta.recordGateway(applied)
			resp := h.refuseGateway(req, meta, client, http.StatusBadRequest,
				fmt.Sprintf("gateway: %s: the request's %v, which a server could read as another path", applied.route.Name, err))
			return req, resp
		}
		if viaGateway {
			meta.recordGateway(applied)
			// The pin that matters is the one for where the request goes.
			endpoint = requestEndpoint(&http.Request{URL: applied.forwarded})
		}
		meta.trust = h.trustTable().lookup(client.ID, endpoint, time.Now())

		// A gateway's response is not cached: it is the gateway's, which the
		// project can change or remove, and a key made from what the sandbox
		// sent would outlive either.
		//
		// Nor is one that carried a raw secret (ADR 26-10-27-840): its key
		// and the entry's metadata are on the pool's disk.
		if matcher := h.cache.Matcher(); !viaGateway && !meta.raw.carried.Load() && matcher != nil && matcher.ShouldCache(req) {
			key := matcher.GenerateKey(req)
			meta.cacheKey = key
			lookupCtx, lookupSpan := proxyTracer().Start(traceCtx, "proxy.cache.lookup",
				trace.WithAttributes(attribute.String("proxy.cache.key", meta.rawSet.Redact(key))),
			)
			if entry, err := h.cache.Get(key); err == nil {
				meta.cacheHit = true
				lookupSpan.SetAttributes(attribute.Bool("proxy.cache.hit", true))
				lookupSpan.End()
				meta.ctx = lookupCtx
				resp := cache.RestoreResponse(entry, req)
				span.SetAttributes(attribute.Bool("proxy.cache.hit", true), attribute.Int("http.response.status_code", resp.StatusCode))
				if resp.Body == nil || !bodyAllowed(req, resp) {
					discardBody(req, resp)
					h.audit.RecordHTTP(h.auditEvent(req, resp, meta, time.Since(meta.start), false))
					span.End()
					return req, resp
				}
				// The cache entry is the recorded body: spooling it again
				// would write a second copy of every blob on every hit (ADR
				// 26-10-08-698 §1).
				resp.Body = &responseStream{
					source:    resp.Body,
					req:       req,
					meta:      meta,
					audit:     h.audit,
					cacheBody: true,
					status:    resp.StatusCode,
					headers:   resp.Header.Clone(),
				}
				return req, resp
			} else if !errors.Is(err, cache.ErrMiss) {
				meta.cacheError = err.Error()
			}
			lookupSpan.SetAttributes(attribute.Bool("proxy.cache.hit", false))
			lookupSpan.End()
		}

		if refused := h.swapSecrets(req, meta, client); refused != nil {
			span.End()
			return req, refused
		}
		if viaGateway {
			if refused := h.resolveGateway(req, meta, client, applied); refused != nil {
				return req, refused
			}
			// A credential the gateway removes or replaces never reaches the
			// upstream, so its answer is no evidence about it: neither retried
			// on a 401 nor reported either way.
			if applied.withholds(meta.swappedHeaders) {
				meta.credentialWithheld = true
				meta.reportSentinels = nil
			}
		}
		h.bufferRetryBody(req, meta)
		h.captureRequestBody(req, meta)
		return req, nil
	})

	h.proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		if resp == nil || ctx == nil || ctx.Req == nil {
			return resp
		}
		meta, _ := ctx.UserData.(*requestMeta)
		if meta == nil {
			traceCtx, span := proxyTracer().Start(ctx.Req.Context(), "proxy.http.request", trace.WithSpanKind(trace.SpanKindServer))
			meta = &requestMeta{ctx: traceCtx, span: span, start: time.Now(), client: clientIdentityFromRequest(ctx.Req)}
		}
		if meta.cacheHit || meta.answered {
			return resp
		}
		if retried := h.retryRejectedSwap(resp, ctx, meta); retried != nil {
			resp = retried
		}
		// A swapped credential the upstream took clears a rejection this proxy
		// reported earlier, however the credential came to be fixed —
		// reconfigured, replaced by hand, or rotated upstream. Nothing is sent
		// unless something was reported, so an ordinary working request is
		// silent.
		if len(meta.reportSentinels) > 0 && succeeded(resp.StatusCode) {
			h.reports.accepted(meta.client.ID, ctx.Req.Host, meta.reportSentinels)
		}

		if upgradedProtocol, isUpgrade := getUpgradeProtocol(ctx.Req, resp); isUpgrade {
			resp.Header.Set("Connection", "Upgrade")
			resp.Header.Set("Upgrade", upgradedProtocol)
			meta.span.SetAttributes(
				attribute.Bool("proxy.http.upgrade", true),
				attribute.String("proxy.http.upgrade_type", upgradedProtocol),
				attribute.Int("http.response.status_code", resp.StatusCode),
			)
			if body, ok := resp.Body.(io.ReadWriteCloser); ok {
				streamRecord, stream, err := h.audit.BeginUpgradeStream(meta.client.ID, upgradedProtocol, strings.Join(resp.Header.Values("Sec-WebSocket-Extensions"), ","))
				if err != nil {
					meta.cacheError = err.Error()
					recordSpanError(meta.span, err)
				}
				resp.Body = &upgradedResponseStream{
					source:       body,
					req:          ctx.Req,
					meta:         meta,
					audit:        h.audit,
					stream:       stream,
					streamRecord: streamRecord,
					status:       resp.StatusCode,
					headers:      resp.Header.Clone(),
					upgradeType:  upgradedProtocol,
				}
			} else {
				h.audit.RecordHTTP(h.auditEvent(ctx.Req, resp, meta, time.Since(meta.start), false))
				meta.span.End()
			}
			return resp
		}

		if resp.Body == nil || !bodyAllowed(ctx.Req, resp) {
			discardBody(ctx.Req, resp)
			h.audit.RecordHTTP(h.auditEvent(ctx.Req, resp, meta, time.Since(meta.start), false))
			meta.span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
			meta.span.End()
			return resp
		}

		var store *cache.StreamingPut
		// A pool host's content is the pool's own, already on its disk: a
		// cache entry would only store it twice.
		if matcher := h.cache.Matcher(); meta.poolHost == nil && !meta.raw.carried.Load() && matcher != nil && matcher.ShouldCache(ctx.Req) && matcher.ShouldCacheResponse(resp) {
			key := matcher.GenerateKey(ctx.Req)
			meta.cacheKey = key
			_, cacheSpan := proxyTracer().Start(meta.ctx, "proxy.cache.store.start", trace.WithAttributes(attribute.String("proxy.cache.key", meta.rawSet.Redact(key))))
			var err error
			store, err = h.cache.BeginStreamingPut(key, resp)
			if err != nil {
				meta.cacheError = err.Error()
				recordSpanError(cacheSpan, err)
				store = nil
			}
			cacheSpan.End()
		}
		// A body the cache is storing is recorded by reference to the entry
		// it becomes rather than spooled beside it (ADR 26-10-08-698 §1).
		var (
			bodyRecord *audit.BodyRecord
			bodySpool  *audit.BodySpool
		)
		if store == nil && (meta.poolHost == nil || !meta.poolHost.unspooledBodies) {
			var err error
			bodyRecord, bodySpool, err = h.beginResponseBody(meta)
			if err != nil {
				meta.cacheError = err.Error()
				recordSpanError(meta.span, err)
			}
		}
		resp.Body = &responseStream{
			source:       resp.Body,
			req:          ctx.Req,
			meta:         meta,
			audit:        h.audit,
			cacheStore:   store,
			cacheMatcher: h.cache.Matcher(),
			cacheBody:    store != nil,
			bodyRecord:   bodyRecord,
			bodySpool:    bodySpool,
			status:       resp.StatusCode,
			headers:      resp.Header.Clone(),
		}
		return resp
	})
}

func (h *httpProxy) auditEvent(req *http.Request, resp *http.Response, meta *requestMeta, duration time.Duration, cacheStored bool) audit.HTTPEvent {
	status := 0
	var headers http.Header
	if resp != nil {
		status = resp.StatusCode
		headers = resp.Header
	}
	responseBytes := int64(0)
	// A HEAD's ContentLength describes the GET it stands in for; nothing of it
	// was relayed.
	if resp != nil && resp.ContentLength > 0 && bodyAllowed(req, resp) {
		responseBytes = resp.ContentLength
	}
	requestBodyFile, requestBodyFormat, requestBodyBytes, requestBodyError := meta.requestBodyMetadata()
	return audit.HTTPEvent{
		Context:              meta.ctx,
		Time:                 time.Now().UTC(),
		ClientID:             meta.client.ID,
		ClientSubject:        meta.client.Subject,
		ClientSerial:         meta.client.Serial,
		Method:               req.Method,
		URL:                  meta.url(req),
		Host:                 req.Host,
		Status:               status,
		Duration:             duration,
		CacheHit:             meta.cacheHit,
		CacheStored:          cacheStored,
		CacheKey:             meta.cacheKey,
		CacheError:           meta.cacheError,
		AppliedRuleID:        meta.appliedRuleID,
		AppliedPattern:       meta.appliedPattern,
		AppliedHeaders:       meta.appliedHeaders,
		ForwardedURL:         meta.forwardedURL,
		SwappedSecretIDs:     meta.spentSecretIDs(),
		RawBindingIDs:        meta.spentRawBindingIDs(),
		RedactRequestHeaders: meta.redactRequestHeaders(),
		RequestHeaders:       req.Header,
		ResponseHeaders:      headers,
		ResponseBytes:        responseBytes,
		RequestBodyFile:      requestBodyFile,
		RequestBodyFormat:    requestBodyFormat,
		RequestBodyBytes:     requestBodyBytes,
		RequestBodyError:     requestBodyError,
	}
}

// swapSecrets substitutes sentinel placeholder credentials in req for their
// resolved real values and records which header names and URL were affected so
// the audit trail redacts them. The real value is never written to audit.
//
// A request carrying sentinels is authorized first — held to its secrets'
// routes — and one that is not allowed is answered here, with the response
// swapSecrets returns, and never sent. Authorizing before resolving is the
// point: a refused request decrypts nothing, and a value already in the cache
// does not carry a request its routes refuse.
func (h *httpProxy) swapSecrets(req *http.Request, meta *requestMeta, client clientIdentity) *http.Response {
	swapper := h.swapper
	if !swapper.Active(client.ID) {
		return nil
	}
	preURL := requestURL(req)
	_, span := proxyTracer().Start(meta.ctx, "proxy.secret_swap")
	defer span.End()
	preSwapHeader := req.Header.Clone()
	matched := swapper.Match(req, client.ID)
	if len(matched) == 0 {
		span.SetAttributes(attribute.Bool("proxy.secret_swap.swapped", false))
		return nil
	}
	if refused := h.authorizeSwap(req, meta, client, preURL, preSwapHeader, matched); refused != nil {
		span.SetAttributes(attribute.Bool("proxy.secret_swap.authorized", false))
		return refused
	}
	span.SetAttributes(attribute.Bool("proxy.secret_swap.authorized", true))
	var preSwapQuery string
	if req.URL != nil {
		preSwapQuery = req.URL.RawQuery
	}
	result := swapper.Apply(meta.ctx, req, client.ID)
	if len(result.Errors) > 0 {
		span.SetAttributes(attribute.Int("proxy.secret_swap.errors", len(result.Errors)))
		// Whatever did resolve went into req, which is never sent now: put
		// the sentinels back, in the headers and the query, so nothing past
		// here holds a real value.
		req.Header = preSwapHeader
		if req.URL != nil {
			req.URL.RawQuery = preSwapQuery
		}
		return h.refuseUnresolved(req, meta, client, preURL, result.Errors)
	}
	if !result.Swapped() {
		span.SetAttributes(attribute.Bool("proxy.secret_swap.swapped", false))
		return nil
	}
	meta.swappedHeaders = result.Headers
	meta.swappedSecretIDs = result.SecretIDs
	meta.swappedSentinels = result.Sentinels
	meta.reportSentinels = result.Reportable
	// A query swap rewrote the URL, so the retry path — which rebuilds a
	// request from the pre-swap headers — cannot reproduce this request.
	meta.retryable = len(result.QueryParams) == 0
	meta.preSwapHeader = preSwapHeader
	if len(result.QueryParams) > 0 {
		// A swapped value now lives in the outbound URL; audit the pre-swap URL
		// so the real value is never persisted.
		meta.auditURL = preURL
	}
	span.SetAttributes(
		attribute.Bool("proxy.secret_swap.swapped", true),
		attribute.Int("proxy.secret_swap.headers", len(result.Headers)),
		attribute.Int("proxy.secret_swap.query_params", len(result.QueryParams)),
		attribute.Bool("proxy.secret_swap.encoded", result.Encoded),
	)
	return nil
}

// refuseUnresolved answers a request carrying a sentinel that could not be
// resolved for any reason but a denial — the control plane unreachable through
// the whole retry window — and never sends it (ADR 26-10-14-482 §2). Sent, the
// sentinel would reach the upstream as a credential it rejects, and a client
// reads a 401 as a dead login; a 503 it reads as an outage, and retries.
func (h *httpProxy) refuseUnresolved(req *http.Request, meta *requestMeta, client clientIdentity, preURL string, errs []string) *http.Response {
	reason := "credential unavailable: " + strings.Join(errs, "; ")
	status := http.StatusServiceUnavailable
	meta.answered = true
	meta.span.SetAttributes(attribute.Bool("proxy.blocked", true), attribute.Int("http.response.status_code", status))
	h.recordRefusal(req, meta, client, refusalRecord{
		url:    preURL,
		header: req.Header,
		reason: reason,
		status: status,
	})
	resp := goproxy.NewResponse(req, goproxy.ContentTypeText, status,
		"blocked by proxy: the credential this request carries could not be resolved; retry shortly\n")
	// Wait about as long as the proxy itself just waited.
	retryAfter := int64(math.Ceil(h.swapper.ResolveRetry().Seconds()))
	resp.Header.Set("Retry-After", strconv.FormatInt(retryAfter, 10))
	resp.Header.Set("X-Should-Retry", "true")
	return resp
}

// serveGate answers a request for the gate host, which is never sent to the
// internet (ADR 0140 §2). What the resolver admits is answered by the upstream
// behind the gate and recorded like any exchange, a 502 included when the gate
// fails after letting it in; what it refuses is answered here with a 403 and
// recorded once, as blocked. Nothing is swapped on the way.
func (h *httpProxy) serveGate(req *http.Request, meta *requestMeta, client clientIdentity, swapper *secrets.Swapper) *http.Response {
	admitted, err := swapper.Gate(meta.ctx, secrets.GateRequest{ClientID: client.ID, Request: req})
	var refusal *secrets.GateRefusal
	if err == nil || !errors.As(err, &refusal) {
		if err == nil {
			return admitted.Response
		}
		// Let in, and then the gate failed: the upstream was unreachable, or
		// went quiet after the request went out, and may have acted on it.
		// That is not a refusal, so it is recorded as an ordinary exchange
		// the upstream failed — a 502, by the response path, like any
		// other — and never as a request the proxy refused and did not send.
		return goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusBadGateway, "discobox API unreachable: "+err.Error())
	}
	status := http.StatusForbidden
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
		BlockedReason:  "gate: " + err.Error(),
		RequestHeaders: req.Header,
	})
	meta.span.End()
	return goproxy.NewResponse(req, goproxy.ContentTypeText, status, "blocked by proxy: "+err.Error())
}

// authorizeSwap asks whether a request may carry the credentials its sentinels
// stand for, and answers it here when it may not. What is asked about is the
// request as the sandbox sent it — nothing has been resolved yet, so it holds
// sentinels and never a credential. An authorizer that cannot answer refuses:
// a credential is resolved only for a request something agreed to.
func (h *httpProxy) authorizeSwap(req *http.Request, meta *requestMeta, client clientIdentity, preURL string, preSwapHeader http.Header, matched []string) *http.Response {
	verdict, err := h.swapper.Authorize(meta.ctx, secrets.AuthorizeRequest{
		ClientID:  client.ID,
		Sentinels: matched,
		Method:    req.Method,
		Host:      req.Host,
		URL:       preURL,
		Header:    preSwapHeader,
		Body:      routeBody(req),
	})
	if err == nil && verdict.Allow {
		return nil
	}
	reason := refusalReason(verdict, err)
	status := http.StatusForbidden
	meta.answered = true
	meta.span.SetAttributes(attribute.Bool("proxy.blocked", true), attribute.Int("http.response.status_code", status))
	h.recordRefusal(req, meta, client, refusalRecord{
		url:    preURL,
		header: preSwapHeader,
		reason: "route: " + reason,
		status: status,
	})
	return goproxy.NewResponse(req, goproxy.ContentTypeText, status, "blocked by proxy: "+reason)
}

// refusalReason is what a refused request is answered with, whichever way the
// ask ended. A verdict that refuses and says nothing still has to say
// something: a refusal nobody can read is indistinguishable from a broken
// credential.
func refusalReason(verdict secrets.Verdict, err error) string {
	switch {
	case err != nil:
		return "the credential's routes could not be read: " + err.Error()
	case verdict.Reason == "":
		return "not a route of this credential"
	default:
		return verdict.Reason
	}
}

// refusalRecord is one refused request as the audit trail keeps it: the URL and
// headers as the sandbox sent them, why it was refused, and the status the
// sandbox actually received.
//
// The status is the caller's because a refusal is not always the answer. A
// first attempt refused before it is sent is answered 403 here; a refused
// retry is not answered at all, and the sandbox keeps whatever the upstream
// already said, so claiming a 403 would put a response in the trail that
// nobody received.
//
// The reason is the row's whole BlockedReason, led by what refused it: "route:"
// for a credential's routes, "credential unavailable:" for a sentinel nothing
// could resolve.
type refusalRecord struct {
	url    string
	header http.Header
	reason string
	status int
}

// recordRefusal writes the blocked row a refusal leaves behind.
func (h *httpProxy) recordRefusal(req *http.Request, meta *requestMeta, client clientIdentity, rec refusalRecord) {
	h.audit.RecordHTTP(audit.HTTPEvent{
		Context:        meta.ctx,
		Time:           time.Now().UTC(),
		ClientID:       client.ID,
		ClientSubject:  client.Subject,
		ClientSerial:   client.Serial,
		Method:         req.Method,
		URL:            rec.url,
		Host:           req.Host,
		Status:         rec.status,
		Blocked:        true,
		BlockedReason:  rec.reason,
		RequestHeaders: rec.header,
	})
}

// unauthorizedRetryMaxBody bounds what a retryable request holds in memory. A
// body larger than this is streamed as before and its 401 is passed through:
// buffering a request of unbounded size to make a retry possible would trade a
// rare recoverable failure for a common memory one.
const unauthorizedRetryMaxBody = 8 << 20

// bufferRetryBody keeps a swapped request's body so the request can be sent a
// second time if the upstream rejects the credential.
//
// A body of declared length is read into memory before the request goes out,
// and one that does not fit is left streaming and stops the request being
// retryable. A body of undeclared length — a chunked upload, or any HTTP/2
// stream, gRPC's included — is copied as it is sent instead: it may be a
// stream whose sender waits on the response before it sends more, and reading
// it ahead would wait for an end that never comes. Such a request is
// retryable only if all of it was sent, and fit, by the time the 401 came back.
func (h *httpProxy) bufferRetryBody(req *http.Request, meta *requestMeta) {
	if !meta.retryable || req.Body == nil || req.Body == http.NoBody {
		return
	}
	if req.ContentLength < 0 {
		meta.retryCopy = &retainedBody{source: req.Body}
		req.Body = meta.retryCopy
		return
	}
	buffered, err := io.ReadAll(io.LimitReader(req.Body, unauthorizedRetryMaxBody+1))
	if err != nil || int64(len(buffered)) > unauthorizedRetryMaxBody {
		// Hand back exactly what the transport would have read: what came out
		// of the body so far, then the rest of it (or the error it stopped on).
		meta.retryable = false
		req.Body = joinedBody{Reader: io.MultiReader(bytes.NewReader(buffered), req.Body), closer: req.Body}
		return
	}
	_ = req.Body.Close()
	meta.retryBody = buffered
	req.Body = io.NopCloser(bytes.NewReader(buffered))
}

// retainedBody passes a body through while keeping a copy of what passed, up
// to unauthorizedRetryMaxBody. The transport may still be sending it when the
// response arrives, so what it holds is read under its lock.
type retainedBody struct {
	source io.ReadCloser
	mu     sync.Mutex
	kept   []byte
	over   bool
	whole  bool
}

func (b *retainedBody) Read(p []byte) (int, error) {
	n, err := b.source.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > 0 && !b.over {
		if len(b.kept)+n > unauthorizedRetryMaxBody {
			b.over, b.kept = true, nil
		} else {
			b.kept = append(b.kept, p[:n]...)
		}
	}
	if errors.Is(err, io.EOF) && !b.over {
		b.whole = true
	}
	return n, err
}

func (b *retainedBody) Close() error { return b.source.Close() }

// sent returns the whole body once all of it has passed and fit, and false
// while any of it is still to come or it did not fit.
func (b *retainedBody) sent() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.kept, b.whole
}

// joinedBody re-fronts an already partly-read body without taking ownership of
// closing anything but the original.
type joinedBody struct {
	io.Reader
	closer io.Closer
}

func (b joinedBody) Close() error { return b.closer.Close() }

// retryRejectedSwap sends a request once more when the upstream rejected the
// credential the proxy swapped into it, returning the retry's response or nil
// to keep the original — and reports whatever the attempt settled.
//
// A 401 on a swapped request is not the sandbox's error: the sandbox holds a
// sentinel, which cannot expire, and everything behind it belongs to the
// control plane. It is worth one more attempt because a harness reading that
// 401 will conclude its login is gone — Claude Code clears its credentials file
// and cannot restore it, since the refresh token it holds is a placeholder.
//
// There are exactly two other values to try, and both are tried in the order
// that a rejection makes them likely:
//
//  1. A freshly resolved one. The cached value is stale whenever the control
//     plane rotated the credential and the proxy has not caught up, which is
//     invisible until something 401s on it.
//  2. The value the last rotation displaced, still within its grace. This is
//     the opposite failure: the proxy did catch up, onto a token the upstream
//     has not started honoring yet.
//
// If neither produces a different credential there is nothing new to send, and
// re-sending the rejected one would only spend an upstream request to fail the
// same way.
//
// What the attempt settles is then reported (ADR 0132 §1), and the retry is
// what makes the report worth anything: "the credential was refused" is a
// rotation as often as it is a dead login, and only "the other value was
// refused too" tells them apart. A retry that *works* reports nothing — that is
// the blip ADR 0059 is about, and it is handled by having handled it.
func (h *httpProxy) retryRejectedSwap(resp *http.Response, ctx *goproxy.ProxyCtx, meta *requestMeta) *http.Response {
	if resp.StatusCode != http.StatusUnauthorized || meta.retried {
		return nil
	}
	if len(meta.swappedHeaders) == 0 || len(meta.swappedSentinels) == 0 || meta.credentialWithheld {
		// Not a request this proxy put a credential into, or one whose
		// credential a gateway took out before it was sent: the 401 belongs to
		// whatever was sent, and is neither ours to retry nor ours to report.
		return nil
	}
	swapper := h.swapper
	if swapper == nil {
		return nil
	}
	if meta.retryCopy != nil {
		if body, whole := meta.retryCopy.sent(); whole {
			meta.retryBody = body
		} else {
			meta.retryable = false
		}
	}
	// A request that cannot be retried — a body too large to hold or not yet
	// all sent, a swap into the URL — is still a refused credential, and the
	// one thing that can be said about it is that it was refused.
	if !meta.retryable || meta.preSwapHeader == nil {
		h.reports.rejected(meta.client.ID, ctx.Req.Host, meta.reportSentinels, secrets.OutcomeRejected)
		return nil
	}
	req := ctx.Req
	meta.retried = true

	_, span := proxyTracer().Start(meta.ctx, "proxy.secret_swap.retry")
	defer span.End()

	// A retry carries a credential the first attempt did not, so it is
	// authorized the way the first attempt was: the same derivation, over the
	// request about to be sent, because what the pool holds for the sentinel
	// can have changed in the time the upstream took to refuse.
	//
	// The set comes from Match and not from what the first attempt managed to
	// resolve. The swaps below re-scan every sentinel from scratch, so a
	// sentinel that was denied the first time — and sent as itself — can
	// resolve now, and would otherwise go upstream under a verdict that never
	// named it.
	previous := h.rebuiltRequest(req, meta)
	matched := swapper.Match(previous, meta.client.ID)
	verdict, err := swapper.Authorize(meta.ctx, secrets.AuthorizeRequest{
		ClientID:  meta.client.ID,
		Sentinels: matched,
		Method:    req.Method,
		Host:      req.Host,
		URL:       meta.url(req),
		Header:    meta.preSwapHeader,
		Body:      retryRouteBody(meta.preSwapHeader, meta.retryBody),
	})
	if err != nil || !verdict.Allow {
		// Not an answer to the sandbox: the request has already been sent and
		// answered, so the upstream's own 401 stands. What the refusal owes is
		// the trail — the retry is refused where the first attempt was allowed,
		// which is recorded rather than only traced.
		span.SetAttributes(attribute.Bool("proxy.secret_swap.retry.authorized", false))
		h.recordRefusal(req, meta, meta.client, refusalRecord{
			url:    meta.url(req),
			header: meta.preSwapHeader,
			reason: "route: " + refusalReason(verdict, err),
			// What the sandbox got is the upstream's own answer, which is
			// going down the wire unchanged.
			status: resp.StatusCode,
		})
		h.reports.rejected(meta.client.ID, req.Host, meta.reportSentinels, secrets.OutcomeRejected)
		return nil
	}

	// Read the displaced value before invalidating: invalidation drops the
	// cache entry, and this asks about the value behind it.
	previousResult := swapper.ApplyPrevious(previous, meta.client.ID)

	swapper.Invalidate(meta.client.ID, req.Host)
	resolved := h.rebuiltRequest(req, meta)
	resolvedResult := swapper.Apply(meta.ctx, resolved, meta.client.ID)

	var retryReq *http.Request
	var source string
	switch {
	// A fresh resolve that failed transiently for any sentinel would send that
	// sentinel as itself (ADR 26-10-14-482 §2), so it is not a candidate.
	case resolvedResult.Swapped() && len(resolvedResult.Errors) == 0 && !sameHeaderValues(req.Header, resolved.Header, meta.swappedHeaders):
		retryReq, source = resolved, "resolved"
	// The displaced values cover only sentinels that rotated lately. One the
	// first attempt swapped and this cannot would go out as itself, so the
	// previous candidate is sent only when it covers all of them.
	case previousResult.Swapped() && coversAll(previousResult.Sentinels, meta.swappedSentinels) && !sameHeaderValues(req.Header, previous.Header, meta.swappedHeaders):
		retryReq, source = previous, "previous"
	default:
		// Nothing to send that is not the value just refused, which means the
		// credential the resolver would hand out now is the rejected one.
		span.SetAttributes(attribute.Bool("proxy.secret_swap.retry.attempted", false))
		h.reports.rejected(meta.client.ID, req.Host, meta.reportSentinels, secrets.OutcomeRejected)
		return nil
	}

	retryResp, err := ctx.RoundTrip(retryReq)
	if err != nil {
		// The original 401 is still intact and unread; returning it beats
		// turning a rejected request into a proxy error.
		recordSpanError(span, err)
		return nil
	}

	// The 401 was a real exchange with the upstream and is audited as one; the
	// retry is audited as its own by the normal response path.
	h.audit.RecordHTTP(h.auditEvent(req, resp, meta, time.Since(meta.start), false))
	drainAndClose(resp.Body)

	span.SetAttributes(
		attribute.Bool("proxy.secret_swap.retry.attempted", true),
		attribute.String("proxy.secret_swap.retry.credential", source),
		attribute.Int("proxy.secret_swap.retry.status", retryResp.StatusCode),
	)
	// A second credential refused is the fact that needs a person. A retry that
	// worked says the opposite, and clears anything reported before it.
	switch {
	case retryResp.StatusCode == http.StatusUnauthorized:
		h.reports.rejected(meta.client.ID, req.Host, meta.reportSentinels, secrets.OutcomeRejectedAfterRetry)
	case succeeded(retryResp.StatusCode):
		h.reports.accepted(meta.client.ID, req.Host, meta.reportSentinels)
	}
	ctx.Req = retryReq
	meta.start = time.Now()
	return retryResp
}

// coversAll reports whether every one of want is in have.
func coversAll(have, want []string) bool {
	for _, sentinel := range want {
		if !slices.Contains(have, sentinel) {
			return false
		}
	}
	return true
}

// rebuiltRequest is the same request again, with the sentinels back in its
// headers and its body ready to be read from the start.
func (h *httpProxy) rebuiltRequest(req *http.Request, meta *requestMeta) *http.Request {
	out := req.Clone(meta.ctx)
	out.Header = meta.preSwapHeader.Clone()
	body := meta.retryBody
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return out
}

// succeeded is the narrow reading of a response that retracts a rejection: 2xx
// and nothing else.
//
// A redirect is not a success here, which is the whole reason this is a
// function and not a `< 400`. An expired session is commonly answered with a
// 302 to a sign-in page, and taking that as "the credential works again" would
// retract a standing rejection about a credential that is still dead — using
// the upstream's own way of *saying* it is dead as the evidence it is alive.
func succeeded(status int) bool { return status >= 200 && status < 300 }

// bodyAllowed reports whether resp may have a body on the wire: it is not the
// answer to a HEAD, nor a 1xx, 204, or 304 (RFC 9112 §6.3).
func bodyAllowed(req *http.Request, resp *http.Response) bool {
	if req.Method == http.MethodHead {
		return false
	}
	return resp.StatusCode >= 200 && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified
}

// discardBody leaves a response that cannot carry a body with http.NoBody, the
// only body goproxy relays as nothing: it frames any other as chunked, an empty
// wrapper included. A 1xx, 204, or 304 also loses its transfer coding, which a
// 304 may carry by the letter of RFC 9110 and for which goproxy would still
// write a chunk terminator; after the headers the client reads it as the start
// of the next response. A HEAD keeps its own: it describes the GET, and
// net/http writes nothing after a HEAD's headers either way.
func discardBody(req *http.Request, resp *http.Response) {
	if bodyAllowed(req, resp) {
		return
	}
	if resp.Body != nil && resp.Body != http.NoBody {
		_ = resp.Body.Close()
	}
	resp.Body = http.NoBody
	if req.Method != http.MethodHead {
		resp.TransferEncoding = nil
		resp.Header.Del("Transfer-Encoding")
	}
}

// sameHeaderValues reports whether every named header holds the same values in
// both, which for the swapped headers means the retry would send the same
// credential that was just rejected.
func sameHeaderValues(a, b http.Header, names []string) bool {
	for _, name := range names {
		if !slices.Equal(a.Values(name), b.Values(name)) {
			return false
		}
	}
	return true
}

// drainAndClose discards a response body the proxy is replacing, so the
// upstream connection can be reused rather than torn down.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<20))
	_ = body.Close()
}

func (h *httpProxy) captureRequestBody(req *http.Request, meta *requestMeta) {
	if req == nil || req.Body == nil || req.Body == http.NoBody {
		return
	}
	record, spool, err := h.audit.BeginBody(meta.client.ID, audit.BodyKindRequest)
	if err != nil {
		meta.requestBodyError = err.Error()
		recordSpanError(meta.span, err)
		return
	}
	if spool == nil {
		return
	}
	meta.requestBodyRecord = record
	meta.requestBodySpool = spool
	req.Body = &requestBodyStream{source: req.Body, meta: meta}
}

func (h *httpProxy) beginResponseBody(meta *requestMeta) (*audit.BodyRecord, *audit.BodySpool, error) {
	record, spool, err := h.audit.BeginBody(meta.client.ID, audit.BodyKindResponse)
	if err != nil {
		return nil, nil, err
	}
	return record, spool, nil
}

func (s *upgradedResponseStream) Read(p []byte) (int, error) {
	n, err := s.source.Read(p)
	if n > 0 {
		s.s2cBytes.Add(int64(n))
		if s.stream != nil {
			s.stream.RecordChunk(audit.StreamServerToClient, p[:n])
		}
	}
	if err != nil {
		s.finish()
	}
	return n, err
}

func (s *upgradedResponseStream) Write(p []byte) (int, error) {
	// Counted before the bytes are handed on, not after. The origin can answer
	// them the instant Write returns, and that answer travels the other
	// direction — whose goroutine may reach finish() and snapshot the counters
	// while this one has not yet run its Add. Counting first closes that
	// window; a short write gives the difference back below, so the count is
	// only ever transiently high and never loses a byte the origin has seen.
	//
	// The bytes are recorded before they are handed on for the same reason: a
	// session finished in that window is closed, and drops a later chunk
	// without counting it. The spool holds what the client sent, which a short
	// write does not change.
	//
	// The s2c direction needs no such care: its bytes reach the client only
	// after Read returns, which is after its own Add and record.
	s.c2sBytes.Add(int64(len(p)))
	if s.stream != nil {
		s.stream.RecordChunk(audit.StreamClientToServer, p)
	}
	n, err := s.source.Write(p)
	if n < len(p) {
		s.c2sBytes.Add(int64(n) - int64(len(p)))
	}
	if err != nil {
		s.finish()
	}
	return n, err
}

func (s *upgradedResponseStream) Close() error {
	err := s.source.Close()
	s.finish()
	return err
}

func (s *upgradedResponseStream) finish() {
	s.finalizeOnce.Do(func() {
		_ = s.stream.Close()
		duration := time.Since(s.meta.start)
		event := s.auditEvent(duration)
		s.audit.RecordHTTP(event)
		s.meta.span.SetAttributes(
			attribute.Int("http.response.status_code", s.status),
			attribute.Bool("proxy.http.upgrade", true),
			attribute.String("proxy.http.upgrade_type", s.upgradeType),
			attribute.Int64("proxy.http.upgrade.c2s_bytes", event.UpgradeC2SBytes),
			attribute.Int64("proxy.http.upgrade.s2c_bytes", event.UpgradeS2CBytes),
		)
		s.meta.span.End()
	})
}

func (s *upgradedResponseStream) auditEvent(duration time.Duration) audit.HTTPEvent {
	requestBodyFile, requestBodyFormat, requestBodyBytes, requestBodyError := s.meta.requestBodyMetadata()
	event := audit.HTTPEvent{
		Context:              s.meta.ctx,
		Time:                 time.Now().UTC(),
		ClientID:             s.meta.client.ID,
		ClientSubject:        s.meta.client.Subject,
		ClientSerial:         s.meta.client.Serial,
		Method:               s.req.Method,
		URL:                  s.meta.url(s.req),
		Host:                 s.req.Host,
		Status:               s.status,
		Duration:             duration,
		AppliedRuleID:        s.meta.appliedRuleID,
		AppliedPattern:       s.meta.appliedPattern,
		AppliedHeaders:       s.meta.appliedHeaders,
		ForwardedURL:         s.meta.forwardedURL,
		SwappedSecretIDs:     s.meta.spentSecretIDs(),
		RawBindingIDs:        s.meta.spentRawBindingIDs(),
		RedactRequestHeaders: s.meta.redactRequestHeaders(),
		RequestHeaders:       s.req.Header,
		ResponseHeaders:      s.headers,
		RequestBodyFile:      requestBodyFile,
		RequestBodyFormat:    requestBodyFormat,
		RequestBodyBytes:     requestBodyBytes,
		RequestBodyError:     requestBodyError,
		Upgrade:              true,
		UpgradeType:          s.upgradeType,
		UpgradeC2SBytes:      s.c2sBytes.Load(),
		UpgradeS2CBytes:      s.s2cBytes.Load(),
	}
	if s.streamRecord != nil {
		event.StreamSessionID = s.streamRecord.SessionID
		event.StreamFile = s.streamRecord.File
		event.StreamFormat = s.streamRecord.Format
	}
	if s.stream != nil {
		event.StreamDroppedChunks = s.stream.DroppedChunks()
		event.StreamDroppedBytes = s.stream.DroppedBytes()
	}
	return event
}

func (s *responseStream) Read(p []byte) (int, error) {
	n, err := s.source.Read(p)
	if n > 0 && s.bodySpool != nil {
		if _, writeErr := s.bodySpool.Write(p[:n]); writeErr != nil && s.bodyError == "" {
			s.bodyError = writeErr.Error()
			recordSpanError(s.meta.span, writeErr)
		}
	}
	if n > 0 && s.cacheStore != nil {
		s.bytesRead += int64(n)
		if _, writeErr := s.cacheStore.Write(p[:n]); writeErr != nil {
			s.meta.cacheError = writeErr.Error()
			_ = s.cacheStore.Abort()
			s.cacheStore = nil
		}
	} else if n > 0 {
		s.bytesRead += int64(n)
	}
	if err == io.EOF {
		s.sawEOF = true
		s.finish(false, nil)
	} else if err != nil {
		s.finish(true, err)
	}
	return n, err
}

func (s *responseStream) Close() error {
	err := s.source.Close()
	s.finish(!s.sawEOF, err)
	return err
}

func (s *responseStream) finish(aborted bool, readErr error) {
	s.finalizeOnce.Do(func() {
		cacheStored := false
		if s.cacheStore != nil {
			_, cacheSpan := proxyTracer().Start(s.meta.ctx, "proxy.cache.store.finish")
			if aborted || (readErr != nil && !errors.Is(readErr, io.EOF)) {
				_ = s.cacheStore.Abort()
				cacheSpan.SetAttributes(attribute.Bool("proxy.cache.store.aborted", true))
			} else if err := s.cacheMatcher.VerifyDigestHex(s.req.URL.Path, s.cacheStore.DigestHex()); err != nil {
				_ = s.cacheStore.Abort()
				s.meta.cacheError = err.Error()
				recordSpanError(cacheSpan, err)
			} else if err := s.cacheStore.Commit(); err == nil {
				cacheStored = true
				cacheSpan.SetAttributes(attribute.Bool("proxy.cache.stored", true))
			} else {
				s.meta.cacheError = err.Error()
				recordSpanError(cacheSpan, err)
			}
			cacheSpan.End()
		}
		responseBodyFile, responseBodyFormat, responseBodyBytes, responseBodyError := s.responseBodyMetadata(cacheStored)
		responseBodyError = mergeResponseBodyError(responseBodyError, readErr)
		requestBodyFile, requestBodyFormat, requestBodyBytes, requestBodyError := s.meta.requestBodyMetadata()
		event := audit.HTTPEvent{
			Context:              s.meta.ctx,
			Time:                 time.Now().UTC(),
			ClientID:             s.meta.client.ID,
			ClientSubject:        s.meta.client.Subject,
			ClientSerial:         s.meta.client.Serial,
			Method:               s.req.Method,
			URL:                  s.meta.url(s.req),
			Host:                 s.req.Host,
			Status:               s.status,
			Duration:             time.Since(s.meta.start),
			CacheHit:             s.meta.cacheHit,
			CacheStored:          cacheStored,
			CacheKey:             s.meta.cacheKey,
			CacheError:           s.meta.cacheError,
			AppliedRuleID:        s.meta.appliedRuleID,
			AppliedPattern:       s.meta.appliedPattern,
			AppliedHeaders:       s.meta.appliedHeaders,
			ForwardedURL:         s.meta.forwardedURL,
			SwappedSecretIDs:     s.meta.spentSecretIDs(),
			RawBindingIDs:        s.meta.spentRawBindingIDs(),
			RedactRequestHeaders: s.meta.redactRequestHeaders(),
			RequestHeaders:       s.req.Header,
			ResponseHeaders:      s.headers,
			ResponseBytes:        s.bytesRead,
			RequestBodyFile:      requestBodyFile,
			RequestBodyFormat:    requestBodyFormat,
			RequestBodyBytes:     requestBodyBytes,
			RequestBodyError:     requestBodyError,
			ResponseBodyFile:     responseBodyFile,
			ResponseBodyFormat:   responseBodyFormat,
			ResponseBodyBytes:    responseBodyBytes,
			ResponseBodyError:    responseBodyError,
		}
		s.audit.RecordHTTP(event)
		s.meta.span.SetAttributes(attribute.Int("http.response.status_code", s.status), attribute.Bool("proxy.cache.stored", cacheStored))
		s.meta.span.End()
	})
}

func mergeResponseBodyError(bodyError string, readErr error) string {
	if readErr == nil || errors.Is(readErr, io.EOF) {
		return bodyError
	}
	if bodyError == "" {
		return readErr.Error()
	}
	return bodyError + "; response read: " + readErr.Error()
}

func (s *responseStream) responseBodyMetadata(cacheStored bool) (file string, format string, bytes int64, errText string) {
	if s.cacheBody {
		// A miss whose store did not commit has already streamed to the client
		// with nothing spooled, so the body is not retained, and the row says
		// so rather than reading as a body never sent.
		if s.meta.cacheHit || cacheStored {
			return "", audit.BodyFormatCache, s.bytesRead, ""
		}
		return "", "", 0, "not retained: the response cache did not store it"
	}
	if s.bodySpool != nil {
		if err := s.bodySpool.Close(); err != nil && s.bodyError == "" {
			s.bodyError = err.Error()
		}
		if err := s.bodySpool.Err(); err != nil && s.bodyError == "" {
			s.bodyError = err.Error()
		}
		bytes = s.bodySpool.Bytes()
	}
	if s.bodyRecord != nil {
		file = s.bodyRecord.File
		format = s.bodyRecord.Format
	}
	return file, format, bytes, s.bodyError
}

func (s *requestBodyStream) Read(p []byte) (int, error) {
	n, err := s.source.Read(p)
	if n > 0 && s.meta != nil && s.meta.requestBodySpool != nil {
		if _, writeErr := s.meta.requestBodySpool.Write(p[:n]); writeErr != nil && s.meta.requestBodyError == "" {
			s.meta.requestBodyError = writeErr.Error()
			recordSpanError(s.meta.span, writeErr)
		}
	}
	if err == io.EOF && s.meta != nil {
		s.meta.closeRequestBodySpool()
	}
	return n, err
}

func (s *requestBodyStream) Close() error {
	err := s.source.Close()
	if s.meta != nil {
		s.meta.closeRequestBodySpool()
	}
	return err
}

// redactRequestHeaders returns the header names to redact in audit: the
// headers a gateway touches plus any header whose value was secret-swapped.
func (m *requestMeta) redactRequestHeaders() []string {
	if len(m.swappedHeaders) == 0 {
		return m.appliedHeaders
	}
	out := append(slices.Clone(m.appliedHeaders), m.swappedHeaders...)
	slices.Sort(out)
	return slices.Compact(out)
}

// url returns the URL to record in audit, preferring the pre-swap URL when a
// secret was swapped into a query parameter.
func (m *requestMeta) url(req *http.Request) string {
	if m.auditURL != "" {
		return m.auditURL
	}
	return requestURL(req)
}

func (m *requestMeta) requestBodyMetadata() (file string, format string, bytes int64, errText string) {
	if m == nil {
		return "", "", 0, ""
	}
	m.closeRequestBodySpool()
	if m.requestBodyRecord != nil {
		file = m.requestBodyRecord.File
		format = m.requestBodyRecord.Format
	}
	bytes = m.requestBodyBytes
	return file, format, bytes, m.requestBodyError
}

func (m *requestMeta) closeRequestBodySpool() {
	if m == nil || m.requestBodySpool == nil {
		return
	}
	m.requestBodyCloseOnce.Do(func() {
		if err := m.requestBodySpool.Close(); err != nil && m.requestBodyError == "" {
			m.requestBodyError = err.Error()
		}
		if err := m.requestBodySpool.Err(); err != nil && m.requestBodyError == "" {
			m.requestBodyError = err.Error()
		}
		m.requestBodyBytes = m.requestBodySpool.Bytes()
	})
}

func getUpgradeProtocol(req *http.Request, resp *http.Response) (string, bool) {
	if req == nil || resp == nil || resp.StatusCode != http.StatusSwitchingProtocols {
		return "", false
	}
	if !headerContainsToken(req.Header, "Connection", "Upgrade") {
		return "", false
	}
	requestUpgrade := strings.TrimSpace(req.Header.Get("Upgrade"))
	responseUpgrade := strings.TrimSpace(resp.Header.Get("Upgrade"))
	upgrade := responseUpgrade
	if upgrade == "" {
		upgrade = requestUpgrade
	}
	if upgrade == "" {
		return "", false
	}
	return strings.ToLower(upgrade), true
}

// hopByHopHeaders are the request headers that describe the sandbox's
// connection to this proxy rather than the request (RFC 9110 §7.6.1): the
// list net/http/httputil.ReverseProxy drops.
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// dropHopByHopHeaders removes the hop-by-hop headers from a request before
// anything judges or forwards it, along with every header its Connection
// names. Forwarded, they are wrong on any upstream, and over HTTP/2 a strict
// one ends the connection: there TE may only be "trailers" (RFC 9113 §8.2.2),
// and libwww-perl sends "TE: deflate,gzip;q=0.3" on every request.
//
// Two survive: "TE: trailers" when the client asked for trailers, which gRPC
// needs, and a WebSocket handshake's "Connection: Upgrade" with its Upgrade,
// which the upgraded-stream path needs. That is the one upgrade goproxy
// forwards — it deletes Connection from any other — so another upgrade loses
// both, rather than going out with a bare Upgrade that an HTTP/2 upstream
// refuses as TE is refused.
func dropHopByHopHeaders(header http.Header) {
	upgrade := ""
	if headerContainsToken(header, "Connection", "Upgrade") && headerContainsToken(header, "Upgrade", "websocket") {
		upgrade = header.Get("Upgrade")
	}
	trailers := headerContainsToken(header, "Te", "trailers")
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				header.Del(name)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		header.Del(name)
	}
	if trailers {
		header.Set("Te", "trailers")
	}
	if upgrade != "" {
		header.Set("Connection", "Upgrade")
		header.Set("Upgrade", upgrade)
	}
}

func headerContainsToken(header http.Header, key, token string) bool {
	for _, value := range header.Values(key) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func (h *httpProxy) serveConn(ctx context.Context, conn *peekedConn, identity clientIdentity) bool {
	// Bind the client identity to the connection's RemoteAddr for the lifetime of
	// the connection. The entry is removed when the connection closes rather than
	// when serveConn returns, because goproxy hijacks the connection for HTTPS
	// MITM and keeps dispatching requests after serveConn has returned; deleting
	// on return would leave those MITM'd requests without a client identity.
	var served net.Conn = conn
	if conn.RemoteAddr() != nil {
		key := conn.RemoteAddr().String()
		h.mu.Lock()
		h.ids[key] = identity
		h.mu.Unlock()
		served = &identityConn{Conn: conn, onClose: func() {
			h.mu.Lock()
			delete(h.ids, key)
			h.mu.Unlock()
		}}
	}
	listener := &singleConnListener{conn: served, done: make(chan struct{})}
	var hijacked atomic.Bool
	server := &http.Server{
		Handler:           h.proxy,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return contextOrBackground(ctx)
		},
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateHijacked {
				hijacked.Store(true)
				_ = listener.Close()
				return
			}
			if state == http.StateClosed {
				_ = listener.Close()
			}
		},
	}
	_ = server.Serve(listener)
	_ = listener.Close()
	return hijacked.Load()
}

func (h *httpProxy) clientIdentity(req *http.Request) clientIdentity {
	if req != nil && req.RemoteAddr != "" {
		h.mu.RLock()
		identity := h.ids[req.RemoteAddr]
		h.mu.RUnlock()
		if identity.ID != "" || identity.Serial != "" {
			return identity
		}
	}
	return clientIdentityFromRequest(req)
}

// identityConn removes the connection's stored client identity when the
// connection is closed, so the identity outlives serveConn (which returns early
// once goproxy hijacks the connection for MITM) but does not leak.
type identityConn struct {
	net.Conn
	onClose func()
	once    sync.Once
}

func (c *identityConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.onClose)
	return err
}

type singleConnListener struct {
	conn   net.Conn
	served bool
	done   chan struct{}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.served {
		<-l.done
		return nil, net.ErrClosed
	}
	l.served = true
	return l.conn, nil
}

func (l *singleConnListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *singleConnListener) Addr() net.Addr {
	return l.conn.LocalAddr()
}

type clientIdentity struct {
	ID      string
	Subject string
	Serial  string
}

func clientIdentityFromRequest(req *http.Request) clientIdentity {
	if req == nil || req.TLS == nil || len(req.TLS.PeerCertificates) == 0 {
		return clientIdentity{}
	}
	cert := req.TLS.PeerCertificates[0]
	id := cert.Subject.CommonName
	if id == "" {
		id = cert.SerialNumber.String()
	}
	return clientIdentity{ID: id, Subject: cert.Subject.String(), Serial: cert.SerialNumber.String()}
}

func requestURL(req *http.Request) string {
	if req.URL == nil {
		return ""
	}
	if req.URL.IsAbs() {
		return req.URL.String()
	}
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + req.Host + req.URL.RequestURI()
}

func clientIdentityFromConn(conn net.Conn) clientIdentity {
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return clientIdentity{}
	}
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return clientIdentity{}
	}
	cert := state.PeerCertificates[0]
	id := cert.Subject.CommonName
	if id == "" {
		id = cert.SerialNumber.String()
	}
	return clientIdentity{ID: id, Subject: cert.Subject.String(), Serial: cert.SerialNumber.String()}
}
