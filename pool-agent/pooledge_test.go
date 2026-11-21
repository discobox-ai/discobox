package poolagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

func writeCredential(t *testing.T, path string, credential machine.Credential) {
	t.Helper()
	data, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A pool behind an edge gives each sandbox its host's address as it last
// resolved it, IPv4 first, and the credential the server placed, as the file
// holds it now (ADR 26-11-16-619 §§2–3). It looks the host up again only once
// poolEdgeResolveEvery has passed, and keeps the address it had when a lookup
// fails, since the edge routes by name.
func TestPoolEdgeIsTheAddressLastResolvedAndTheCredentialPlaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-edge-credential.json")
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	expires := now.Add(24 * time.Hour)
	writeCredential(t, path, machine.Credential{Header: "X-Archil-Token", Token: "first", Expires: expires})
	edge := newPoolEdge("7302-pool.edge.example", path)
	edge.now = func() time.Time { return now }
	lookups := 0
	answer := []netip.Addr{netip.MustParseAddr("2001:db8::7"), netip.MustParseAddr("::ffff:192.0.2.7")}
	var lookupErr error
	edge.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host != "7302-pool.edge.example" {
			t.Fatalf("looked up %q, want the pool's host", host)
		}
		lookups++
		return answer, lookupErr
	}

	got := edge.edge()
	want := sandboxconfig.RuntimeEdge{Address: "192.0.2.7", Header: "X-Archil-Token", Token: "first", Expires: expires}
	if *got != want {
		t.Fatalf("edge = %+v, want %+v", *got, want)
	}

	// Renewed by the server: taken at once. The address is not looked up again
	// within the interval.
	writeCredential(t, path, machine.Credential{Header: "X-Archil-Token", Token: "second", Expires: expires.Add(time.Hour)})
	answer = []netip.Addr{netip.MustParseAddr("192.0.2.8")}
	if got = edge.edge(); got.Token != "second" || got.Address != "192.0.2.7" || lookups != 1 {
		t.Fatalf("edge = %+v after %d lookups, want the renewed credential and the address held", got, lookups)
	}

	now = now.Add(poolEdgeResolveEvery)
	if got = edge.edge(); got.Address != "192.0.2.8" || lookups != 2 {
		t.Fatalf("address = %q after %d lookups, want the one looked up once the interval passed", got.Address, lookups)
	}
	// A name that rotates among addresses keeps the one held while it still
	// answers it.
	now = now.Add(poolEdgeResolveEvery)
	answer = []netip.Addr{netip.MustParseAddr("192.0.2.9"), netip.MustParseAddr("192.0.2.8")}
	if got = edge.edge(); got.Address != "192.0.2.8" || lookups != 3 {
		t.Fatalf("address = %q after %d lookups, want the one held while the host still answers it", got.Address, lookups)
	}
	now = now.Add(poolEdgeResolveEvery)
	lookupErr = errors.New("no such host")
	if got = edge.edge(); got.Address != "192.0.2.8" || lookups != 4 {
		t.Fatalf("address = %q after %d lookups, want the last one kept over a failed lookup", got.Address, lookups)
	}
}

// A credential the server has not placed, or has let expire, is left out,
// and the address is still given: the rest of each sandbox's document is
// delivered, and the credential follows once the server places one.
func TestPoolEdgeLeavesOutACredentialItCannotHand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sandbox-edge-credential.json")
	edge := newPoolEdge("7302-pool.edge.example", path)
	edge.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.7")}, nil
	}
	addressOnly := sandboxconfig.RuntimeEdge{Address: "192.0.2.7"}
	if got := edge.edge(); *got != addressOnly {
		t.Fatalf("edge with no credential placed = %+v, want the address alone", got)
	}
	writeCredential(t, path, machine.Credential{Header: "X-Archil-Token", Token: "old", Expires: time.Now().Add(-time.Minute)})
	if got := edge.edge(); *got != addressOnly {
		t.Fatalf("edge with an expired credential = %+v, want the address alone", got)
	}
	if err := os.WriteFile(path, []byte(`{"header":"X-Archil-Token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := edge.edge(); *got != addressOnly {
		t.Fatalf("edge with a credential holding no token = %+v, want the address alone", got)
	}
	writeCredential(t, path, machine.Credential{Header: "X-Archil-Token", Token: "placed", Expires: time.Now().Add(time.Hour)})
	if got := edge.edge(); got.Token != "placed" {
		t.Fatalf("edge once the credential is placed = %+v, want it", got)
	}
	// The server replaces the file in place: a read that catches it missing
	// or half written hands out the last one read whole while it lasts.
	if err := os.WriteFile(path, []byte(`{"header":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := edge.edge(); got.Token != "placed" {
		t.Fatalf("edge while the file is half written = %+v, want the last credential read", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := edge.edge(); got.Token != "placed" {
		t.Fatalf("edge while the file is missing = %+v, want the last credential read", got)
	}
	now := time.Now().Add(2 * time.Hour)
	edge.now = func() time.Time { return now }
	if got := edge.edge(); got.Token != "" {
		t.Fatalf("edge once the last credential read has expired = %+v, want none", got)
	}

	// A driver whose URLs answer without one is given the address alone.
	bare := newPoolEdge("7302-pool.edge.example", "")
	bare.lookup = edge.lookup
	if got := bare.edge(); *got != addressOnly {
		t.Fatalf("edge with no credential to hand = %+v, want the address alone", got)
	}
}

// A lookup the resolver is slow to answer holds up only the caller that made
// it: every other is answered at once with the address held. With none held
// yet, as just after the agent starts, they wait for it rather than hand a
// sandbox no address.
func TestPoolEdgeLooksUpOutsideItsLock(t *testing.T) {
	edge := newPoolEdge("7302-pool.edge.example", "")
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	edge.now = func() time.Time { return now }
	answer := "192.0.2.7"
	release, started := make(chan struct{}), make(chan struct{}, 1)
	edge.lookup = func(context.Context, string) ([]netip.Addr, error) {
		started <- struct{}{}
		<-release
		return []netip.Addr{netip.MustParseAddr(answer)}, nil
	}

	// None held: a caller during the first lookup waits for its answer.
	first, waiter := make(chan string), make(chan string)
	go func() { first <- edge.edge().Address }()
	<-started
	go func() { waiter <- edge.edge().Address }()
	select {
	case got := <-waiter:
		t.Fatalf("with no address held a caller got %q before the first lookup ended", got)
	case <-time.After(100 * time.Millisecond):
	}
	release <- struct{}{}
	if got, waited := <-first, <-waiter; got != "192.0.2.7" || waited != "192.0.2.7" {
		t.Fatalf("first lookup = %q, waiter = %q, want both the address", got, waited)
	}

	// One held: a caller during a later lookup is answered at once.
	now = now.Add(poolEdgeResolveEvery)
	answer = "192.0.2.8"
	again := make(chan string)
	go func() { again <- edge.edge().Address }()
	<-started
	answered := make(chan string)
	go func() { answered <- edge.edge().Address }()
	select {
	case got := <-answered:
		if got != "192.0.2.7" {
			t.Fatalf("a caller during the lookup got %q, want the address held", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a caller waited on another's lookup with an address held")
	}
	release <- struct{}{}
	if got := <-again; got != "192.0.2.8" {
		t.Fatalf("the caller that looked up got %q", got)
	}
}

// The edge form names the host the pool resolves for its sandboxes: its pool
// URL's.
func TestPoolHopEdgeNamesThePoolsHost(t *testing.T) {
	hop, err := resolvePoolHop(Bootstrap{Runtime: RuntimeMachine, IngressURL: "https://7302-pool.edge.example", PoolURL: "https://7302-pool.edge.example"})
	if err != nil {
		t.Fatal(err)
	}
	if hop.form != hopEdge || hop.edgeHost != "7302-pool.edge.example" {
		t.Fatalf("hop = %s at %q, want the edge form at the pool's host", hop.form, hop.edgeHost)
	}
}
