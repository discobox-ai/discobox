package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/x/gormdb"

	"github.com/discobox-ai/discobox/proxy/gateway"
	"github.com/discobox-ai/discobox/proxy/internal/audit"
)

// gatewayResolver swaps a sentinel as stubResolver does and answers a
// gateway's header values from env, counting the asks.
type gatewayResolver struct {
	stubResolver
	mu   sync.Mutex
	env  map[string]string
	err  error
	asks []GatewayHeadersRequest
}

func (r *gatewayResolver) ResolveGatewayHeaders(_ context.Context, req GatewayHeadersRequest) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asks = append(r.asks, req)
	if r.err != nil {
		return nil, r.err
	}
	out := map[string]string{}
	for _, name := range req.Env {
		if value, ok := r.env[name]; ok {
			out[name] = value
		}
	}
	return out, nil
}

func (r *gatewayResolver) askCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.asks)
}

type gatewayFixture struct {
	server   *Server
	cfg      Config
	client   *http.Client
	dsn      string
	provider *url.URL
	mu       sync.Mutex
	seen     []*http.Request
	provided int
	// status is what the gateway answers with; zero is 200.
	status int
}

const (
	gatewaySentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	gatewayReal     = "sk-ant-oat01-REALSECRETVALUE1234567890abcdefgh"
	gatewayKey      = "gw-key-0123456789"
)

// newGatewayFixture runs a proxy whose sandbox-1 has one gateway sending the
// provider's /v1 to the gateway's /anthropic/v1. The provider and the gateway
// are both on loopback; the provider is reached as 127.0.0.1 and the gateway
// as localhost, so the egress filter can tell them apart.
func newGatewayFixture(t *testing.T, resolver *gatewayResolver, tune func(*Config)) *gatewayFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f := &gatewayFixture{}
	provider := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.provided++
		f.mu.Unlock()
		_, _ = io.WriteString(w, "provider")
	})
	t.Cleanup(provider.Close)
	gw := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Clone(context.Background()))
		status := f.status
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, "gateway")
	})
	t.Cleanup(gw.Close)
	f.provider, _ = url.Parse(provider.URL)
	gwURL, _ := url.Parse(gw.URL)
	gwURL.Host = "localhost:" + gwURL.Port()
	resolver.host = f.provider.Hostname()
	resolver.value = gatewayReal

	route := gateway.Normalize(gateway.Route{
		Name:   "anthropic",
		Match:  provider.URL + "/v1",
		Target: gwURL.String() + "/anthropic/v1",
		RequestHeaders: gateway.HeaderModifier{
			Remove: []string{"X-Remove-Me"},
			Set: []gateway.Header{
				{Name: "Cf-Aig-Authorization", Env: "DISCOBOX_GATEWAY_KEY"},
				{Name: "X-Team", Value: "platform"},
			},
			Add: []gateway.Header{{Name: "X-Tag", Value: "added"}},
		},
	})
	f.server, f.client = startSecretProxy(ctx, t, gatewaySentinel, resolver, func(cfg *Config) {
		f.dsn = cfg.DatabaseDSN
		cfg.Gateways.Clients = map[string][]gateway.Route{"sandbox-1": {route}}
		if tune != nil {
			tune(cfg)
		}
		f.cfg = *cfg
	})
	return f
}

func (f *gatewayFixture) send(t *testing.T, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, f.provider.String()+path, strings.NewReader(`{"model":"claude"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+gatewaySentinel)
	req.Header.Set("X-Remove-Me", "gone")
	req.Header.Set("X-Team", "sandbox-said")
	req.Header.Set("X-Tag", "sandbox")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (f *gatewayFixture) requests() ([]*http.Request, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.seen...), f.provided
}

func readExchanges(t *testing.T, dsn string, want int) []audit.HTTPExchange {
	t.Helper()
	pools, err := gormdb.Open(gormdb.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	deadline := time.Now().Add(2 * time.Second)
	for {
		var rows []audit.HTTPExchange
		if err := pools.Read.Order("id").Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if len(rows) >= want || time.Now().After(deadline) {
			return rows
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A request the gateway matches goes to the gateway, not the provider: the
// rest of its path and its query kept, the credential its sentinel stands for
// swapped in as it would be for the provider, and the gateway's headers
// removed, set, and added. Its audit row is the request as the sandbox sent
// it, with the gateway and where it went beside it, and every header the
// gateway touched redacted.
func TestGatewayForwardsTheRequestAsTheSandboxSentIt(t *testing.T) {
	resolver := &gatewayResolver{env: map[string]string{"DISCOBOX_GATEWAY_KEY": gatewayKey}}
	f := newGatewayFixture(t, resolver, nil)

	status, body := f.send(t, "/v1/messages?beta=true")
	if status != http.StatusOK || body != "gateway" {
		t.Fatalf("status %d, body %q", status, body)
	}
	seen, provided := f.requests()
	if provided != 0 || len(seen) != 1 {
		t.Fatalf("provider saw %d, gateway saw %d", provided, len(seen))
	}
	got := seen[0]
	if got.URL.Path != "/anthropic/v1/messages" || got.URL.RawQuery != "beta=true" {
		t.Errorf("gateway got %s", got.URL)
	}
	if !strings.HasPrefix(got.Host, "localhost:") {
		t.Errorf("Host = %q, want the gateway's", got.Host)
	}
	if got.Header.Get("Authorization") != "Bearer "+gatewayReal {
		t.Errorf("Authorization = %q, want the swapped credential", got.Header.Get("Authorization"))
	}
	if got.Header.Get("Cf-Aig-Authorization") != gatewayKey {
		t.Errorf("Cf-Aig-Authorization = %q", got.Header.Get("Cf-Aig-Authorization"))
	}
	if got.Header.Get("X-Remove-Me") != "" {
		t.Error("a removed header reached the gateway")
	}
	if got.Header.Get("X-Team") != "platform" {
		t.Errorf("X-Team = %q, want the gateway's value replacing the sandbox's", got.Header.Get("X-Team"))
	}
	if tags := got.Header.Values("X-Tag"); len(tags) != 2 || tags[0] != "sandbox" || tags[1] != "added" {
		t.Errorf("X-Tag = %q, want the sandbox's and the gateway's", tags)
	}

	rows := readExchanges(t, f.dsn, 1)
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	row := rows[0]
	if row.URL != f.provider.String()+"/v1/messages?beta=true" || row.Host != f.provider.Host {
		t.Errorf("row records %s at %s, want what the sandbox sent", row.URL, row.Host)
	}
	if row.ForwardedURL != "http://"+got.Host+"/anthropic/v1/messages?beta=true" {
		t.Errorf("ForwardedURL = %q", row.ForwardedURL)
	}
	if row.AppliedRuleID != "anthropic" || row.AppliedPattern != f.provider.String()+"/v1" {
		t.Errorf("gateway recorded as %q %q", row.AppliedRuleID, row.AppliedPattern)
	}
	for _, leaked := range []string{gatewayReal, gatewayKey, "gone"} {
		if strings.Contains(row.RequestHeaders, leaked) {
			t.Errorf("row's headers carry %q: %s", leaked, row.RequestHeaders)
		}
	}
	var headers map[string][]string
	if err := json.Unmarshal([]byte(row.RequestHeaders), &headers); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"X-Remove-Me", "X-Team", "X-Tag", "Authorization"} {
		if values := headers[name]; len(values) == 0 || values[0] != "[REDACTED]" {
			t.Errorf("%s recorded as %q", name, values)
		}
	}
}

// Only a path beneath the match goes to the gateway, on a segment boundary.
func TestGatewayLeavesOtherPathsAlone(t *testing.T) {
	resolver := &gatewayResolver{env: map[string]string{"DISCOBOX_GATEWAY_KEY": gatewayKey}}
	f := newGatewayFixture(t, resolver, nil)
	if status, body := f.send(t, "/v1beta/messages"); status != http.StatusOK || body != "provider" {
		t.Fatalf("status %d, body %q", status, body)
	}
	if resolver.askCount() != 0 {
		t.Fatal("a request no gateway matched asked for header values")
	}
}

// A header value is asked of the control plane once and then kept.
func TestGatewayKeepsWhatTheControlPlaneAnswered(t *testing.T) {
	resolver := &gatewayResolver{env: map[string]string{"DISCOBOX_GATEWAY_KEY": gatewayKey}}
	f := newGatewayFixture(t, resolver, nil)
	for range 3 {
		if status, _ := f.send(t, "/v1/messages"); status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
	}
	if n := resolver.askCount(); n != 1 {
		t.Fatalf("asked %d times", n)
	}
	ask := resolver.asks[0]
	if ask.ClientID != "sandbox-1" || ask.Gateway != "anthropic" || len(ask.Env) != 1 || ask.Env[0] != "DISCOBOX_GATEWAY_KEY" {
		t.Fatalf("asked %+v", ask)
	}
}

// A variable the server does not set refuses the request: it is sent neither
// without its header nor to the provider.
func TestGatewayRefusesWhatItHasNoValueFor(t *testing.T) {
	f := newGatewayFixture(t, &gatewayResolver{}, nil)
	status, body := f.send(t, "/v1/messages")
	if status != http.StatusBadGateway || !strings.Contains(body, "DISCOBOX_GATEWAY_KEY") {
		t.Fatalf("status %d, body %q", status, body)
	}
	if seen, provided := f.requests(); len(seen) != 0 || provided != 0 {
		t.Fatalf("sent anyway: gateway %d, provider %d", len(seen), provided)
	}
	rows := waitForBlockedRows(t, f.dsn, 1)
	if rows[0].AppliedRuleID != "anthropic" || strings.Contains(rows[0].RequestHeaders, gatewayReal) {
		t.Fatalf("row %+v", rows[0])
	}
}

// A control plane that does not answer is a 503 to retry, not a 502.
func TestGatewayHoldsWhenTheControlPlaneDoesNotAnswer(t *testing.T) {
	f := newGatewayFixture(t, &gatewayResolver{err: errors.New("control plane down")}, nil)
	if status, _ := f.send(t, "/v1/messages"); status != http.StatusServiceUnavailable {
		t.Fatalf("status %d", status)
	}
	if seen, provided := f.requests(); len(seen) != 0 || provided != 0 {
		t.Fatalf("sent anyway: gateway %d, provider %d", len(seen), provided)
	}
}

// Egress is judged on the host the sandbox named. The gateway's is never
// filtered: the sandbox never names it.
func TestGatewayTargetIsNotFiltered(t *testing.T) {
	resolver := &gatewayResolver{env: map[string]string{"DISCOBOX_GATEWAY_KEY": gatewayKey}}
	f := newGatewayFixture(t, resolver, func(cfg *Config) {
		cfg.Allowlist.Clients = map[string][]string{"sandbox-1": {"127.0.0.1"}}
	})
	if status, body := f.send(t, "/v1/messages"); status != http.StatusOK || body != "gateway" {
		t.Fatalf("status %d, body %q", status, body)
	}
}

// Removing a gateway leaves the client's requests to their own host.
func TestGatewayRemovedByApplyConfig(t *testing.T) {
	resolver := &gatewayResolver{env: map[string]string{"DISCOBOX_GATEWAY_KEY": gatewayKey}}
	f := newGatewayFixture(t, resolver, nil)
	if status, body := f.send(t, "/v1/messages"); status != http.StatusOK || body != "gateway" {
		t.Fatalf("status %d, body %q", status, body)
	}
	cfg := f.cfg
	cfg.Gateways = GatewaysConfig{}
	if err := f.server.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if status, body := f.send(t, "/v1/messages"); status != http.StatusOK || body != "provider" {
		t.Fatalf("status %d, body %q", status, body)
	}
}

// A value is asked for again once it is stale, and served stale while the
// control plane does not answer (ADR 26-10-14-482 §1).
func TestGatewayServesAStaleValueThroughAnOutage(t *testing.T) {
	resolver := &gatewayResolver{env: map[string]string{"DISCOBOX_GATEWAY_KEY": gatewayKey}}
	f := newGatewayFixture(t, resolver, func(cfg *Config) { cfg.Secrets.ResolveRetrySeconds = 1 })
	if status, _ := f.send(t, "/v1/messages"); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	later := time.Now().Add(gatewayValueTTL + time.Minute)
	values := f.server.http.gatewayValues
	values.mu.Lock()
	values.now = func() time.Time { return later }
	values.mu.Unlock()
	resolver.mu.Lock()
	resolver.err = errors.New("control plane restarting")
	resolver.mu.Unlock()

	if status, body := f.send(t, "/v1/messages"); status != http.StatusOK || body != "gateway" {
		t.Fatalf("status %d, body %q", status, body)
	}
	seen, _ := f.requests()
	if got := seen[len(seen)-1].Header.Get("Cf-Aig-Authorization"); got != gatewayKey {
		t.Fatalf("Cf-Aig-Authorization = %q, want the stale value", got)
	}
	if resolver.askCount() < 2 {
		t.Fatal("a stale value was not asked for again")
	}
}

// A control plane that never answered is a 503 the client is told to retry.
func TestGatewayRefusalWithNothingHeldSaysRetry(t *testing.T) {
	f := newGatewayFixture(t, &gatewayResolver{err: errors.New("down")}, func(cfg *Config) { cfg.Secrets.ResolveRetrySeconds = 1 })
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, f.provider.String()+"/v1/models", nil)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("X-Should-Retry") != "true" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d, headers %v", resp.StatusCode, resp.Header)
	}
}

// A 401 from a gateway that took the swapped credential out is about the
// gateway, not the credential: it is neither retried nor reported.
func TestGateway401OnAWithheldCredentialIsNotTheCredentials(t *testing.T) {
	reports := &reportLog{}
	resolver := &gatewayResolver{env: map[string]string{"DISCOBOX_GATEWAY_KEY": gatewayKey}}
	resolver.reports = reports
	f := newGatewayFixture(t, resolver, nil)
	cfg := f.cfg
	route := cfg.Gateways.Clients["sandbox-1"][0]
	route.RequestHeaders.Remove = append(route.RequestHeaders.Remove, "Authorization")
	cfg.Gateways.Clients = map[string][]gateway.Route{"sandbox-1": {route}}
	if err := f.server.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.status = http.StatusUnauthorized
	f.mu.Unlock()

	if status, _ := f.send(t, "/v1/messages"); status != http.StatusUnauthorized {
		t.Fatalf("status %d", status)
	}
	if seen, _ := f.requests(); len(seen) != 1 {
		t.Fatalf("the gateway was sent %d requests, want no retry", len(seen))
	}
	time.Sleep(100 * time.Millisecond)
	if got := reports.reports(); len(got) != 0 {
		t.Fatalf("reported %+v", got)
	}
}

// A path a server could read as another is refused under a gateway's origin,
// not sent to the gateway nor the provider.
func TestGatewayRefusesAnAmbiguousPath(t *testing.T) {
	resolver := &gatewayResolver{env: map[string]string{"DISCOBOX_GATEWAY_KEY": gatewayKey}}
	f := newGatewayFixture(t, resolver, nil)
	if status, _ := f.send(t, "/v1/..%2F..%2Fadmin"); status != http.StatusBadRequest {
		t.Fatalf("status %d", status)
	}
	if seen, provided := f.requests(); len(seen) != 0 || provided != 0 {
		t.Fatalf("sent: gateway %d, provider %d", len(seen), provided)
	}
}
