package proxyagent

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	_ "github.com/discobox-ai/vm/pkg/machine/archil"

	"github.com/discobox-ai/discobox/proxy"
)

// A machine pool's engine whose provisioning key file holds a sentinel for
// its driver's key calls its driver's service through the pool's proxy, as
// its own client, and the proxy swaps the pool's provider's key in where the
// driver sends it (ADR 26-11-11-080 §9). Driven here with disco-vm's real
// archil driver against a fake Archil API, through the real proxy, with this
// package's resolver and the policy the engine registers in SecretsFile, and
// a fake control plane answering resolve-driver-key:
//
//   - the driver's call reaches Archil with the provider's key, and the
//     control plane is asked for it once, for the engine's sentinel and the
//     host the call went to;
//   - a request carrying the sentinel off the places the driver declares
//     (another path) is refused by the proxy and never sent;
//   - a connection to a host the key is never sent to is dialed direct, not
//     through the proxy;
//   - a new sentinel in the key file is registered at its first use.
func TestEngineSentinelCallsTheDriversServiceThroughThePoolsProxy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const realKey = "key-REALREALREALREALREALREALREALREAL"
	const first = "key-SENTINEL1SENTINEL1SENTINEL1SENT"
	const second = "key-SENTINEL2SENTINEL2SENTINEL2SENT"

	var mu sync.Mutex
	var seen []string
	archil := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+realKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"success":false,"error":"unauthorized"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"data":{"sandboxes":[]}}`)
	}))
	defer archil.Close()
	archilHost := archil.Listener.Addr().String()

	var resolves []resolveDriverKeyBody
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pool-agent/"+testPoolID+"/resolve-driver-key" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body resolveDriverKeyBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		resolves = append(resolves, body)
		mu.Unlock()
		if body.Sentinel != first && body.Sentinel != second {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		expires := time.Now().Add(time.Hour)
		_ = json.NewEncoder(w).Encode(resolveDriverKeyResponse{Value: realKey, ExpiresAt: &expires})
	}))
	defer controlPlane.Close()

	root := withTestRoot(t)
	if err := WriteResolveContext(root, testProjectID, testPoolID, controlPlane.URL, "tok"); err != nil {
		t.Fatal(err)
	}
	bundle, err := PrepareBundle(root, testProjectID, testPoolID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := proxy.DefaultConfig()
	cfg.AllowLoopbackDestinations = true
	cfg.ListenAddress = "127.0.0.1:0"
	cfg.CertDir = bundle.Dir
	cfg.DatabaseDSN = filepath.Join(root.Dir(), "audit.db")
	cfg.Recording.Enabled = false
	resolver := newSecretResolver(root, testProjectID, testPoolID)
	server, err := proxy.NewServer(ctx, cfg, bundle, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.ListenAndServe() }()
	proxyAddr := waitForProxyAddr(t, server)
	policy := newPolicyPublisher(server, cfg, func(err error) { t.Errorf("apply proxy config: %v", err) })
	// The fake Archil's certificate is no public CA's: the engine's client
	// trusts it by a pin, as a person's approval would a sandbox's.
	policy.setTrusts([]proxy.HostTrust{{ClientID: EngineClientID, Host: archilHost, Pin: proxy.TrustPin{Kind: proxy.TrustPinLeafSPKI, SHA256: proxy.SPKISHA256(archil.Certificate())}}})

	keyFile := filepath.Join(t.TempDir(), "key.json")
	writeKey := func(secret string) {
		t.Helper()
		data, err := json.Marshal(machine.Key{Driver: "archil", Name: EngineSentinelKeyName, Secret: secret}) //nolint:gosec // G117: a sentinel, the key file the engine reads
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyFile, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	source := func(context.Context) (machine.Key, error) {
		data, err := os.ReadFile(keyFile)
		if err != nil {
			return machine.Key{}, err
		}
		var key machine.Key
		return key, json.Unmarshal(data, &key)
	}
	writeKey(first)
	driverConfig := []byte(`{"region":"aws-us-east-1","baseUrl":"https://` + archilHost + `"}`)
	sentinel, err := newEngineSentinel(root, testProjectID, testPoolID, "archil", driverConfig, "https://"+proxyAddr)
	if err != nil {
		t.Fatalf("newEngineSentinel() = %v", err)
	}
	// As the agent does before the proxy unit starts.
	if err := sentinel.Register(ctx, source); err != nil {
		t.Fatal(err)
	}
	keys := sentinel.Keys(source)
	// What the proxy unit's watcher does when the file changes.
	load := func() {
		t.Helper()
		if err := policy.loadFile(root.ProxySecretsFile(testProjectID, testPoolID), resolver); err != nil {
			t.Fatal(err)
		}
	}
	load()
	driver, err := machine.New("archil", machine.Config{Pool: "pool-x", Keys: keys, Dial: sentinel.Dial, RootCAs: sentinel.RootCAs, DriverConfig: driverConfig})
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Check(ctx); err != nil {
		t.Fatalf("the driver's Check through the pool's proxy = %v", err)
	}
	mu.Lock()
	if len(seen) != 1 || !strings.HasPrefix(seen[0], "GET /api/sandboxes ") || !strings.HasSuffix(seen[0], "Bearer "+realKey) {
		t.Fatalf("Archil saw %q, want one listing with the provider's key", seen)
	}
	if len(resolves) != 1 || resolves[0].Sentinel != first || resolves[0].Host != "127.0.0.1" {
		t.Fatalf("the control plane was asked %+v, want once, for the sentinel at 127.0.0.1", resolves)
	}
	mu.Unlock()

	// Off the places the driver declares, the sentinel is refused, not sent.
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: sentinel.Dial, TLSClientConfig: machine.Config{RootCAs: sentinel.RootCAs}.TLSConfig()}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+archilHost+"/elsewhere", nil)
	req.Header.Set("Authorization", "Bearer "+first)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	mu.Lock()
	if resp.StatusCode != http.StatusForbidden || len(seen) != 1 {
		t.Fatalf("a request off the driver's places = %d, Archil saw %q; want the proxy's 403 and nothing sent", resp.StatusCode, seen)
	}
	mu.Unlock()

	// A host the key is never sent to is dialed direct.
	var listen net.ListenConfig
	plain, err := listen.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	got := make(chan string, 1)
	go func() {
		conn, err := plain.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 5)
		_, _ = io.ReadFull(conn, buf)
		got <- string(buf)
	}()
	_, port, _ := net.SplitHostPort(plain.Addr().String())
	conn, err := sentinel.Dial(ctx, "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "hello")
	_ = conn.Close()
	if line := <-got; line != "hello" {
		t.Fatalf("a direct host's listener read %q, want the caller's bytes, not a CONNECT", line)
	}

	// A new sentinel is registered at its first use, and works once the
	// proxy has read it.
	writeKey(second)
	if _, err := keys(ctx); err != nil {
		t.Fatal(err)
	}
	load()
	if err := driver.Check(ctx); err != nil {
		t.Fatalf("the driver's Check on a new sentinel = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if last := resolves[len(resolves)-1]; last.Sentinel != second {
		t.Fatalf("the control plane was last asked for %q, want the new sentinel", last.Sentinel)
	}
}
