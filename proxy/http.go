package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/discobox/proxy/internal/audit"
	"github.com/discobox-ai/discobox/proxy/internal/cache"
	"github.com/discobox-ai/discobox/proxy/internal/filter"
	"github.com/discobox-ai/discobox/proxy/internal/rules"
	"github.com/discobox-ai/discobox/proxy/internal/secrets"
	"github.com/elazarl/goproxy"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type httpProxy struct {
	proxy         *goproxy.ProxyHttpServer
	certs         *CertificateBundle
	filter        *filter.Filter
	rewriter      *rules.Rewriter
	swapper       *secrets.Swapper
	cache         *cache.Cache
	audit         *audit.Recorder
	mitmConnect   *goproxy.ConnectAction
	rejectConnect *goproxy.ConnectAction
	// reports carries a refused credential back to the resolver. It outlives
	// every swapper this proxy is given, which is the point of it being here;
	// see credentialreport.go.
	reports *credentialReporter
	mu      sync.RWMutex
	ids     map[string]clientIdentity
	// trusts is the pins in force (ADR 0149), replaced whole by ApplyConfig.
	trusts *trustTable
}

type requestMeta struct {
	ctx      context.Context
	span     trace.Span
	start    time.Time
	client   clientIdentity
	cacheHit bool
	// answered is a request the proxy refused and answered itself, and has
	// already recorded as blocked. Its response is the proxy's own, so the
	// response path neither records it again nor treats it as the upstream's
	// word on a credential.
	answered bool
	// authorized is a request swapSecrets already put to the authorizer. A
	// request to a trusted host that carried no sentinel was never asked
	// about there, and is asked about on its own below; one that carried a
	// sentinel was asked about against both, and must not be asked twice.
	authorized           bool
	cacheKey             string
	cacheError           string
	appliedRuleID        string
	appliedPattern       string
	appliedHeaders       []string
	requestBodyRecord    *audit.BodyRecord
	requestBodySpool     *audit.BodySpool
	requestBodyError     string
	requestBodyCloseOnce sync.Once
	requestBodyBytes     int64
	swappedHeaders       []string
	// swappedUseIDs names the approved credential uses this request spent, for
	// the audit row's join to the control plane's verdict trail (ADR 0130 §3).
	//
	// The unauthorized retry (ADR 0059) does not add to it. The retry re-swaps
	// the same sentinels for the same client and host, which is the same
	// activation and so the same uses, and the 401 and the retry are audited as
	// two rows off this one meta — so writing the retry's result here would
	// backdate it onto the 401's row, which is written after the retry is
	// chosen.
	swappedUseIDs []string
	// swappedSentinels is which sentinels this request carried, kept beside the
	// header names they were found in because a rejection has to name the
	// credential and a header name cannot (ADR 0132 §1).
	swappedSentinels []string
	auditURL         string
	// preSwapHeader is the request's headers as the sandbox sent them, with the
	// sentinels still in place. It is what a retry re-swaps from; re-swapping
	// the outbound headers would look for a sentinel that is no longer there.
	preSwapHeader http.Header
	// retryBody holds the request body when the request is eligible for an
	// unauthorized retry, which is the only reason it is in memory at all.
	retryBody []byte
	retryable bool
	retried   bool
	// trust is the client's pin for this request's endpoint, when it holds
	// one: the request goes out on the trust's own transport, and is judged
	// against the uses the host was trusted for.
	trust *pinnedTrust
}

type responseStream struct {
	source       io.ReadCloser
	req          *http.Request
	meta         *requestMeta
	audit        *audit.Recorder
	cacheStore   *cache.StreamingPut
	cacheMatcher *cache.Matcher
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

type requestBodyStream struct {
	source io.ReadCloser
	meta   *requestMeta
}

func newHTTPProxy(certs *CertificateBundle, flt *filter.Filter, rewriter *rules.Rewriter, swapper *secrets.Swapper, c *cache.Cache, recorder *audit.Recorder) *httpProxy {
	p := goproxy.NewProxyHttpServer()
	p.Verbose = false
	h := &httpProxy{proxy: p, certs: certs, filter: flt, rewriter: rewriter, swapper: swapper, cache: c, audit: recorder, ids: map[string]clientIdentity{}}
	// Through the current swapper rather than the one held at construction:
	// ApplyConfig replaces it, and a report belongs to whichever resolver is
	// live when it goes out.
	h.reports = newCredentialReporter(func(ctx context.Context, req secrets.ReportRequest) error {
		return h.secretSwapper().Report(ctx, req)
	})
	h.setupMITM()
	h.setupHandlers()
	return h
}

func (h *httpProxy) setPolicy(flt *filter.Filter, rewriter *rules.Rewriter) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.filter = flt
	h.rewriter = rewriter
}

func (h *httpProxy) setSwapper(swapper *secrets.Swapper) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.swapper = swapper
}

func (h *httpProxy) policy() (*filter.Filter, *rules.Rewriter) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.filter, h.rewriter
}

func (h *httpProxy) secretSwapper() *secrets.Swapper {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.swapper
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
		flt, _ := h.policy()
		// The gate host is intercepted whatever the allowlist says: it never
		// reaches the internet, and what may reach it is the gate's to decide.
		if h.secretSwapper().IsGate(host) {
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
		return h.mitmConnect, host
	})

	h.proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		client := h.clientIdentity(req)
		traceCtx, span := proxyTracer().Start(req.Context(), "proxy.http.request", trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(append(clientAttrs(client),
				attribute.String("http.request.method", req.Method),
				attribute.String("url.full", requestURL(req)),
				attribute.String("server.address", req.Host),
			)...),
		)
		*req = *req.WithContext(traceCtx)
		meta := &requestMeta{ctx: traceCtx, span: span, start: time.Now(), client: client}
		ctx.UserData = meta
		// Every request leaves through roundTrip, which picks the transport a
		// pin calls for and answers a refused upstream certificate itself.
		ctx.RoundTripper = goproxy.RoundTripperFunc(h.roundTrip)

		if swapper := h.secretSwapper(); swapper.IsGate(req.Host) {
			return req, h.serveGate(req, meta, client, swapper)
		}

		flt, rewriter := h.policy()
		if !flt.AllowHostForClient(req.Host, client.ID) {
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
		meta.trust = h.trustTable().lookup(client.ID, requestEndpoint(req), time.Now())

		if matcher := h.cache.Matcher(); matcher != nil && matcher.ShouldCache(req) {
			key := matcher.GenerateKey(req)
			meta.cacheKey = key
			lookupCtx, lookupSpan := proxyTracer().Start(traceCtx, "proxy.cache.lookup",
				trace.WithAttributes(attribute.String("proxy.cache.key", key)),
			)
			if entry, err := h.cache.Get(key); err == nil {
				meta.cacheHit = true
				lookupSpan.SetAttributes(attribute.Bool("proxy.cache.hit", true))
				lookupSpan.End()
				meta.ctx = lookupCtx
				resp := cache.RestoreResponse(entry, req)
				span.SetAttributes(attribute.Bool("proxy.cache.hit", true), attribute.Int("http.response.status_code", resp.StatusCode))
				if resp.Body == nil {
					h.audit.RecordHTTP(h.auditEvent(req, resp, meta, time.Since(meta.start), false))
					span.End()
					return req, resp
				}
				bodyRecord, bodySpool, err := h.beginResponseBody(meta)
				if err != nil {
					meta.cacheError = err.Error()
					recordSpanError(span, err)
				}
				resp.Body = &responseStream{
					source:     resp.Body,
					req:        req,
					meta:       meta,
					audit:      h.audit,
					bodyRecord: bodyRecord,
					bodySpool:  bodySpool,
					status:     resp.StatusCode,
					headers:    resp.Header.Clone(),
				}
				return req, resp
			} else if !errors.Is(err, cache.ErrMiss) {
				meta.cacheError = err.Error()
			}
			lookupSpan.SetAttributes(attribute.Bool("proxy.cache.hit", false))
			lookupSpan.End()
		}

		_, rewriteSpan := proxyTracer().Start(traceCtx, "proxy.header_rewrite")
		match := rewriter.Apply(req, client.ID)
		if match.Matched {
			meta.appliedRuleID = match.RuleID
			meta.appliedPattern = match.Pattern
			meta.appliedHeaders = match.Headers
			rewriteSpan.SetAttributes(
				attribute.Bool("proxy.header_rewrite.matched", true),
				attribute.String("proxy.header_rewrite.rule_id", match.RuleID),
				attribute.String("proxy.header_rewrite.pattern", match.Pattern),
			)
		} else {
			rewriteSpan.SetAttributes(attribute.Bool("proxy.header_rewrite.matched", false))
		}
		rewriteSpan.End()
		if refused := h.swapSecrets(req, meta, client); refused != nil {
			span.End()
			return req, refused
		}
		// A request to a trusted host is judged whether or not it carries a
		// credential; one that carried a sentinel was judged against both in
		// swapSecrets, which is what meta.authorized records.
		if meta.trust != nil && !meta.authorized {
			if refused := h.authorizeSwap(req, meta, client, requestURL(req), req.Header.Clone(), nil); refused != nil {
				span.End()
				return req, refused
			}
			meta.authorized = true
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
		if len(meta.swappedSentinels) > 0 && succeeded(resp.StatusCode) {
			h.reports.accepted(meta.client.ID, ctx.Req.Host, meta.swappedSentinels)
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
				streamRecord, stream, err := h.audit.BeginUpgradeStream(meta.client.ID, upgradedProtocol)
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

		var store *cache.StreamingPut
		if matcher := h.cache.Matcher(); matcher != nil && matcher.ShouldCache(ctx.Req) && matcher.ShouldCacheResponse(resp) {
			key := matcher.GenerateKey(ctx.Req)
			meta.cacheKey = key
			_, cacheSpan := proxyTracer().Start(meta.ctx, "proxy.cache.store.start", trace.WithAttributes(attribute.String("proxy.cache.key", key)))
			var err error
			store, err = h.cache.BeginStreamingPut(key, resp)
			if err != nil {
				meta.cacheError = err.Error()
				recordSpanError(cacheSpan, err)
				store = nil
			}
			cacheSpan.End()
		}
		if resp.Body == nil {
			h.audit.RecordHTTP(h.auditEvent(ctx.Req, resp, meta, time.Since(meta.start), false))
			meta.span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
			meta.span.End()
			return resp
		}
		bodyRecord, bodySpool, err := h.beginResponseBody(meta)
		if err != nil {
			meta.cacheError = err.Error()
			recordSpanError(meta.span, err)
		}
		resp.Body = &responseStream{
			source:       resp.Body,
			req:          ctx.Req,
			meta:         meta,
			audit:        h.audit,
			cacheStore:   store,
			cacheMatcher: h.cache.Matcher(),
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
	if resp != nil && resp.ContentLength > 0 {
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
		SwappedUseIDs:        meta.swappedUseIDs,
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
// A request carrying sentinels is authorized first, and one that is not
// allowed is answered here, with the response swapSecrets returns, and never
// sent. Authorizing before resolving is the point: a refused request decrypts
// nothing, and a value already in the cache does not carry a new operation
// through on the strength of an older one (ADR 26-09-22-838 §4).
func (h *httpProxy) swapSecrets(req *http.Request, meta *requestMeta, client clientIdentity) *http.Response {
	swapper := h.secretSwapper()
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
	meta.authorized = true
	result := swapper.Apply(meta.ctx, req, client.ID)
	if !result.Swapped() {
		span.SetAttributes(attribute.Bool("proxy.secret_swap.swapped", false))
		if len(result.Errors) > 0 {
			span.SetAttributes(attribute.Int("proxy.secret_swap.errors", len(result.Errors)))
		}
		return nil
	}
	meta.swappedHeaders = result.Headers
	meta.swappedUseIDs = result.UseIDs
	meta.swappedSentinels = result.Sentinels
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

// serveGate answers a request for the gate host, which is never sent to the
// internet (ADR 0140 §2). What the resolver admits is answered by the upstream
// behind the gate and recorded like any exchange, a 502 included when the gate
// fails after letting it in; what it refuses is answered here with a 403 and
// recorded once, as blocked. Nothing is swapped on
// the way: the sentinel is the resolver's to recognize and remove.
func (h *httpProxy) serveGate(req *http.Request, meta *requestMeta, client clientIdentity, swapper *secrets.Swapper) *http.Response {
	admitted, err := swapper.Gate(meta.ctx, secrets.GateRequest{ClientID: client.ID, Request: req})
	var refusal *secrets.GateRefusal
	if err == nil || !errors.As(err, &refusal) {
		// Let in. The use it was let in under is recorded on its row, as a
		// swapped credential's is: it is what joins the request to the
		// approval that allowed it.
		if admitted.UseID != "" {
			meta.swappedUseIDs = []string{admitted.UseID}
		}
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
	var useIDs []string
	if refusal.UseID != "" {
		useIDs = []string{refusal.UseID}
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
		SwappedUseIDs:  useIDs,
		RequestHeaders: req.Header,
	})
	meta.span.End()
	return goproxy.NewResponse(req, goproxy.ContentTypeText, status, "blocked by proxy: "+err.Error())
}

// authorizeSwap asks whether a request may carry the credentials its sentinels
// stand for, or go to a host the client trusts by a pin, and answers it here
// when it may not. What is asked about is the request as the sandbox sent it —
// nothing has been resolved yet, so it holds sentinels and never a credential
// — and a refusal is audited the same way as one the judge returns. An
// authorizer that cannot answer refuses: a credential is resolved only for a
// request something agreed to.
func (h *httpProxy) authorizeSwap(req *http.Request, meta *requestMeta, client clientIdentity, preURL string, preSwapHeader http.Header, matched []string) *http.Response {
	var trustUseIDs []string
	if meta.trust != nil {
		trustUseIDs = meta.trust.UseIDs
	}
	verdict, err := h.secretSwapper().Authorize(meta.ctx, secrets.AuthorizeRequest{
		ClientID:    client.ID,
		Sentinels:   matched,
		TrustUseIDs: trustUseIDs,
		Method:      req.Method,
		Host:        req.Host,
		URL:         preURL,
		Header:      preSwapHeader,
	})
	if err == nil && verdict.Allow {
		return nil
	}
	reason := refusalReason(verdict, err)
	if verdict.Reason == "" && err == nil && len(matched) == 0 {
		// Nothing here spends a credential, so the sentence about one would be
		// the wrong one: this request was refused for where it is going.
		reason = "not an approved use of this host"
	}
	meta.answered = true
	meta.span.SetAttributes(attribute.Bool("proxy.blocked", true), attribute.Int("http.response.status_code", http.StatusForbidden))
	h.recordRefusal(req, meta, client, refusalRecord{
		url:    preURL,
		header: preSwapHeader,
		reason: reason,
		useIDs: verdict.UseIDs,
		status: http.StatusForbidden,
	})
	return goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusForbidden, "blocked by proxy: "+reason)
}

// refusalReason is what a refused request is answered with, whichever way the
// ask ended. A verdict that refuses and says nothing still has to say
// something: a refusal nobody can read is indistinguishable from a broken
// credential (ADR 26-09-22-838 §1).
func refusalReason(verdict secrets.Verdict, err error) string {
	switch {
	case err != nil:
		return "the judge could not decide: " + err.Error()
	case verdict.Reason == "":
		return "not an approved use of this credential"
	default:
		return verdict.Reason
	}
}

// refusalRecord is one refused request as the audit trail keeps it: the URL and
// headers as the sandbox sent them, why it was refused, the uses it was refused
// under, and the status the sandbox actually received.
//
// The status is the caller's because a refusal is not always the answer. A
// first attempt refused before it is sent is answered 403 here; a refused
// retry is not answered at all, and the sandbox keeps whatever the upstream
// already said, so claiming a 403 would put a response in the trail that
// nobody received.
type refusalRecord struct {
	url    string
	header http.Header
	reason string
	useIDs []string
	status int
}

// recordRefusal writes the blocked row a refusal leaves behind. Nothing was
// substituted, so the uses come from the verdict — a refusal is still recorded
// against what it was about (ADR 26-09-22-838 §8).
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
		BlockedReason:  "judge: " + rec.reason,
		SwappedUseIDs:  rec.useIDs,
		RequestHeaders: rec.header,
	})
}

// unauthorizedRetryMaxBody bounds what a retryable request holds in memory. A
// body larger than this is streamed as before and its 401 is passed through:
// buffering a request of unbounded size to make a retry possible would trade a
// rare recoverable failure for a common memory one.
const unauthorizedRetryMaxBody = 8 << 20

// bufferRetryBody reads a swapped request's body into memory so the request can
// be sent a second time if the upstream rejects the credential. A body that
// does not fit is left streaming and the request stops being retryable.
func (h *httpProxy) bufferRetryBody(req *http.Request, meta *requestMeta) {
	if !meta.retryable || req.Body == nil || req.Body == http.NoBody {
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
	if len(meta.swappedHeaders) == 0 || len(meta.swappedSentinels) == 0 {
		// Not a request this proxy put a credential into: the 401 belongs to
		// whatever the sandbox sent, and is neither ours to retry nor ours to
		// report.
		return nil
	}
	swapper := h.secretSwapper()
	if swapper == nil {
		return nil
	}
	// A request that cannot be retried — a body too large to hold, a swap into
	// the URL — is still a refused credential, and the one thing that can be
	// said about it is that it was refused.
	if !meta.retryable || meta.preSwapHeader == nil {
		h.reports.rejected(meta.client.ID, ctx.Req.Host, meta.swappedSentinels, secrets.OutcomeRejected)
		return nil
	}
	req := ctx.Req
	meta.retried = true

	_, span := proxyTracer().Start(meta.ctx, "proxy.secret_swap.retry")
	defer span.End()

	// A retry carries a credential the first attempt did not, so it is
	// authorized the way the first attempt was (ADR 26-09-22-838 §4): the same
	// derivation, over the request about to be sent, because the grant behind
	// it can have been revoked in the time the upstream took to refuse.
	//
	// The set comes from Match and not from what the first attempt managed to
	// resolve. The swaps below re-scan every sentinel from scratch, so a
	// sentinel that failed transiently the first time can resolve now — and
	// would otherwise go upstream under a verdict that never named it.
	previous := h.rebuiltRequest(req, meta)
	matched := swapper.Match(previous, meta.client.ID)
	var retryTrustUseIDs []string
	if meta.trust != nil {
		retryTrustUseIDs = meta.trust.UseIDs
	}
	verdict, err := swapper.Authorize(meta.ctx, secrets.AuthorizeRequest{
		ClientID:    meta.client.ID,
		Sentinels:   matched,
		TrustUseIDs: retryTrustUseIDs,
		Method:      req.Method,
		Host:        req.Host,
		URL:         meta.url(req),
		Header:      meta.preSwapHeader,
	})
	if err != nil || !verdict.Allow {
		// Not an answer to the sandbox: the request has already been sent and
		// answered, so the upstream's own 401 stands. What the refusal owes is
		// the trail — the retry is refused where the first attempt was allowed,
		// and ADR 26-09-22-838 §8 wants that recorded rather than only traced.
		span.SetAttributes(attribute.Bool("proxy.secret_swap.retry.authorized", false))
		h.recordRefusal(req, meta, meta.client, refusalRecord{
			url:    meta.url(req),
			header: meta.preSwapHeader,
			reason: refusalReason(verdict, err),
			useIDs: verdict.UseIDs,
			// What the sandbox got is the upstream's own answer, which is
			// going down the wire unchanged.
			status: resp.StatusCode,
		})
		h.reports.rejected(meta.client.ID, req.Host, meta.swappedSentinels, secrets.OutcomeRejected)
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
	case resolvedResult.Swapped() && !sameHeaderValues(req.Header, resolved.Header, meta.swappedHeaders):
		retryReq, source = resolved, "resolved"
	case previousResult.Swapped() && !sameHeaderValues(req.Header, previous.Header, meta.swappedHeaders):
		retryReq, source = previous, "previous"
	default:
		// Nothing to send that is not the value just refused, which means the
		// credential the resolver would hand out now is the rejected one.
		span.SetAttributes(attribute.Bool("proxy.secret_swap.retry.attempted", false))
		h.reports.rejected(meta.client.ID, req.Host, meta.swappedSentinels, secrets.OutcomeRejected)
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
		h.reports.rejected(meta.client.ID, req.Host, meta.swappedSentinels, secrets.OutcomeRejectedAfterRetry)
	case succeeded(retryResp.StatusCode):
		h.reports.accepted(meta.client.ID, req.Host, meta.swappedSentinels)
	}
	ctx.Req = retryReq
	meta.start = time.Now()
	return retryResp
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
	// The s2c direction needs no such care: its bytes reach the client only
	// after Read returns, which is after its own Add.
	s.c2sBytes.Add(int64(len(p)))
	n, err := s.source.Write(p)
	if n < len(p) {
		s.c2sBytes.Add(int64(n) - int64(len(p)))
	}
	if n > 0 && s.stream != nil {
		s.stream.RecordChunk(audit.StreamClientToServer, p[:n])
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
		SwappedUseIDs:        s.meta.swappedUseIDs,
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
		responseBodyFile, responseBodyFormat, responseBodyBytes, responseBodyError := s.responseBodyMetadata()
		responseBodyError = mergeResponseBodyError(responseBodyError, readErr)
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
			SwappedUseIDs:        s.meta.swappedUseIDs,
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

func (s *responseStream) responseBodyMetadata() (file string, format string, bytes int64, errText string) {
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

// redactRequestHeaders returns the header names to redact in audit: rewrite-rule
// headers plus any header whose value was secret-swapped.
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
