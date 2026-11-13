package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestConfigPaths(t *testing.T) {
	config := map[string]json.RawMessage{"top": json.RawMessage(`1`), "driver": json.RawMessage(`{"region":"fra"}`)}
	if v, ok := ConfigValue(config, "driver.region"); !ok || string(v) != `"fra"` {
		t.Fatalf("driver.region = %s, %v", v, ok)
	}
	if _, ok := ConfigValue(config, "top.nested"); ok {
		t.Fatal("a path through a number reached something")
	}
	if err := SetConfigValue(config, "driver.apiKey", json.RawMessage(`"k"`)); err != nil {
		t.Fatal(err)
	}
	if err := SetConfigValue(config, "fresh.key", json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
	if err := SetConfigValue(config, "top.key", json.RawMessage(`true`)); err == nil {
		t.Fatal("a path through a number was set")
	}
	if v, ok := DeleteConfigValue(config, "driver.apiKey"); !ok || string(v) != `"k"` {
		t.Fatalf("delete driver.apiKey = %s, %v", v, ok)
	}
	if string(config["driver"]) != `{"region":"fra"}` || string(config["fresh"]) != `{"key":true}` {
		t.Fatalf("config = %s %s", config["driver"], config["fresh"])
	}
	joined, err := JoinProviderConfig(json.RawMessage(`{"driver":{"region":"fra"}}`), map[string]json.RawMessage{"driver.apiKey": json.RawMessage(`"k"`)})
	if err != nil || string(joined) != `{"driver":{"apiKey":"k","region":"fra"}}` {
		t.Fatalf("JoinProviderConfig = %s, %v", joined, err)
	}
}

// An endpoint is public and over TLS, unless the operator allows it or it is
// carried (ADR 26-11-11-080 §3).
func TestCheckEndpoint(t *testing.T) {
	policy := EndpointPolicy{Allowed: []string{"boxd.internal", "10.0.0.5:9443"}}
	ctx := context.Background()
	for _, tc := range []struct {
		value   string
		carried bool
		want    string
	}{
		{"https://203.0.113.9/token", false, ""},
		{"203.0.113.9:9443", false, ""},
		{"https://boxd.internal/token", false, ""},
		{"10.0.0.5:9443", false, ""},
		{"10.0.0.5:9444", false, "not a public address"},
		{"http://203.0.113.9/token", false, "https"},
		{"https://[::1]:8443/t", false, "not a public address"},
		{"https://169.254.169.254/latest", false, "not a public address"},
		{"https://100.64.1.1/t", false, "not a public address"},
		{"https://127.0.0.1/t", true, ""},
		{"boxd.sh", false, "HOST:PORT"},
	} {
		err := policy.CheckEndpoint(ctx, tc.value, tc.carried)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, ErrEndpointRefused)) {
			t.Errorf("CheckEndpoint(%q, %v) = %v, want %q", tc.value, tc.carried, err, tc.want)
		}
	}
}

// The dial is checked at the address it reaches, and dials that address.
func TestEndpointDial(t *testing.T) {
	var dialed []string
	next := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, errors.New("dialed")
	}
	dial := EndpointPolicy{Allowed: []string{"boxd.internal:9443"}}.Dial([]string{"https://127.0.0.1:8443/token", "http://127.0.0.1:18490/token"}, next)
	ctx := context.Background()
	if _, err := dial(ctx, "tcp", "10.1.2.3:443"); !errors.Is(err, ErrEndpointRefused) {
		t.Fatalf("dial to a private address = %v, want refused", err)
	}
	if _, err := dial(ctx, "tcp", "127.0.0.1:8443"); err == nil || errors.Is(err, ErrEndpointRefused) {
		t.Fatalf("dial to the carried endpoint = %v, want dialed", err)
	}
	if _, err := dial(ctx, "tcp", "127.0.0.1:18490"); err == nil || errors.Is(err, ErrEndpointRefused) {
		t.Fatalf("dial to a carried http endpoint = %v, want dialed", err)
	}
	if _, err := dial(ctx, "tcp", "boxd.internal:9443"); err == nil || errors.Is(err, ErrEndpointRefused) {
		t.Fatalf("dial to an allowed endpoint = %v, want dialed", err)
	}
	if _, err := dial(ctx, "tcp", "203.0.113.9:443"); err == nil || errors.Is(err, ErrEndpointRefused) {
		t.Fatalf("dial to a public address = %v, want dialed", err)
	}
	if strings.Join(dialed, ",") != "127.0.0.1:8443,127.0.0.1:18490,boxd.internal:9443,203.0.113.9:443" {
		t.Fatalf("dialed %v", dialed)
	}
	if PublicAddr(netip.MustParseAddr("::ffff:10.0.0.1")) {
		t.Fatal("a mapped private address is public")
	}
}
