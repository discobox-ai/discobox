package proxy

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/elazarl/goproxy"
	"go.opentelemetry.io/otel/attribute"

	"github.com/discobox-ai/discobox/proxy/gateway"
	"github.com/discobox-ai/discobox/proxy/internal/audit"
	"github.com/discobox-ai/discobox/proxy/internal/secrets"
)

// GatewayHeadersRequest asks the control plane for the values of the server
// environment variables one client's gateway takes header values from (ADR
// 26-10-16-470 §4).
type GatewayHeadersRequest struct {
	ClientID string
	Gateway  string
	Env      []string
}

// gatewayValueTTL is how long a resolved header value is fresh: long enough
// that a busy client asks rarely, short enough that a server restarted with a
// new value is in force within it (§4). A stale value is asked for again, and
// still served when the control plane does not answer (ADR 26-10-14-482 §1).
const gatewayValueTTL = 5 * time.Minute

// gatewayValues holds resolved header values, keyed by client, gateway, and
// variable. A value is the server's environment, not a grant: nothing revokes
// it but the gateway's removal, and its age only says when to ask again.
type gatewayValues struct {
	mu      sync.Mutex
	entries map[gatewayValueKey]gatewayValue
	now     func() time.Time
}

type gatewayValueKey struct{ client, gateway, env string }

type gatewayValue struct {
	value   string
	expires time.Time
}

func newGatewayValues() *gatewayValues {
	return &gatewayValues{entries: map[gatewayValueKey]gatewayValue{}, now: time.Now}
}

// lookup is, in one reading, every variable of route held for client,
// whatever its age, and the ones to ask for: absent or stale.
func (v *gatewayValues) lookup(client string, route gateway.Route) (values map[string]string, ask []string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	values = map[string]string{}
	for _, env := range route.Env() {
		entry, ok := v.entries[gatewayValueKey{client, route.Name, env}]
		if ok {
			values[env] = entry.value
		}
		if !ok || !now.Before(entry.expires) {
			ask = append(ask, env)
		}
	}
	return values, ask
}

func (v *gatewayValues) store(client, name string, values map[string]string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	expires := v.now().Add(gatewayValueTTL)
	for env, value := range values {
		v.entries[gatewayValueKey{client, name, env}] = gatewayValue{value: value, expires: expires}
	}
}

// retain drops the values of every client and gateway no longer configured,
// so a gateway removed and added again asks afresh.
func (v *gatewayValues) retain(tables map[string]*gateway.Table) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for key := range v.entries {
		if _, ok := tables[key.client].Lookup(key.gateway); !ok {
			delete(v.entries, key)
		}
	}
}

func buildGatewayTables(cfg GatewaysConfig) map[string]*gateway.Table {
	tables := make(map[string]*gateway.Table, len(cfg.Clients))
	for clientID, routes := range cfg.Clients {
		tables[clientID] = gateway.NewTable(routes)
	}
	return tables
}

// appliedGateway is the gateway a request falls under and what it is
// forwarded with. The request itself stays as the sandbox sent it — that is
// what policy judged and what is audited — and only the copy roundTrip sends
// is rewritten.
type appliedGateway struct {
	route     gateway.Route
	forwarded *url.URL
	values    map[string]string
}

// outbound is the request as the gateway sends it on: the forwarded URL, the
// target's Host, and the route's header changes in the order Gateway API
// applies them.
func (g *appliedGateway) outbound(req *http.Request) *http.Request {
	out := req.Clone(req.Context())
	out.URL = g.forwarded
	out.Host = g.forwarded.Host
	headers := g.route.RequestHeaders
	for _, name := range headers.Remove {
		out.Header.Del(name)
	}
	for _, header := range headers.Set {
		out.Header.Set(header.Name, g.value(header))
	}
	for _, header := range headers.Add {
		out.Header.Add(header.Name, g.value(header))
	}
	return out
}

func (g *appliedGateway) value(header gateway.Header) string {
	if header.Env != "" {
		return g.values[header.Env]
	}
	return header.Value
}

// matchGateway is the gateway client's request falls under, if any, or the
// gateway whose origin it names when its path is ambiguous
// (gateway.ErrAmbiguousPath).
func (h *httpProxy) matchGateway(req *http.Request, clientID string) (*appliedGateway, bool, error) {
	h.mu.RLock()
	table := h.gateways[clientID]
	h.mu.RUnlock()
	route, forwarded, ok, err := table.Match(req.URL)
	if err != nil {
		return &appliedGateway{route: route}, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return &appliedGateway{route: route, forwarded: forwarded}, true, nil
}

// withholds reports whether the gateway removes or replaces a header a
// credential was swapped into, so the credential never reached the upstream
// and what it answers says nothing about it.
func (g *appliedGateway) withholds(swappedHeaders []string) bool {
	touched := map[string]bool{}
	for _, name := range g.route.RequestHeaders.Remove {
		touched[http.CanonicalHeaderKey(name)] = true
	}
	for _, header := range g.route.RequestHeaders.Set {
		touched[http.CanonicalHeaderKey(header.Name)] = true
	}
	for _, name := range swappedHeaders {
		if touched[http.CanonicalHeaderKey(name)] {
			return true
		}
	}
	return false
}

// recordGateway notes on meta the gateway a request falls under, for its
// audit row: the gateway's name and match, the headers it touches — redacted,
// since a value it sets may be a key — and where the request was forwarded.
func (meta *requestMeta) recordGateway(applied *appliedGateway) {
	meta.gateway = applied
	meta.appliedRuleID = applied.route.Name
	meta.appliedPattern = applied.route.Match
	meta.appliedHeaders = applied.route.HeaderNames()
	if applied.forwarded != nil {
		meta.forwardedURL = applied.forwarded.String()
	}
	meta.span.SetAttributes(
		attribute.String("proxy.gateway", applied.route.Name),
		attribute.String("proxy.gateway.forwarded_url", meta.rawSet.Redact(meta.forwardedURL)),
	)
}

// resolveGateway fills in the values of applied's headers that come from the
// server's environment. A request is refused rather than sent without one:
// without its key the gateway would refuse it anyway, less legibly, and
// sending it to the original host would go around the gateway the project
// asked for (§4). A variable the server has no value for is a 502. A control
// plane that does not answer is asked again within the swap's retry window,
// and then a value already held is served, however stale; with none, it is a
// 503 the client retries — the rules ADR 26-10-14-482 gives a credential.
func (h *httpProxy) resolveGateway(req *http.Request, meta *requestMeta, client clientIdentity, applied *appliedGateway) *http.Response {
	route := applied.route
	values, ask := h.gatewayValues.lookup(client.ID, route)
	if len(ask) > 0 {
		if h.resolver == nil {
			return h.refuseGateway(req, meta, client, http.StatusBadGateway,
				fmt.Sprintf("gateway: %s takes headers from the server's environment, and this proxy has no control plane to ask", route.Name))
		}
		fresh, err := secrets.RetryTransient(meta.ctx, time.Now().Add(h.swapper.ResolveRetry()), func(ctx context.Context) (map[string]string, error) {
			return h.resolver.ResolveGatewayHeaders(ctx, GatewayHeadersRequest{ClientID: client.ID, Gateway: route.Name, Env: ask})
		}, func(error) bool { return false })
		if err != nil {
			for _, env := range ask {
				if _, held := values[env]; !held {
					resp := h.refuseGateway(req, meta, client, http.StatusServiceUnavailable,
						fmt.Sprintf("gateway: %s: the control plane did not give its header values: %v", route.Name, err))
					resp.Header.Set("Retry-After", strconv.FormatInt(int64(math.Ceil(h.swapper.ResolveRetry().Seconds())), 10))
					resp.Header.Set("X-Should-Retry", "true")
					return resp
				}
			}
		} else {
			var unset []string
			for _, env := range ask {
				value, ok := fresh[env]
				if !ok || value == "" {
					unset = append(unset, env)
					continue
				}
				values[env] = value
			}
			if len(unset) > 0 {
				return h.refuseGateway(req, meta, client, http.StatusBadGateway,
					fmt.Sprintf("gateway: %s takes a header from %s, which the Discobox server's environment does not set", route.Name, strings.Join(unset, ", ")))
			}
			h.gatewayValues.store(client.ID, route.Name, fresh)
		}
	}
	applied.values = values
	return nil
}

// refuseGateway answers a request a gateway could not forward, and records it
// as blocked.
func (h *httpProxy) refuseGateway(req *http.Request, meta *requestMeta, client clientIdentity, status int, reason string) *http.Response {
	meta.answered = true
	meta.span.SetAttributes(attribute.Bool("proxy.blocked", true), attribute.Int("http.response.status_code", status))
	h.audit.RecordHTTP(audit.HTTPEvent{
		Context:              meta.ctx,
		Time:                 time.Now().UTC(),
		ClientID:             client.ID,
		ClientSubject:        client.Subject,
		ClientSerial:         client.Serial,
		Method:               req.Method,
		URL:                  meta.url(req),
		Host:                 req.Host,
		Status:               status,
		Blocked:              true,
		BlockedReason:        reason,
		AppliedRuleID:        meta.appliedRuleID,
		AppliedPattern:       meta.appliedPattern,
		AppliedHeaders:       meta.appliedHeaders,
		ForwardedURL:         meta.forwardedURL,
		SwappedSecretIDs:     meta.spentSecretIDs(),
		RawBindingIDs:        meta.spentRawBindingIDs(),
		RedactRequestHeaders: meta.redactRequestHeaders(),
		RequestHeaders:       req.Header,
	})
	meta.span.End()
	return goproxy.NewResponse(req, goproxy.ContentTypeText, status, "blocked by proxy: "+reason+"\n")
}
