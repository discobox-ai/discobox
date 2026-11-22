package discovm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// archilControlPlane is the host archil sends its key to in aws-us-east-1.
const archilControlPlane = "control.green.us-east-1.aws.prod.archil.com"

// asArchil makes the fake pool's driver one that neither authenticates as its
// machine nor issues keys, and sends its key where archil does: its pools
// hold a sentinel for the provider's key (ADR 26-11-11-080 §9).
func asArchil(t *testing.T, r *Runtime, driver *poolTestDriver, providerKey string) *poolMachine {
	t.Helper()
	host := r.host.(*poolMachine)
	driver.name, driver.keysNone, driver.noPorts = "archil", true, true
	host.driverConfig = json.RawMessage(`{"region":"aws-us-east-1"}`)
	sends, err := proxyagent.DriverKeySends("archil", host.driverConfig)
	if err != nil {
		t.Fatal(err)
	}
	host.keys.sends, host.keys.providerKey = sends, providerKey
	return host
}

// A pool on a driver that neither authenticates a machine as itself nor
// issues keys, and declares where it sends its key (archil), is made: its
// machine's key file holds a sentinel shaped as the driver's key, never the
// provider's key; nothing is issued; its record and its host credential say
// sentinel; its bootstrap tells the agent so; and the same sentinel is kept
// at every reconcile. The pool's proxy is answered for that sentinel alone,
// at a host the driver sends its key to, with the provider's key as the
// driver would send it there.
func TestFakePoolMachineHoldsASentinelForTheDriversKey(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-sentinel", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	asArchil(t, r, driver, "key-THEPROVIDERSOWN")

	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatalf("EnsurePool() on an archil pool = %v, want it made", err)
	}
	if len(driver.created) != 1 {
		t.Fatalf("made %d machines, want the pool's", len(driver.created))
	}
	key := guestKey(t, r, pool)
	record := recordOf(t, pool).Key
	if key.Driver != "archil" || !strings.HasPrefix(key.Secret, "key-") || len(key.Secret) < 20 || strings.Contains(key.Secret, "THEPROVIDERSOWN") {
		t.Fatalf("the machine's key file = %+v, want a sentinel shaped as an archil key", key)
	}
	if record == nil || record.Sentinel != key.Secret || record.Issued || record.InMachine {
		t.Fatalf("record %+v, want the sentinel placed", record)
	}
	if credential := pool.HostCredential; credential == nil || credential.Key != model.PoolHostKeySentinel {
		t.Fatalf("host credential = %+v, want sentinel", credential)
	}
	if bootstrap := driver.applied[len(driver.applied)-1]; !bootstrap.KeySentinel {
		t.Fatalf("bootstrap = %+v, want it to say the key file holds a sentinel", bootstrap)
	}
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatal(err)
	}
	if again := guestKey(t, r, pool); again.Secret != key.Secret || recordOf(t, pool).Key.Sentinel != key.Secret {
		t.Fatalf("the sentinel moved from %q to %q at a reconcile", key.Secret, again.Secret)
	}
	// A record lost takes the sentinel the machine holds, which its running
	// agent registered with its proxy, rather than placing another.
	host := r.host.(*poolMachine)
	if record, err := host.keys.ensure(ctx, pool, poolGuest(t, r, pool), nil); err != nil || record == nil || record.Sentinel != key.Secret {
		t.Fatalf("ensure() with the record lost = %+v, %v; want the sentinel the machine holds", record, err)
	}
	if again := guestKey(t, r, pool); again.Secret != key.Secret {
		t.Fatalf("a record lost placed %q over the sentinel %q", again.Secret, key.Secret)
	}
	// A key this server did not place is no sentinel, whatever its shape: a
	// real one is replaced, never kept on the disk as one.
	for _, file := range []string{
		`{"driver":"archil","secret":"` + key.Secret + `"}`,
		`{"driver":"archil","name":"discobox-sentinel","secret":"key-AREALKEYOFANOTHERSHAPE0123456789"}`,
	} {
		if err := poolruntime.PlaceBoxFiles(ctx, poolGuest(t, r, pool), poolruntime.BoxFile{Path: bundle.ProvisioningKeyFile, Data: []byte(file)}); err != nil {
			t.Fatal(err)
		}
		record, err := host.keys.ensure(ctx, pool, poolGuest(t, r, pool), nil)
		if err != nil || record == nil || record.Sentinel == "" || strings.Contains(file, record.Sentinel) {
			t.Fatalf("ensure() over %s = %+v, %v; want a new sentinel placed", file, record, err)
		}
		if placed := guestKey(t, r, pool); placed.Secret != record.Sentinel || placed.Name != proxyagent.EngineSentinelKeyName {
			t.Fatalf("the key file holds %+v, want the new sentinel, named as one", placed)
		}
	}

	// Through the provider the server's factory wraps the runtime in.
	provider := poolruntime.New(r, Definition("archil"), nil, nil)
	value, err := provider.ResolveDriverKey(ctx, nil, pool, key.Secret, archilControlPlane)
	if err != nil || value != "key-THEPROVIDERSOWN" {
		t.Fatalf("ResolveDriverKey() = %q, %v; want the provider's key", value, err)
	}
	for _, tc := range []struct{ name, sentinel, host string }{
		{"another sentinel", "key-" + strings.Repeat("x", 32), archilControlPlane},
		{"another host", key.Secret, "api.example.com"},
		{"no sentinel", "", archilControlPlane},
	} {
		if value, err := provider.ResolveDriverKey(ctx, nil, pool, tc.sentinel, tc.host); !errors.Is(err, sandbox.ErrDriverKeyRefused) || value != "" {
			t.Errorf("%s: ResolveDriverKey() = %q, %v; want it refused", tc.name, value, err)
		}
	}
}

// A provider key an older server placed on such a pool's machine is replaced
// by the sentinel: the file holds the sentinel, and the record says so.
func TestFakePoolMachineSentinelReplacesTheProvidersKeyAnOlderServerPlaced(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-sentinel-old", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	host := asArchil(t, r, driver, "key-THEPROVIDERSOWN")
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatal(err)
	}
	guest := poolGuest(t, r, pool)
	if err := poolruntime.PlaceBoxFiles(ctx, guest, poolruntime.BoxFile{Path: bundle.ProvisioningKeyFile, Data: []byte(`{"driver":"archil","secret":"key-THEPROVIDERSOWN"}`)}); err != nil {
		t.Fatal(err)
	}
	record, err := host.keys.ensure(ctx, pool, guest, &poolKeyRecord{Fingerprint: "an-old-provider-key"})
	if err != nil || record == nil || record.Sentinel == "" {
		t.Fatalf("ensure() over an older server's key = %+v, %v; want a sentinel", record, err)
	}
	if key := guestKey(t, r, pool); key.Secret != record.Sentinel {
		t.Fatalf("the key file holds %q, want the sentinel %q in place of the provider's key", key.Secret, record.Sentinel)
	}
}

// What the proxy puts in a sentinel's place is the provider's key as the
// driver would send it there: a key typed without archil's "key-" prefix
// still reaches Archil as "Bearer key-…", as the driver would send it, and a
// JSON field's is the key itself.
func TestDriverKeyInPlaceOfASentinel(t *testing.T) {
	header := machine.SecretSend{Header: "Authorization", ValuePrefix: "Bearer key-", TrimPrefix: "key-"}
	for _, tc := range []struct {
		send          machine.SecretSend
		sentinel, key string
		want          string
	}{
		{header, "key-SENTINEL", "key-REAL", "key-REAL"},
		{header, "key-SENTINEL", "REAL", "key-REAL"},
		{header, "SENTINEL", "key-REAL", "REAL"},
		{machine.SecretSend{Header: "X-Api-Key"}, "SENTINEL", "REAL", "REAL"},
		{machine.SecretSend{JSONField: "api_key"}, "SENTINEL", "REAL", "REAL"},
	} {
		got, err := inPlaceOf(tc.send, tc.sentinel, tc.key)
		if err != nil || got != tc.want {
			t.Errorf("inPlaceOf(%+v, %q, %q) = %q, %v; want %q", tc.send, tc.sentinel, tc.key, got, err, tc.want)
			continue
		}
		// The request the proxy sends is the one the driver would send.
		sent := strings.Replace(sentAs(tc.send, tc.sentinel), tc.sentinel, got, 1)
		if want := sentAs(tc.send, tc.key); sent != want {
			t.Errorf("%+v: the proxy sends %q, the driver would send %q", tc.send, sent, want)
		}
	}
}
