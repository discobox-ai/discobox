package discovm

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/pool-agent/cpchannel"
	"github.com/discobox-ai/discobox/pool-agent/cpmux"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// testEdgeHeader is the header the test driver's credential is sent in.
const testEdgeHeader = "X-Test-Edge"

// guestEdgeCredential is the credential placed for the pool's sandboxes, as
// the pool's machine holds it now.
func guestEdgeCredential(t *testing.T, r *Runtime, pool *model.Pool) machine.Credential {
	t.Helper()
	data, err := readGuestFile(poolGuest(t, r, pool), bundle.SandboxEdgeCredentialFile)
	if err != nil {
		t.Fatalf("read the sandboxes' credential: %v", err)
	}
	var credential machine.Credential
	if err := json.Unmarshal(data, &credential); err != nil {
		t.Fatal(err)
	}
	return credential
}

// On a driver whose URLs answer only a credential, the server places one for
// the pool's sandboxes beside the bootstrap, records it without its token, and
// keeps it through its first half-life; past that a reconcile places a new one,
// and restarts nothing, since the agent reads it as it decides each sandbox's
// document (ADR 26-11-16-619 §2).
func TestFakePoolMachinePlacesItsSandboxesEdgeCredential(t *testing.T) {
	r, driver := newFakePool(t, "")
	driver.edge = true
	ctx := context.Background()
	pool := &model.Pool{ID: "pool_0123456789abcdef", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	maker := newTestMaker()
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatalf("EnsurePool() error = %v", err)
	}
	first := guestEdgeCredential(t, r, pool)
	record := recordOf(t, pool)
	if first.Header != testEdgeHeader || first.Token == "" || record.SandboxEdge == nil || !record.SandboxEdge.Expires.Equal(first.Expires) {
		t.Fatalf("placed %+v, recorded %+v, want the pool URL's credential placed and its expiry recorded", first, record.SandboxEdge)
	}
	if strings.Contains(string(pool.RuntimeState), first.Token) {
		t.Fatal("the pool's record holds the sandboxes' token")
	}
	if strings.Contains(string(mustJSON(t, driver.applied[0])), first.Token) {
		t.Fatal("the bootstrap carries the sandboxes' token")
	}
	applies := driver.applies()

	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatalf("EnsurePool() again: %v", err)
	}
	if again := guestEdgeCredential(t, r, pool); again.Token != first.Token {
		t.Fatal("a credential in its first half-life was replaced")
	}

	// Past half its life: replaced, in place.
	record = recordOf(t, pool)
	record.SandboxEdge.PlacedAt = time.Now().Add(-13 * time.Hour)
	record.SandboxEdge.Expires = time.Now().Add(11 * time.Hour)
	pool.RuntimeState = mustJSON(t, record)
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatalf("EnsurePool() past half-life: %v", err)
	}
	renewed := guestEdgeCredential(t, r, pool)
	if renewed.Token == first.Token || !recordOf(t, pool).SandboxEdge.Expires.Equal(renewed.Expires) {
		t.Fatalf("renewed %+v, recorded %+v, want a new credential placed and recorded", renewed, recordOf(t, pool).SandboxEdge)
	}
	if driver.applies() != applies || len(driver.restarts) != 0 {
		t.Fatalf("a renewal applied %d times and restarted %v, want nothing", driver.applies()-applies, driver.restarts)
	}
}

// A driver whose URLs need no credential places none, and records none.
func TestFakePoolMachinePlacesNoEdgeCredentialWithoutAnEdge(t *testing.T) {
	r, _ := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool_0123456789abcdef", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatalf("EnsurePool() error = %v", err)
	}
	if recordOf(t, pool).SandboxEdge != nil {
		t.Fatal("a pool with no edge recorded a sandboxes' credential")
	}
	if _, err := readGuestFile(poolGuest(t, r, pool), bundle.SandboxEdgeCredentialFile); err == nil {
		t.Fatal("a pool with no edge was placed a sandboxes' credential")
	}
}

// The server reaches the pool's agent with a credential of its own, never the
// sandboxes': held across leases until half its life has passed, sent on every
// request, over HTTP/1.1 even to a front that offers HTTP/2.
func TestAgentClientCarriesTheServersOwnEdgeCredential(t *testing.T) {
	r, driver := newFakePool(t, "")
	driver.edge = true
	ctx := context.Background()
	pool := &model.Pool{ID: "pool_0123456789abcdef", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatalf("EnsurePool() error = %v", err)
	}
	sandboxes := guestEdgeCredential(t, r, pool)
	// One each: the server's ingress URL and pool URL, held, and the
	// sandboxes' own.
	if got := driver.mints(); got != 3 {
		t.Fatalf("making the pool minted %d credentials, want 3", got)
	}
	// Every reconcile runs once a minute: one whose credentials are in their
	// first half-life mints none.
	for range 3 {
		if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := r.AcquirePoolAgentClient(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.AcquirePoolAgentClient(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if got := driver.mints(); got != 3 {
		t.Fatalf("reconciles and leases minted %d credentials in all, want the 3 held", got)
	}
	if lease.Client != again.Client {
		t.Fatal("two leases of the pool's agent built two clients, so they share no connections")
	}

	var mu sync.Mutex
	var seen []string
	front := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Proto+" "+r.Header.Get(testEdgeHeader))
		mu.Unlock()
	}))
	front.EnableHTTP2 = true
	front.StartTLS()
	t.Cleanup(front.Close)
	for _, l := range []string{"first", "second"} {
		client := lease.Client
		if l == "second" {
			client = again.Client
		}
		held, ok := client.Transport.(edgeCredential)
		if !ok {
			t.Fatalf("the lease's transport is %T, want one that sends the edge's credential", client.Transport)
		}
		// The same client, trusting the test front.
		base, _ := front.Client().Transport.(*http.Transport)
		base.ForceAttemptHTTP2 = true
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, front.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := edgeHTTPClient(base, held.credential).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if held.credential.Token == sandboxes.Token {
			t.Fatal("the server reaches the pool's agent with the sandboxes' credential")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != seen[1] || !strings.HasPrefix(seen[0], "HTTP/1.1 edge-token-") {
		t.Fatalf("the front saw %q, want both requests over HTTP/1.1 with the one credential held", seen)
	}
}

// credentialedFront is a provider's edge in front of a pool agent's API that
// answers 401 to a request without its credential, before anything reaches
// the agent, and offers HTTP/2, as Archil's does (ADR 26-11-16-619 §2).
func credentialedFront(t *testing.T, backend, token string) *httptest.Server {
	t.Helper()
	target, err := url.Parse(backend)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 || r.Header.Get(testEdgeHeader) != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	front.EnableHTTP2 = true
	front.StartTLS()
	t.Cleanup(front.Close)
	return front
}

// The control-plane channel opens through an edge that answers 401 without
// its credential once the server's client carries it, and not before.
func TestTheChannelOpensThroughACredentialedEdge(t *testing.T) {
	_, serverKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	poolPublic, poolKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverPublic, _ := serverKey.Public().(ed25519.PublicKey)
	channel, err := cpchannel.New(slog.New(slog.DiscardHandler), poolKey, serverPublic)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(cpchannel.Path("proj", "pool"), channel)
	agentAPI := httptest.NewServer(mux)
	t.Cleanup(agentAPI.Close)
	front := credentialedFront(t, agentAPI.URL, "server-token")
	base, _ := front.Client().Transport.(*http.Transport)
	base.ForceAttemptHTTP2 = true

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if conn, err := cpchannel.Dial(ctx, base, front.URL, "proj", "pool", "token", serverKey, poolPublic); err == nil {
		_ = conn.Close()
		t.Fatal("the channel opened without the edge's credential")
	} else if !strings.Contains(err.Error(), "401") {
		t.Fatalf("Dial without the edge's credential = %v, want the edge's 401", err)
	}

	client := edgeHTTPClient(base, machine.Credential{Header: testEdgeHeader, Token: "server-token", Expires: time.Now().Add(time.Hour)})
	conn, err := cpchannel.Dial(ctx, client.Transport, front.URL, "proj", "pool", "token", serverKey, poolPublic)
	if err != nil {
		t.Fatalf("Dial with the edge's credential: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := cpmux.Client(conn); err != nil {
		t.Fatal(err)
	}
	if err := channel.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Closing a credentialed client's idle connections reaches its transport, so
// a machine forgotten or a credential renewed leaves no connection to the edge
// idle behind it.
func TestEdgeClientClosesItsIdleConnections(t *testing.T) {
	closed := make(chan struct{}, 1)
	front := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	front.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	front.StartTLS()
	t.Cleanup(front.Close)
	base, _ := front.Client().Transport.(*http.Transport)
	client := edgeHTTPClient(base, machine.Credential{Header: testEdgeHeader, Token: "t", Expires: time.Now().Add(time.Hour)})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, front.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	client.CloseIdleConnections()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the edge's idle connection was left open")
	}
}
