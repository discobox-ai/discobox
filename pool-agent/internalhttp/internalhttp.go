// Package internalhttp provides the transport for the pool agent's own HTTP:
// the control-plane client's, and the base every sandbox transport builds on.
// A sandbox is reached through the dial its runtime supplies
// (sandboxruntime.Dialer.Transport), never through Client.
//
// It deliberately does NOT honor HTTP_PROXY. When a pool runs inside a
// Discobox sandbox, that sandbox injects proxy environment variables into the
// pool container so the pool's *egress* can cross the surrounding MITM proxy.
// http.DefaultClient picks those up for every request, including the pool
// agent's own calls to its sandboxes — which the egress proxy has no business
// carrying and rejects, which once surfaced as "sandbox-agent health returned
// 500 Internal Server Error" during sandbox creation.
//
// Traffic built on this transport goes by the route its caller chose: the
// control plane's URL, or the connection a sandbox's runtime dials — a
// container address, a guest socket, a session the sandbox opened outward.
// For the control-plane client, a proxy would replace that route with its
// own. A sandbox transport ignores the address it is asked to dial, so there
// a proxy would instead turn every request into a proxy request sent to the
// sandbox — absolute-form, and with the egress proxy's credentials, when its
// URL carries any, handed to code the pool does not trust. The answer is
// therefore not a NO_PROXY entry (whose value would have to track routes the
// pool does not choose) but a transport that never proxies.
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
