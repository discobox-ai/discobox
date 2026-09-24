package proxyagent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
	"github.com/discobox-ai/discobox/proxy"
)

// fakeTrustControlPlane stands in for the control plane's host trust routes:
// it records the ask it is sent, and answers a poll as granted once approve
// has been called, with the pin the ask's chain offered.
type fakeTrustControlPlane struct {
	t        *testing.T
	mu       sync.Mutex
	asked    *createTrustRequestDoc
	approved *hostTrustDoc
}

func (f *fakeTrustControlPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := "/api/pools/" + testPoolID + "/"
	switch {
	case r.Method == http.MethodHead && r.URL.Path == "/":
		// The sandbox agent's port probe, inside a discobox; not the broker.
	case r.Method == http.MethodPost && r.URL.Path == prefix+"sandbox-trust-requests":
		var body createTrustRequestDoc
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("decode trust request: %v", err)
		}
		f.asked = &body
		f.write(w, trustRequestStatusDoc{RequestID: "treq_1", Status: "pending", Host: body.Host})
	case r.Method == http.MethodGet && r.URL.Path == prefix+"sandbox-trust-requests/treq_1":
		status := trustRequestStatusDoc{RequestID: "treq_1", Status: "pending", Host: f.asked.Host}
		if f.approved != nil {
			status.Status = "granted"
			status.Pin = &f.approved.Pin
			status.Uses = f.approved.Uses
		}
		f.write(w, status)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/judge"):
		// A request to a trusted host is judged against the trust's uses
		// (ADR 0149 §5), which this branch asks before resolving anything
		// (ADR 26-09-22-838 §4), so the control plane this fixture stands in for has
		// to answer one. What the judge decides is its own test; here it
		// allows, so this stays a test about the pin taking effect.
		f.write(w, map[string]any{"allow": true, "reason": "that is the approved use"})
	case r.Method == http.MethodGet && r.URL.Path == prefix+"sandbox-host-trusts":
		body := listHostTrustsDoc{HostTrusts: []hostTrustDoc{}}
		if f.approved != nil {
			body.HostTrusts = append(body.HostTrusts, *f.approved)
		}
		f.write(w, body)
	default:
		f.t.Errorf("unexpected control plane call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeTrustControlPlane) write(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		f.t.Errorf("encode control plane answer: %v", err)
	}
}

// approve pins the CA the ask's chain carries, as a person taking the default
// would.
func (f *fakeTrustControlPlane) approve() {
	f.mu.Lock()
	defer f.mu.Unlock()
	ca := f.asked.ObservedChain[len(f.asked.ObservedChain)-1]
	f.approved = &hostTrustDoc{
		ID:        "trust_1",
		SandboxID: "sb-1",
		Host:      f.asked.Host,
		Pin:       trustPinDoc{Kind: agentcreds.PinCA, SHA256: ca.SHA256},
		PinPEM:    ca.PEM,
		Uses:      []credentialUseDoc{{UseID: "use_kube", Description: "read-only kubectl"}},
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

// The whole pool-side path: the broker probes the host through the proxy,
// sends the chain it saw to the control plane, and — when the poll comes back
// granted — has the pin in the proxy before it answers, so the very next
// request through the proxy reaches the host.
func TestTrustRequestProbesThenGrantedPinTakesEffect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	withTestRoot(t)

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer origin.Close()
	endpoint := strings.TrimPrefix(origin.URL, "https://")

	fake := &fakeTrustControlPlane{t: t}
	controlPlaneServer := httptest.NewServer(fake)
	defer controlPlaneServer.Close()
	if err := WriteResolveContext(testProjectID, testPoolID, controlPlaneServer.URL, "tok"); err != nil {
		t.Fatalf("write resolve context: %v", err)
	}

	bundle, err := PrepareBundle(testProjectID, testPoolID)
	if err != nil {
		t.Fatalf("prepare bundle: %v", err)
	}
	material, err := proxy.EnsureClientCertificate(bundle, "sb-1", PoolProxyURL, "", time.Hour, time.Minute)
	if err != nil {
		t.Fatalf("issue client certificate: %v", err)
	}
	live := newActivations()
	cfg := proxy.DefaultConfig()
	cfg.ListenAddress = "127.0.0.1:0"
	cfg.CertDir = bundle.Dir
	cfg.DatabaseDSN = resolve(testProjectID + "-audit.db")
	cfg.Recording.Enabled = false
	server, err := proxy.NewServer(ctx, cfg, bundle, newSecretResolver(testProjectID, testPoolID, live))
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	policy := newPolicyPublisher(server, cfg, live, func(err error) { t.Errorf("apply proxy config: %v", err) })
	go func() { _ = server.ListenAndServe() }()
	controlPlane := newControlPlaneCredentials(testProjectID, testPoolID)
	trusts := newHostTrusts(server, controlPlane, policy, func(err error) { t.Errorf("host trusts: %v", err) })
	broker := &credentialBroker{sandboxID: "sb-1", controlPlan: controlPlane, activations: live, trusts: trusts}

	client := sandboxClient(t, server, material)
	if resp := mustGet(t, client, origin.URL); resp.StatusCode != http.StatusBadGateway || resp.Header.Get(proxy.UntrustedHostHeader) != endpoint {
		t.Fatalf("before the trust: status %d, %s %q", resp.StatusCode, proxy.UntrustedHostHeader, resp.Header.Get(proxy.UntrustedHostHeader))
	}

	status, err := broker.RequestTrust(ctx, agentcreds.TrustRequestBody{
		Host: endpoint,
		Uses: []agentcreds.RequestedUse{{Description: "read-only kubectl"}},
	})
	if err != nil {
		t.Fatalf("request trust: %v", err)
	}
	if status.Status != agentcreds.StatusPending || status.RequestID != "treq_1" {
		t.Fatalf("status = %+v", status)
	}
	sent := fake.asked
	if sent == nil || sent.SandboxID != "sb-1" || sent.Host != endpoint || len(sent.ObservedChain) == 0 {
		t.Fatalf("control plane was sent %+v", sent)
	}
	if got, want := sent.ObservedChain[0].SHA256, proxy.CertificateSHA256(origin.Certificate()); got != want {
		t.Fatalf("observed chain leaf %s, want the origin's own certificate %s", got, want)
	}

	fake.approve()
	granted, err := broker.TrustRequestStatus(ctx, "treq_1")
	if err != nil || granted.Status != agentcreds.StatusGranted || granted.Pin == nil {
		t.Fatalf("poll: %+v, %v", granted, err)
	}
	// No wait: the poll that said granted applied the pin before answering.
	if resp := mustGet(t, client, origin.URL); resp.StatusCode != http.StatusOK {
		t.Fatalf("after the trust: status %d", resp.StatusCode)
	}
	listed, err := broker.Trusts(ctx)
	if err != nil || len(listed) != 1 || listed[0].Host != endpoint || listed[0].Uses[0].UseID != "use_kube" {
		t.Fatalf("trusts = %+v, %v", listed, err)
	}
}

// A CA the agent offers is refused unless the chain the proxy met verifies
// against it: a person is never shown a pin the host did not answer to.
func TestTrustRequestRefusesASuppliedCAThatDoesNotMatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	withTestRoot(t)

	origin := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer origin.Close()

	controlPlaneServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Inside a discobox the sandbox agent probes every listening port with
		// a HEAD /; that is not the broker.
		if r.Method == http.MethodHead && r.URL.Path == "/" {
			return
		}
		t.Errorf("an ask with a CA the host does not answer to reached the control plane: %s %s", r.Method, r.URL.Path)
	}))
	defer controlPlaneServer.Close()
	if err := WriteResolveContext(testProjectID, testPoolID, controlPlaneServer.URL, "tok"); err != nil {
		t.Fatal(err)
	}
	bundle, err := PrepareBundle(testProjectID, testPoolID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := proxy.DefaultConfig()
	cfg.ListenAddress = "127.0.0.1:0"
	cfg.CertDir = bundle.Dir
	cfg.DatabaseDSN = resolve(testProjectID + "-audit.db")
	cfg.Recording.Enabled = false
	server, err := proxy.NewServer(ctx, cfg, bundle, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	controlPlane := newControlPlaneCredentials(testProjectID, testPoolID)
	broker := &credentialBroker{sandboxID: "sb-1", controlPlan: controlPlane, trusts: newHostTrusts(server, controlPlane, nil, nil)}

	// A CA of our own making, which signed nothing the origin presents.
	stranger := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bundle.MITMCA.Certificate[0]})
	_, err = broker.RequestTrust(ctx, agentcreds.TrustRequestBody{
		Host:       strings.TrimPrefix(origin.URL, "https://"),
		Uses:       []agentcreds.RequestedUse{{Description: "x"}},
		SuppliedCA: string(stranger),
	})
	if !errors.Is(err, agentcreds.ErrInvalid) {
		t.Fatalf("error = %v, want invalid", err)
	}
}

// sandboxClient is a sandbox's egress: mTLS to the pool proxy, and trust in
// the MITM CA for everything the proxy intercepts.
func sandboxClient(t *testing.T, server *proxy.Server, material proxy.ClientMaterial) *http.Client {
	t.Helper()
	addr := waitForProxyAddr(t, server)
	cert, err := tls.LoadX509KeyPair(material.ClientCertPath, material.ClientKeyPath)
	if err != nil {
		t.Fatalf("load client keypair: %v", err)
	}
	pool := x509.NewCertPool()
	for _, path := range []string{material.MTLSCAPath, material.MITMCAPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read CA: %v", err)
		}
		pool.AppendCertsFromPEM(data)
	}
	proxyURL, err := url.Parse("https://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"http/1.1"},
		},
	}}
}

// answered is what a sandbox got back, with the body already read and closed.
type answered struct {
	StatusCode int
	Header     http.Header
}

func mustGet(t *testing.T, client *http.Client, target string) answered {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return answered{StatusCode: resp.StatusCode, Header: resp.Header}
}
