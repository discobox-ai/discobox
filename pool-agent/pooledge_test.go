package poolagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
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

	got, err := edge.edge()
	if err != nil {
		t.Fatal(err)
	}
	want := sandboxconfig.RuntimeEdge{Address: "192.0.2.7", Header: "X-Archil-Token", Token: "first", Expires: expires}
	if *got != want {
		t.Fatalf("edge = %+v, want %+v", *got, want)
	}

	// Renewed by the server: taken at once. The address is not looked up again
	// within the interval.
	writeCredential(t, path, machine.Credential{Header: "X-Archil-Token", Token: "second", Expires: expires.Add(time.Hour)})
	answer = []netip.Addr{netip.MustParseAddr("192.0.2.8")}
	if got, err = edge.edge(); err != nil || got.Token != "second" || got.Address != "192.0.2.7" || lookups != 1 {
		t.Fatalf("edge = %+v, %v after %d lookups, want the renewed credential and the address held", got, err, lookups)
	}

	now = now.Add(poolEdgeResolveEvery)
	if got, _ = edge.edge(); got.Address != "192.0.2.8" || lookups != 2 {
		t.Fatalf("address = %q after %d lookups, want the one looked up once the interval passed", got.Address, lookups)
	}
	// A name that rotates among addresses keeps the one held while it still
	// answers it.
	now = now.Add(poolEdgeResolveEvery)
	answer = []netip.Addr{netip.MustParseAddr("192.0.2.9"), netip.MustParseAddr("192.0.2.8")}
	if got, _ = edge.edge(); got.Address != "192.0.2.8" || lookups != 3 {
		t.Fatalf("address = %q after %d lookups, want the one held while the host still answers it", got.Address, lookups)
	}
	now = now.Add(poolEdgeResolveEvery)
	lookupErr = errors.New("no such host")
	if got, _ = edge.edge(); got.Address != "192.0.2.8" || lookups != 4 {
		t.Fatalf("address = %q after %d lookups, want the last one kept over a failed lookup", got.Address, lookups)
	}
}

// A credential the server has not placed, or has let expire, holds the
// document back, naming why: a sandbox could not reach its pool with it.
func TestPoolEdgeRefusesACredentialItCannotHand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sandbox-edge-credential.json")
	edge := newPoolEdge("7302-pool.edge.example", path)
	edge.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.7")}, nil
	}
	if _, err := edge.edge(); err == nil || !strings.Contains(err.Error(), "is not there") {
		t.Fatalf("edge with no credential placed = %v, want it named", err)
	}
	writeCredential(t, path, machine.Credential{Header: "X-Archil-Token", Token: "old", Expires: time.Now().Add(-time.Minute)})
	if _, err := edge.edge(); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("edge with an expired credential = %v, want it named", err)
	}
	if err := os.WriteFile(path, []byte(`{"header":"X-Archil-Token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := edge.edge(); err == nil || strings.Contains(err.Error(), "X-Archil-Token") {
		t.Fatalf("edge with a credential holding no token = %v, want it refused, quoting nothing of the file", err)
	}

	// A driver whose URLs answer without one is given the address alone.
	bare := newPoolEdge("7302-pool.edge.example", "")
	bare.lookup = edge.lookup
	if got, err := bare.edge(); err != nil || *got != (sandboxconfig.RuntimeEdge{Address: "192.0.2.7"}) {
		t.Fatalf("edge with no credential to hand = %+v, %v, want the address alone", got, err)
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
