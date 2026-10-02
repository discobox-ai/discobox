// Package internalhttp provides the transport for the pool agent's own HTTP:
// the control-plane client's, and the base every sandbox transport builds on.
// A sandbox is reached through the dial its runtime supplies
// (sandboxruntime.Dialer.Transport), never through Client.
//
// It deliberately does NOT honor HTTP_PROXY. When a pool runs inside a
// Discobox sandbox, that sandbox injects proxy environment variables into the
// pool container so the pool's *egress* can cross the surrounding MITM proxy.
// http.DefaultClient picks those up for every request, including the pool
// agent's calls to sandboxes on its own private Docker network — which the
// egress proxy has no business carrying and rejects, surfacing as
// "sandbox-agent health returned 500 Internal Server Error" during sandbox
// creation.
//
// Agent-to-sandbox traffic never leaves the pool's own network, so the correct
// answer is not a NO_PROXY entry (whose value would have to track a subnet the
// pool does not choose) but a client that never proxies at all.
package internalhttp

import (
	"net"
	"net/http"
	"time"
)

// Client is the shared client for pool-internal HTTP. Its transport mirrors
// http.DefaultTransport except that Proxy is nil.
var Client = &http.Client{Transport: Transport()}

// Transport returns a new transport for pool-internal HTTP. Callers that need
// their own timeouts or instrumentation build on this rather than on
// http.DefaultTransport, which would reintroduce proxy handling.
func Transport() *http.Transport {
	return &http.Transport{
		Proxy: nil, // never proxy: this traffic stays on the pool's own network
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}
