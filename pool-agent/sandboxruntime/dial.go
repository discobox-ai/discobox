package sandboxruntime

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"github.com/discobox-ai/discobox/pool-agent/internalhttp"
)

// Dialer opens a connection to one port inside one sandbox. It is what a
// Runtime hands the pool in place of an address, because how a sandbox is
// reached is the backend's own business (ADR 0126 §5): a container address
// here, a guest socket or a session the sandbox opened outward elsewhere.
// Nothing above the Runtime knows which.
//
// A connection says nothing about who is asking: the sandbox agent validates
// its own token on every request whatever carried it, and a failed dial is not
// evidence that the sandbox stopped. Power state comes from the runtime.
type Dialer func(ctx context.Context) (net.Conn, error)

// Transport is an HTTP transport whose every connection is one this dialer
// opened; the request's URL names no address, so build it with HTTPURL.
//
// It keeps no idle connections. A transport is built per use, and an idle
// connection kept by one nobody holds would be a socket leaked until the
// idle timeout; it is also one that might outlive the sandbox it reaches.
// Upgrades are unaffected: net/http leaves a protocol switch's Connection
// header alone.
func (d Dialer) Transport() *http.Transport {
	t := internalhttp.Transport()
	t.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return d(ctx)
	}
	t.ForceAttemptHTTP2 = false
	t.DisableKeepAlives = true
	return t
}

// HTTPURL is the URL a request through a Dialer's transport names. The host
// is never resolved or dialed; it is only what the sandbox's server reads as
// Host, and localhost is what a server inside it expects to be called — a dev
// server that checks Host allows it where it refuses an unknown name.
func HTTPURL(port int, path string) *url.URL {
	return &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort("localhost", strconv.Itoa(port)),
		Path:   path,
	}
}
