package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/elazarl/goproxy"
	"go.opentelemetry.io/otel/attribute"

	"github.com/discobox-ai/discobox/proxy/internal/secrets"
)

// A raw secret is a value a person handed the sandbox itself rather than a
// sentinel (ADR 26-10-27-840). The proxy holds the value too, and watches
// every request the sandbox sends for it, as the sandbox sent it: in the URL,
// in any header, and in the body. A request carrying it off its binding's
// routes is refused; one on them is forwarded and recorded as spending it.
// The routes are the binding sentinel's, checked by the same Authorize every
// sentinel is held to.
//
// It is a guard against mistakes, not a boundary. The value is looked for as
// itself, in base64 and percent-encoded, and inside a header's base64 token;
// anything else — another encoding, a value split or encrypted, a stream the
// proxy does not read — passes.

// rawBodyScanLimit is the largest body of declared length read whole before
// it is sent, so a value in it is refused before any of it goes out. A larger
// one, or one of undeclared length, is watched as it streams instead
// (rawBodyWatch).
const rawBodyScanLimit = 1 << 20

// errRawRefused aborts a streaming body that carried a raw value off its
// routes: the request is cut off before the bytes completing the value are
// sent.
var errRawRefused = errors.New("blocked by proxy: the request body carries a raw secret off its routes")

// rawSpend is what a request spent of raw bindings: the bindings and their
// secrets. It has a lock of its own because a streaming body is watched on the
// transport's goroutine. carried is set once any raw value is found in the
// request, which keeps it out of the response cache.
type rawSpend struct {
	carried    atomic.Bool
	mu         sync.Mutex
	secretIDs  []string
	bindingIDs []string
}

func (s *rawSpend) add(hit secrets.RawHit) {
	s.carried.Store(true)
	s.mu.Lock()
	defer s.mu.Unlock()
	if hit.SecretID != "" && !slices.Contains(s.secretIDs, hit.SecretID) {
		s.secretIDs = append(s.secretIDs, hit.SecretID)
	}
	if hit.BindingID != "" && !slices.Contains(s.bindingIDs, hit.BindingID) {
		s.bindingIDs = append(s.bindingIDs, hit.BindingID)
	}
}

// spentRawBindingIDs is the raw bindings this request carried the values of
// (ADR 26-10-27-840 §5).
func (m *requestMeta) spentRawBindingIDs() []string {
	m.raw.mu.Lock()
	defer m.raw.mu.Unlock()
	return slices.Clone(m.raw.bindingIDs)
}

// spentSecretIDs is every secret this request carried: the ones swapped into
// it and the raw ones found in it.
func (m *requestMeta) spentSecretIDs() []string {
	m.raw.mu.Lock()
	defer m.raw.mu.Unlock()
	if len(m.raw.secretIDs) == 0 {
		return m.swappedSecretIDs
	}
	out := slices.Clone(m.swappedSecretIDs)
	for _, id := range m.raw.secretIDs {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// traceURL is the request's URL as a span may carry it: with the client's raw
// values redacted.
func (h *httpProxy) traceURL(req *http.Request, clientID string) string {
	return h.swapper.Raw(clientID).Redact(requestURL(req))
}

// watchRaw looks for the client's raw values in req and answers a request that
// carries one off its routes, which is never sent. A body is read whole when
// it is small and its length declared, and watched as it streams otherwise.
func (h *httpProxy) watchRaw(req *http.Request, meta *requestMeta, client clientIdentity) *http.Response {
	// Fail closed: a raw value the proxy does not hold yet cannot be watched
	// for, so the request waits for it, as one carrying an unresolved
	// sentinel does, and is refused if it never comes.
	if errs := h.swapper.AwaitRaw(meta.ctx, client.ID); len(errs) > 0 {
		return h.refuseUnresolved(req, meta, client, h.traceURL(req, client.ID), errs)
	}
	set := h.swapper.Raw(client.ID)
	if set == nil {
		return nil
	}
	meta.rawSet = set
	url := requestURL(req)
	hits := set.Find(url)
	for _, values := range req.Header {
		for _, value := range values {
			hits = append(hits, set.Find(value)...)
		}
	}
	if req.Body != nil && req.Body != http.NoBody {
		if req.ContentLength >= 0 && req.ContentLength <= rawBodyScanLimit {
			body, err := io.ReadAll(io.LimitReader(req.Body, rawBodyScanLimit+1))
			_ = req.Body.Close()
			if err != nil {
				meta.answered = true
				return goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusBadRequest, "bad request: the request body could not be read\n")
			}
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
			hits = append(hits, set.FindBytes(body)...)
		} else {
			req.Body = &rawBodyWatch{
				source: req.Body,
				set:    set,
				seen:   map[string]bool{},
				judge: func(found []secrets.RawHit) error {
					if reason, ok := h.judgeRaw(req, meta, client, url, found); !ok {
						h.recordRefusal(req, meta, client, refusalRecord{url: url, header: req.Header, reason: reason, status: http.StatusForbidden})
						return errRawRefused
					}
					return nil
				},
			}
		}
	}
	if len(hits) == 0 {
		return nil
	}
	reason, ok := h.judgeRaw(req, meta, client, url, hits)
	if ok {
		return nil
	}
	status := http.StatusForbidden
	meta.answered = true
	meta.span.SetAttributes(attribute.Bool("proxy.blocked", true), attribute.Int("http.response.status_code", status))
	h.recordRefusal(req, meta, client, refusalRecord{url: url, header: req.Header, reason: reason, status: status})
	return goproxy.NewResponse(req, goproxy.ContentTypeText, status, "blocked by proxy: "+reason+"\n")
}

// judgeRaw holds a request to the routes of every raw binding found in it,
// recording each one it may carry as spent. It answers the refusal's reason,
// naming the binding, when one may not go.
func (h *httpProxy) judgeRaw(req *http.Request, meta *requestMeta, client clientIdentity, url string, hits []secrets.RawHit) (string, bool) {
	if len(hits) > 0 {
		meta.raw.carried.Store(true)
	}
	seen := map[string]bool{}
	for _, hit := range hits {
		if seen[hit.Sentinel] {
			continue
		}
		seen[hit.Sentinel] = true
		verdict, err := h.swapper.Authorize(meta.ctx, secrets.AuthorizeRequest{
			ClientID:  client.ID,
			Sentinels: []string{hit.Sentinel},
			Method:    req.Method,
			Host:      req.Host,
			URL:       url,
			Header:    req.Header,
		})
		if err != nil || !verdict.Allow {
			binding := hit.BindingID
			if binding == "" {
				binding = "a raw secret"
			}
			return "raw: " + binding + ": " + refusalReason(verdict, err), false
		}
	}
	for _, hit := range hits {
		meta.raw.add(hit)
	}
	return "", true
}

// rawBodyWatch watches a body as it is sent for the client's raw values. It
// scans what it read with the bytes before it that a value could straddle,
// and, when a value it finds may not go, fails the read that would complete
// it, so the upstream never receives the whole value.
type rawBodyWatch struct {
	source io.ReadCloser
	set    *secrets.RawSet
	judge  func([]secrets.RawHit) error
	seen   map[string]bool
	tail   []byte
	err    error
}

func (b *rawBodyWatch) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	n, err := b.source.Read(p)
	if n > 0 {
		window := append(b.tail, p[:n]...)
		var fresh []secrets.RawHit
		for _, hit := range b.set.FindBytes(window) {
			if !b.seen[hit.Sentinel] {
				b.seen[hit.Sentinel] = true
				fresh = append(fresh, hit)
			}
		}
		if len(fresh) > 0 {
			if judged := b.judge(fresh); judged != nil {
				b.err = judged
				return 0, judged
			}
		}
		keep := max(b.set.MaxLen()-1, 0)
		if len(window) > keep {
			window = window[len(window)-keep:]
		}
		b.tail = append([]byte(nil), window...)
	}
	return n, err
}

func (b *rawBodyWatch) Close() error { return b.source.Close() }
