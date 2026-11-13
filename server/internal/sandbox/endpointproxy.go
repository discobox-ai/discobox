package sandbox

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/proxy"
)

// EnvironmentProxy is the proxy the server's own environment names for an
// https connection to addr: HTTPS_PROXY under NO_PROXY, as net/http reads
// them, or else a SOCKS5 ALL_PROXY, which boxd's driver honored itself. A
// driver given a dialer reads neither (disco-vm's machine.Config.Dial), so
// the checked dial proxies as the driver would have (ADR 26-11-11-080 §3).
func EnvironmentProxy() func(addr string) (*url.URL, error) {
	https := httpproxy.FromEnvironment().ProxyFunc()
	return func(addr string) (*url.URL, error) {
		if u, err := https(&url.URL{Scheme: "https", Host: addr}); err != nil || u != nil {
			return u, err
		}
		all := strings.TrimSpace(os.Getenv("ALL_PROXY"))
		if all == "" {
			all = strings.TrimSpace(os.Getenv("all_proxy"))
		}
		if all == "" {
			return nil, nil
		}
		u, err := url.Parse(all)
		if err != nil {
			return nil, fmt.Errorf("ALL_PROXY: %w", err)
		}
		if u.Scheme != "socks5" && u.Scheme != "socks5h" {
			return nil, nil
		}
		return u, nil
	}
}

// dialProxy opens a connection to addr through proxyURL: an HTTP CONNECT
// through an http or https proxy, or SOCKS5. The proxy itself is the
// operator's, and is dialed with next as it is, unchecked: it may well be on
// loopback.
func dialProxy(ctx context.Context, proxyURL *url.URL, addr string, next func(ctx context.Context, network, addr string) (net.Conn, error)) (net.Conn, error) {
	switch proxyURL.Scheme {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if proxyURL.User != nil {
			password, _ := proxyURL.User.Password()
			auth = &proxy.Auth{User: proxyURL.User.Username(), Password: password}
		}
		dialer, err := proxy.SOCKS5("tcp", proxyURL.Host, auth, dialerFunc(next))
		if err != nil {
			return nil, err
		}
		contextDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("proxy %s: the SOCKS5 dialer takes no context", proxyURL.Redacted())
		}
		return contextDialer.DialContext(ctx, "tcp", addr)
	case "http", "https":
	default:
		return nil, fmt.Errorf("proxy %s: scheme %q", proxyURL.Redacted(), proxyURL.Scheme)
	}
	proxyAddr := proxyURL.Host
	if proxyURL.Port() == "" {
		proxyAddr = net.JoinHostPort(proxyURL.Hostname(), map[string]string{"http": "80", "https": "443"}[proxyURL.Scheme])
	}
	conn, err := next(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	if proxyURL.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: proxyURL.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = tlsConn
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
		defer func() { _ = conn.SetDeadline(time.Time{}) }()
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: http.Header{}}
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username()+":"+password)))
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req) //nolint:bodyclose // a CONNECT's answer has no body to close; the connection is the tunnel
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy %s refused CONNECT %s: %s", proxyURL.Redacted(), addr, resp.Status)
	}
	if reader.Buffered() > 0 {
		return &bufferedConn{Conn: conn, reader: reader}, nil
	}
	return conn, nil
}

// bufferedConn is a tunnel whose first bytes were read past the CONNECT's
// answer.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }

// dialerFunc is a dial function as a proxy.Dialer.
type dialerFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func (f dialerFunc) Dial(network, addr string) (net.Conn, error) {
	return f(context.Background(), network, addr)
}

func (f dialerFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f(ctx, network, addr)
}
