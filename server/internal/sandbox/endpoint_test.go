package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
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

// With a proxy, a dial goes through it with a CONNECT to the target as named:
// the proxy resolves it, and what it reaches is the operator's policy. Only a
// literal non-public address is refused before it. The proxy itself is not
// checked at all: it is the operator's, on loopback here.
func TestEndpointDialThroughAProxy(t *testing.T) {
	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connects := make(chan string, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				connects <- req.Method + " " + req.Host
				_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\ntunnel")
			}()
		}
	}()
	proxyURL, _ := url.Parse("http://" + listener.Addr().String())
	policy := EndpointPolicy{Allowed: []string{"boxd.internal"}, Proxy: func(string) (*url.URL, error) { return proxyURL, nil }}
	dial := policy.Dial(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", "203.0.113.9:443")
	if err != nil {
		t.Fatalf("a public target through the proxy: %v", err)
	}
	first := make([]byte, 6)
	if _, err := io.ReadFull(conn, first); err != nil || string(first) != "tunnel" {
		t.Fatalf("read through the tunnel = %q, %v", first, err)
	}
	_ = conn.Close()
	if got := <-connects; got != "CONNECT 203.0.113.9:443" {
		t.Fatalf("the proxy was asked %q", got)
	}
	if _, err := dial(ctx, "tcp", "10.1.2.3:443"); !errors.Is(err, ErrEndpointRefused) {
		t.Fatalf("a private target through the proxy = %v, want refused", err)
	}
	conn, err = dial(ctx, "tcp", "no-such-host.invalid:443")
	if err != nil {
		t.Fatalf("a name only the proxy resolves: %v", err)
	}
	_ = conn.Close()
	if got := <-connects; got != "CONNECT no-such-host.invalid:443" {
		t.Fatalf("the proxy was asked %q, want the name", got)
	}
	conn, err = dial(ctx, "tcp", "boxd.internal:443")
	if err != nil {
		t.Fatalf("an allowed endpoint through the proxy: %v", err)
	}
	_ = conn.Close()
	if got := <-connects; got != "CONNECT boxd.internal:443" {
		t.Fatalf("the proxy was asked %q for an allowed endpoint, want it by name", got)
	}
}
