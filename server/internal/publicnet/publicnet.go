// Package publicnet keeps the server's requests to a URL somebody else wrote
// on the public internet. A person may name such a URL — an exchange recipe's
// token endpoint, a target document — and the server fetching where they
// cannot reach, then telling them about the answer, is a reach they never had:
// loopback, private and cloud-internal addresses are the control plane's own
// network position.
package publicnet

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// Internal reports whether ip is one only the control plane's own network
// position reaches — loopback, private, carrier-grade NAT, link-local,
// unspecified, or multicast. It is a package var so tests can reach an
// httptest server on loopback.
var Internal = func(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || sharedAddressSpace.Contains(ip)
}

// sharedAddressSpace is RFC 6598's carrier-grade NAT range, which netip does
// not count as private and which cloud providers use internally.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// RefuseHost refuses a host that is, or resolves to, an internal address.
// Resolving here catches a name pointed inward when a proxy does the dialing;
// Client checks the address it actually dials, so a name that answers
// differently a second time is caught too.
func RefuseHost(ctx context.Context, host string) error {
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if Internal(ip) {
			what := host
			if ip.Unmap().String() != host {
				what = host + " resolves to " + ip.Unmap().String() + ", which"
			}
			return fmt.Errorf("%s is an internal address", what)
		}
	}
	return nil
}

// Client is base whose dial to target itself checks the address it connects
// to, each dial bounded by dialTimeout. A dial to anything else is the
// configured proxy, which may well sit on loopback, and is left alone. It
// follows base's redirect policy, so a caller fetching a URL somebody wrote
// gives it one that follows none: a redirect would be a second target nobody
// checked.
func Client(base *http.Client, target *url.URL, dialTimeout time.Duration) *http.Client {
	transport, _ := base.Transport.(*http.Transport)
	if transport == nil {
		transport, _ = http.DefaultTransport.(*http.Transport)
	}
	transport = transport.Clone()
	port := target.Port()
	if port == "" {
		port = "443"
		if target.Scheme == "http" {
			port = "80"
		}
	}
	authority := net.JoinHostPort(target.Hostname(), port)
	plain := transport.DialContext
	if plain == nil {
		plain = (&net.Dialer{Timeout: dialTimeout}).DialContext
	}
	checked := &net.Dialer{Timeout: dialTimeout, Control: func(_, address string, _ syscall.RawConn) error {
		addrPort, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		if Internal(addrPort.Addr()) {
			return fmt.Errorf("%s resolved to %s, an internal address", target.Hostname(), addrPort.Addr().Unmap())
		}
		return nil
	}}
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == authority {
			return checked.DialContext(ctx, network, addr)
		}
		return plain(ctx, network, addr)
	}
	client := *base
	client.Transport = transport
	return &client
}
