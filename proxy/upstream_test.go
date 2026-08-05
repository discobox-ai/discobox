package proxy

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"golang.org/x/net/http/httpproxy"
)

func TestUpstreamProxyURL(t *testing.T) {
	t.Run("explicit config wins over environment", func(t *testing.T) {
		t.Setenv("HTTPS_PROXY", "http://from-env:1")
		u, err := upstreamProxyURL(Config{UpstreamProxy: "http://from-config:2"})
		if err != nil {
			t.Fatalf("upstreamProxyURL: %v", err)
		}
		if u == nil || u.Host != "from-config:2" {
			t.Fatalf("got %v, want from-config:2", u)
		}
	})

	t.Run("falls back to the environment a sandbox injects", func(t *testing.T) {
		t.Setenv("HTTPS_PROXY", "http://172.30.0.1:17008")
		u, err := upstreamProxyURL(Config{})
		if err != nil {
			t.Fatalf("upstreamProxyURL: %v", err)
		}
		if u == nil || u.Host != "172.30.0.1:17008" {
			t.Fatalf("got %v", u)
		}
	})

	// A pool with direct egress must keep dialing origins itself.
	t.Run("no upstream means direct", func(t *testing.T) {
		for _, name := range UpstreamProxyEnvVars {
			t.Setenv(name, "")
		}
		u, err := upstreamProxyURL(Config{})
		if err != nil {
			t.Fatalf("upstreamProxyURL: %v", err)
		}
		if u != nil {
			t.Fatalf("expected direct egress, got %v", u)
		}
	})

	// Proxy env vars are commonly written bare.
	t.Run("bare host:port is accepted", func(t *testing.T) {
		u, err := upstreamProxyURL(Config{UpstreamProxy: "172.30.0.1:17008"})
		if err != nil {
			t.Fatalf("upstreamProxyURL: %v", err)
		}
		if u == nil || u.Scheme != "http" || u.Host != "172.30.0.1:17008" {
			t.Fatalf("got %v", u)
		}
	})

	t.Run("garbage is reported, not ignored", func(t *testing.T) {
		if _, err := upstreamProxyURL(Config{UpstreamProxy: "http://a b c/"}); err == nil {
			t.Fatal("expected an error for an unparseable upstream")
		}
	})
}

// Both hooks must be set: Transport.Proxy carries plain HTTP and every MITM'd
// request's re-issue, ConnectDial carries un-inspected CONNECT tunnels. Setting
// one and not the other leaves half of the traffic dialing origins directly,
// which is exactly what fails inside a sandbox.
func TestApplyUpstreamProxySetsBothPaths(t *testing.T) {
	h := newHTTPProxy(nil, nil, nil, nil, nil, nil)
	u, err := upstreamProxyURL(Config{UpstreamProxy: "http://172.30.0.1:17008"})
	if err != nil {
		t.Fatalf("upstreamProxyURL: %v", err)
	}
	applyUpstreamProxy(h, u, "")

	if h.proxy.Tr == nil || h.proxy.Tr.Proxy == nil {
		t.Fatal("Transport.Proxy not set: MITM'd traffic would dial origins directly")
	}
	got, err := h.proxy.Tr.Proxy(&http.Request{URL: mustURL(t, "https://example.com/")})
	if err != nil {
		t.Fatalf("Transport.Proxy: %v", err)
	}
	if got == nil || got.Host != "172.30.0.1:17008" {
		t.Fatalf("Transport.Proxy returned %v", got)
	}
	if h.proxy.ConnectDial == nil {
		t.Fatal("ConnectDial not set: passthrough CONNECT would dial origins directly")
	}
}

// A pool with direct egress must be left exactly as goproxy configured it,
// rather than being pointed at an upstream that does not exist.
func TestApplyUpstreamProxyNilLeavesDirect(t *testing.T) {
	h := newHTTPProxy(nil, nil, nil, nil, nil, nil)
	beforeDial := h.proxy.ConnectDial
	var beforeProxy bool
	if h.proxy.Tr != nil {
		beforeProxy = h.proxy.Tr.Proxy != nil
	}

	applyUpstreamProxy(h, nil, "")

	if fmt.Sprintf("%p", h.proxy.ConnectDial) != fmt.Sprintf("%p", beforeDial) {
		t.Fatal("ConnectDial was changed despite there being no upstream")
	}
	afterProxy := h.proxy.Tr != nil && h.proxy.Tr.Proxy != nil
	if afterProxy != beforeProxy {
		t.Fatal("Transport.Proxy was changed despite there being no upstream")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// Exemptions must be honoured on BOTH paths. Routing unconditionally breaks a
// proxy that reaches its own control plane or loopback services directly -- and
// it is what made the existing proxy tests fail, since they talk to 127.0.0.1
// while the surrounding sandbox sets NO_PROXY=127.0.0.1,localhost.
func TestApplyUpstreamProxyHonoursNoProxy(t *testing.T) {
	h := newHTTPProxy(nil, nil, nil, nil, nil, nil)
	u, err := upstreamProxyURL(Config{UpstreamProxy: "http://172.30.0.1:17008"})
	if err != nil {
		t.Fatalf("upstreamProxyURL: %v", err)
	}
	applyUpstreamProxy(h, u, "127.0.0.1,localhost,::1")

	// Transport path: exempt host goes direct, other host goes via upstream.
	got, err := h.proxy.Tr.Proxy(&http.Request{URL: mustURL(t, "http://127.0.0.1:8080/x")})
	if err != nil || got != nil {
		t.Fatalf("loopback must bypass the upstream, got %v (err %v)", got, err)
	}
	got, err = h.proxy.Tr.Proxy(&http.Request{URL: mustURL(t, "https://example.com/")})
	if err != nil {
		t.Fatalf("Transport.Proxy: %v", err)
	}
	if got == nil || got.Host != "172.30.0.1:17008" {
		t.Fatalf("external host must use the upstream, got %v", got)
	}

	// CONNECT path: same decision, via the same matcher.
	if !exempt(proxyFuncFor(t, u, "127.0.0.1,localhost,::1"), "127.0.0.1:8080") {
		t.Fatal("loopback CONNECT must bypass the upstream")
	}
	if exempt(proxyFuncFor(t, u, "127.0.0.1,localhost,::1"), "example.com:443") {
		t.Fatal("external CONNECT must use the upstream")
	}
}

func proxyFuncFor(t *testing.T, upstream *url.URL, noProxy string) func(*url.URL) (*url.URL, error) {
	t.Helper()
	return (&httpproxy.Config{
		HTTPProxy:  upstream.String(),
		HTTPSProxy: upstream.String(),
		NoProxy:    noProxy,
	}).ProxyFunc()
}
