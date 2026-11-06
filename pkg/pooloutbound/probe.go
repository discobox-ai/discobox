package pooloutbound

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/http/httpproxy"
)

// Probe records an observation from one process context, never inferred from a
// different hop. Detail is a fixed classification and cannot contain URL secrets.
type Probe struct {
	State      string `json:"state"`
	Detail     string `json:"detail,omitempty"`
	HTTPStatus int    `json:"httpStatus,omitempty"`
}

type Probes struct {
	HTTP  Probe `json:"http"`
	HTTPS Probe `json:"https"`
}

// Check performs bounded HTTP and TLS/registry requests with no ambient proxy
// fallback and no direct retry. The registry's unauthenticated 401 is reachable.
func Check(ctx context.Context, r Resolved) Probes {
	probe := func(target string) Probe {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if r.CAPEM != "" {
			roots.AppendCertsFromPEM([]byte(r.CAPEM))
		}
		selectProxy := (&httpproxy.Config{HTTPProxy: r.HTTP, HTTPSProxy: r.HTTPS, NoProxy: r.NoProxy}).ProxyFunc()
		proxyStatus := 0
		tr := &http.Transport{OnProxyConnectResponse: func(_ context.Context, _ *url.URL, _ *http.Request, resp *http.Response) error {
			proxyStatus = resp.StatusCode
			return nil
		}, Proxy: func(req *http.Request) (*url.URL, error) { return selectProxy(req.URL) }, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext, DisableKeepAlives: true}
		defer tr.CloseIdleConnections()
		client := http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		resp, err := client.Do(req)
		if err != nil {
			var unknown x509.UnknownAuthorityError
			detail := "connection_failed"
			if proxyStatus == 407 {
				return Probe{State: "failed", Detail: "proxy_authentication", HTTPStatus: 407}
			}
			if proxyStatus == 403 {
				return Probe{State: "failed", Detail: "denied", HTTPStatus: 403}
			}
			if errors.As(err, &unknown) {
				detail = "untrusted_ca"
			} else if errors.Is(err, context.DeadlineExceeded) {
				detail = "timeout"
			}
			u, _ := url.Parse(target)
			p, _ := selectProxy(u)
			if p == nil && detail == "connection_failed" {
				detail = "direct_path_failed_configure_proxy_if_required"
			}
			return Probe{State: "failed", Detail: detail}
		}
		resp.Body.Close()
		switch resp.StatusCode {
		case 407:
			return Probe{State: "failed", Detail: "proxy_authentication", HTTPStatus: 407}
		case 403:
			return Probe{State: "failed", Detail: "denied", HTTPStatus: 403}
		}
		if resp.StatusCode >= 500 {
			return Probe{State: "failed", Detail: "upstream_failure", HTTPStatus: resp.StatusCode}
		}
		return Probe{State: "reachable", HTTPStatus: resp.StatusCode}
	}
	return Probes{HTTP: probe("http://deb.debian.org/debian/"), HTTPS: probe("https://ghcr.io/v2/")}
}
