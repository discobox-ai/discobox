package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"golang.org/x/net/http/httpproxy"
)

// UpstreamProxyEnvVars are the environment variables an upstream proxy address
// is read from, in precedence order. They are the standard names because the
// process that sets them is not this one: when a pool proxy runs inside a
// Discobox sandbox, the sandbox's own runc wrapper injects exactly these into
// the container, and the proxy is simply another client of them.
var UpstreamProxyEnvVars = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"}

// upstreamProxyURL resolves the proxy this proxy must forward through, or nil
// when it should reach origins directly.
//
// A pool proxy normally has direct egress and needs none of this. It matters
// when a pool runs *inside* a sandbox — the nested case — because a sandbox has
// no route off-box by design: every origin dial and even DNS resolution fails
// there ("lookup ...: server misbehaving"), so the inner proxy has to hand its
// traffic to the outer one rather than resolve anything itself.
func upstreamProxyURL(cfg Config) (*url.URL, error) {
	raw := strings.TrimSpace(cfg.UpstreamProxy)
	if raw == "" {
		for _, name := range UpstreamProxyEnvVars {
			if v := strings.TrimSpace(os.Getenv(name)); v != "" {
				raw = v
				break
			}
		}
	}
	if raw == "" {
		return nil, nil
	}
	// A bare host:port is a proxy address, not a URL; http is the only scheme
	// a forward proxy's own listener speaks here.
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse upstream proxy %q: %w", raw, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("upstream proxy %q names no host", raw)
	}
	return u, nil
}

// upstreamNoProxy returns the exemption list applied to the upstream.
func upstreamNoProxy(cfg Config) string {
	if v := strings.TrimSpace(cfg.UpstreamNoProxy); v != "" {
		return v
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// applyUpstreamProxy routes this proxy's own outbound traffic through upstream.
//
// Both hooks are needed and they cover different paths:
//
//   - Transport.Proxy carries plain HTTP, and the re-issued request of every
//     MITM'd HTTPS connection. That is the path that actually fails in a
//     sandbox, because after terminating TLS the proxy dials the origin itself.
//   - ConnectDial carries CONNECT tunnels that are *not* MITM'd, where the
//     proxy never sees inside and only splices bytes.
//
// Setting only the first leaves passthrough CONNECT broken; setting only the
// second leaves everything inspected broken.
//
// Exemptions are honored on both. Routing unconditionally (http.ProxyURL) is
// wrong even when an upstream exists: a pool proxy reaches its own control
// plane and loopback services directly, and forcing those through an upstream
// breaks them.
func applyUpstreamProxy(h *httpProxy, upstream *url.URL, noProxy string) {
	if upstream == nil {
		return
	}
	// httpproxy implements the same NO_PROXY matching as the standard library
	// (suffix, CIDR, port-specific, "*"), so exemptions behave the way every
	// other tool in a sandbox already expects.
	proxyFunc := (&httpproxy.Config{
		HTTPProxy:  upstream.String(),
		HTTPSProxy: upstream.String(),
		NoProxy:    noProxy,
	}).ProxyFunc()

	if h.proxy.Tr == nil {
		h.proxy.Tr = &http.Transport{}
	}
	h.proxy.Tr.Proxy = func(req *http.Request) (*url.URL, error) {
		return proxyFunc(req.URL)
	}

	viaProxy := h.proxy.NewConnectDialToProxy(upstream.String())
	direct := h.proxy.ConnectDial
	h.proxy.ConnectDial = func(network, addr string) (net.Conn, error) {
		if exempt(proxyFunc, addr) {
			if direct != nil {
				return direct(network, addr)
			}
			// goproxy's ConnectDial carries no context, and the tunnel it
			// dials lives as long as the CONNECT itself, so there is nothing
			// narrower than Background to hand the dialer.
			return (&net.Dialer{}).DialContext(context.Background(), network, addr)
		}
		return viaProxy(network, addr)
	}
}

// exempt reports whether a CONNECT target bypasses the upstream. The address is
// host:port, so it is lifted to a URL for the same matcher the transport uses,
// keeping one definition of "exempt" for both paths.
func exempt(proxyFunc func(*url.URL) (*url.URL, error), addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	target := &url.URL{Scheme: "https", Host: addr}
	if host == addr {
		target.Host = net.JoinHostPort(addr, "443")
	}
	u, err := proxyFunc(target)
	return err == nil && u == nil
}
