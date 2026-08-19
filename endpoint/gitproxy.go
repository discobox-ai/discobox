package endpoint

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// LoopbackProxy exposes a local endpoint over an ordinary HTTP address on the
// loopback interface.
//
// A unix socket or named pipe is only reachable by a process that can dial it
// directly, which excludes anything that speaks URLs and nothing else — git in
// particular, which the CLI shells out to for source push and apply. The proxy
// gives those tools an http:// URL that lands on the same server the API
// client is already talking to, so a local endpoint is not a second-class
// server that only part of the CLI can reach.
//
// It binds the loopback interface only and lives for the duration of the
// command that started it.
type LoopbackProxy struct {
	baseURL  string
	listener net.Listener
	server   *http.Server
}

// StartLoopbackProxy serves endpoint over a freshly bound loopback address.
// The caller closes the proxy when the commands that need the URL are done.
func StartLoopbackProxy(ctx context.Context, endpoint string) (*LoopbackProxy, error) {
	_, client, err := HTTPClient(endpoint, nil)
	if err != nil {
		return nil, err
	}
	target, err := url.Parse(LogicalHTTPBaseURL)
	if err != nil {
		return nil, err
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for local %s proxy: %w", endpoint, err)
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = target.Host
		},
		Transport: client.Transport,
		// Git's smart HTTP transport streams both directions of a fetch and a
		// push, so responses must not be buffered waiting for more output.
		FlushInterval: -1,
	}
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		_ = server.Serve(listener)
	}()
	return &LoopbackProxy{
		baseURL:  "http://" + listener.Addr().String(),
		listener: listener,
		server:   server,
	}, nil
}

// BaseURL is the http:// address the proxy serves the endpoint on.
func (p *LoopbackProxy) BaseURL() string {
	if p == nil {
		return ""
	}
	return p.baseURL
}

// Close stops serving and releases the loopback address.
func (p *LoopbackProxy) Close() error {
	if p == nil {
		return nil
	}
	return p.server.Close()
}
