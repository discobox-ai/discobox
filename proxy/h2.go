package proxy

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/elazarl/goproxy"
)

// enableHTTP2 makes both MITM legs speak HTTP/2 where the peer offers it,
// which is what gRPC needs. Interception is unchanged: the sandbox's leg
// negotiates h2 over ALPN with the MITM certificate, and every stream on it
// goes through the same request and response handlers an HTTP/1.1 request
// does, one stream to one request. The origin's leg is the proxy's own
// transport, offering h2 to the origin; it has a TLS configuration of its
// own, so without ForceAttemptHTTP2 it would quietly stay on HTTP/1.1.
func enableHTTP2(p *goproxy.ProxyHttpServer) {
	p.AllowHTTP2 = true
	if p.Tr == nil {
		p.Tr = &http.Transport{}
	}
	// goproxy's default transport shares one package-level TLS configuration
	// among every proxy, and configuring h2 appends to its NextProtos in
	// place; this proxy's transport gets its own.
	p.Tr.TLSClientConfig = p.Tr.TLSClientConfig.Clone()
	p.Tr.ForceAttemptHTTP2 = true
}

// errCleartextHTTP2ViaUpstream is an h2c request that would have to go
// through the upstream proxy. A request for an http:// URL is sent to an
// HTTP proxy as itself, not through a CONNECT, so prior-knowledge HTTP/2
// would be spoken to the proxy rather than the origin.
var errCleartextHTTP2ViaUpstream = errors.New("cleartext HTTP/2 (h2c) cannot be sent through the upstream proxy")

// cleartextHTTP2Transport is the transport for a request the sandbox sent as
// h2c, insecure gRPC's protocol, to an http:// origin: HTTP/2 with prior
// knowledge, since an origin that speaks only h2c answers HTTP/1.1 with
// nothing a client can use. It is base in every other respect, so it dials
// the way the proxy's own transport does.
func cleartextHTTP2Transport(base *http.Transport) *http.Transport {
	h2c := base.Clone()
	h2c.Protocols = new(http.Protocols)
	h2c.Protocols.SetUnencryptedHTTP2(true)
	if upstream := base.Proxy; upstream != nil {
		h2c.Proxy = func(req *http.Request) (*url.URL, error) {
			via, err := upstream(req)
			if err != nil {
				return nil, err
			}
			if via != nil {
				return nil, errCleartextHTTP2ViaUpstream
			}
			return nil, nil
		}
	}
	return h2c
}

// cleartextHTTP2 reports whether req arrived as h2c and leaves for an http://
// origin, so it has to be sent as h2c too. One the tunnel upgraded to https
// goes out on the ordinary transport, which negotiates h2 over TLS.
func cleartextHTTP2(req *http.Request) bool {
	return req.ProtoMajor == 2 && req.URL != nil && req.URL.Scheme == "http"
}
