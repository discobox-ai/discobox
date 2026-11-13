package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
)

// EndpointPolicy is where a provider's Privileged endpoints may point: the
// hosts its secret fields are sent to, which the server dials from its own
// network (ADR 26-11-11-080 §3). Any project member may write a provider, so
// an endpoint is public and reached over TLS, unless the server's operator
// allows it, or it was carried from the server's own environment into the
// row (§8).
type EndpointPolicy struct {
	// Allowed are the endpoints the operator lets providers name, private
	// ones included: each a HOST, which allows every port, or a HOST:PORT.
	Allowed []string
	// Resolver resolves a name to check what it reaches; nil is
	// net.DefaultResolver.
	Resolver *net.Resolver
	// Proxy is the proxy a dial to addr goes through, or nil for none:
	// EnvironmentProxy, the server's own proxy settings, which a driver given
	// this dial no longer reads itself. Nil Proxy dials everything directly.
	Proxy func(addr string) (*url.URL, error)
}

// ErrEndpointRefused is what a refused endpoint, or a refused dial to one,
// wraps.
var ErrEndpointRefused = errors.New("endpoint refused")

// CheckEndpoint refuses an endpoint value a write sets: a URL that is not
// https, or a HOST:PORT (which the driver reaches over TLS whatever it says),
// whose host is, or resolves to, an address that is not public, unless the
// operator allows it or carried is set.
func (p EndpointPolicy) CheckEndpoint(ctx context.Context, value string, carried bool) error {
	host, port, err := endpointHostPort(value)
	if err != nil {
		return fmt.Errorf("%w: %q: %w", ErrEndpointRefused, value, err)
	}
	if carried || p.allows(host, port) {
		return nil
	}
	addrs, err := p.resolve(ctx, host)
	if err != nil {
		return fmt.Errorf("%w: %q: %w", ErrEndpointRefused, value, err)
	}
	for _, addr := range addrs {
		if !PublicAddr(addr) {
			return fmt.Errorf("%w: %q reaches %s, which is not a public address; the server's operator may allow it (providerEndpoints)", ErrEndpointRefused, value, addr)
		}
	}
	return nil
}

// Dial is next, checked: a connection to a HOST:PORT that carried names, or
// the operator allows, is dialed as it is; any other is resolved, refused if
// any address it reaches is not public, and dialed at the address checked, so
// a name that resolves elsewhere between the check and the dial reaches
// nothing. Where Proxy names a proxy for it, the connection goes through the
// proxy by name, which the proxy resolves: what an operator's proxy reaches
// is its policy, not the server's. Only a literal non-public address is
// refused there.
func (p EndpointPolicy) Dial(carried []string, next func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if next == nil {
		var d net.Dialer
		next = d.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("%w: dial %q: %w", ErrEndpointRefused, addr, err)
		}
		allowed := p.allows(host, port) || slices.ContainsFunc(carried, func(value string) bool {
			h, pt, ok := carriedHostPort(value)
			return ok && strings.EqualFold(h, host) && pt == port
		})
		if allowed {
			return p.dialVia(ctx, network, addr, next)
		}
		if p.Proxy != nil {
			proxyURL, err := p.Proxy(addr)
			if err != nil {
				return nil, err
			}
			if proxyURL != nil {
				// The proxy resolves the name, and what it lets the server
				// reach is the operator's policy to enforce, not this one's:
				// the name goes to it as named. Only an address given
				// literally, which no one resolves, is checked here.
				if literal, err := netip.ParseAddr(host); err == nil && !PublicAddr(literal) {
					return nil, fmt.Errorf("%w: dial %q, which is not a public address", ErrEndpointRefused, addr)
				}
				return dialProxy(ctx, proxyURL, addr, next)
			}
		}
		addrs, err := p.resolve(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("%w: dial %q: %w", ErrEndpointRefused, addr, err)
		}
		for _, a := range addrs {
			if !PublicAddr(a) {
				return nil, fmt.Errorf("%w: dial %q reaches %s, which is not a public address", ErrEndpointRefused, addr, a)
			}
		}
		// Any other is dialed at the address checked, so a name that
		// resolves elsewhere between the check and the dial reaches nothing.
		var errs []error
		for _, a := range addrs {
			conn, err := next(ctx, network, net.JoinHostPort(a.String(), port))
			if err == nil {
				return conn, nil
			}
			errs = append(errs, err)
		}
		return nil, errors.Join(errs...)
	}
}

// dialVia dials addr as named: through the proxy Proxy names for it, or
// straight.
func (p EndpointPolicy) dialVia(ctx context.Context, network, addr string, next func(ctx context.Context, network, addr string) (net.Conn, error)) (net.Conn, error) {
	if p.Proxy != nil {
		proxyURL, err := p.Proxy(addr)
		if err != nil {
			return nil, err
		}
		if proxyURL != nil {
			return dialProxy(ctx, proxyURL, addr, next)
		}
	}
	return next(ctx, network, addr)
}

// allows reports whether the operator's list names host, or host:port.
func (p EndpointPolicy) allows(host, port string) bool {
	for _, entry := range p.Allowed {
		entry = strings.TrimSpace(entry)
		if h, pt, err := net.SplitHostPort(entry); err == nil {
			if strings.EqualFold(h, host) && pt == port {
				return true
			}
			continue
		}
		if strings.EqualFold(strings.Trim(entry, "[]"), host) {
			return true
		}
	}
	return false
}

func (p EndpointPolicy) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr.Unmap()}, nil
	}
	resolver := p.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s resolves to no address", host)
	}
	for i := range addrs {
		addrs[i] = addrs[i].Unmap()
	}
	return addrs, nil
}

// cgnat is shared address space (RFC 6598), private to a carrier's network.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// PublicAddr reports whether addr is a public unicast address: not loopback,
// private, link-local, shared (CGNAT), unspecified or multicast.
func PublicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	return addr.IsValid() && addr.IsGlobalUnicast() && !addr.IsPrivate() && !cgnat.Contains(addr)
}

// carriedHostPort is a carried endpoint's host and port, whatever its
// scheme: a carried value is the operator's, which the old driver took over
// http to a loopback address too.
func carriedHostPort(value string) (host, port string, ok bool) {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "://") {
		host, port, err := net.SplitHostPort(value)
		return host, port, err == nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" {
		return "", "", false
	}
	port = u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	return u.Hostname(), port, port != ""
}

// endpointHostPort is an endpoint's host and port: an https URL's, at 443
// when it names none, or a HOST:PORT's. Any other scheme is refused.
func endpointHostPort(value string) (host, port string, err error) {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "://") {
		host, port, err = net.SplitHostPort(value)
		if err != nil || host == "" {
			return "", "", errors.New("not an https URL or HOST:PORT")
		}
		return host, port, nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" {
		return "", "", errors.New("not a URL")
	}
	if u.Scheme != "https" {
		return "", "", fmt.Errorf("scheme %q: an endpoint is https", u.Scheme)
	}
	port = u.Port()
	if port == "" {
		port = "443"
	}
	return u.Hostname(), port, nil
}
