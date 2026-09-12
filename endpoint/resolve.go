package endpoint

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// discoboxRecordLabel is the label a name's peer ID is published under: the
// TXT record at _discobox.<name> holds the peer the name stands for, written
// the way every other Discobox surface writes one (ADR 0114 §1).
const discoboxRecordLabel = "_discobox"

// discoboxRecord is the record name's peer ID is published at.
func discoboxRecord(name string) string {
	return discoboxRecordLabel + "." + name
}

// lookupTXT asks DNS for a name's TXT records. It is a variable so a test can
// answer for DNS rather than depend on it.
var lookupTXT = net.DefaultResolver.LookupTXT

// Resolve reads raw and settles what carries it: [Parse], then, for a
// discobox:// name, the DNS lookup that decides whether the name is a peer or
// an https server (ADR 0114 §1). Every other endpoint comes back exactly as
// Parse returns it, and DNS is asked nothing.
//
// It is the step between reading an address and dialing it, and the only place
// a name exists: what comes out is an iroh or an https endpoint, so nothing
// below it has a name to learn.
func Resolve(ctx context.Context, raw string) (Endpoint, error) {
	parsed, err := Parse(raw)
	if err != nil || parsed.Resolved() {
		return parsed, err
	}
	return resolveName(ctx, parsed)
}

// resolveName looks a name up. A record naming one peer makes it that peer;
// no record makes it https.
//
// A lookup that fails is an error, not "no record". Falling back would dial an
// iroh server over https because its resolver was slow, and the error that
// came back would name the wrong thing entirely.
func resolveName(ctx context.Context, name Endpoint) (Endpoint, error) {
	record := discoboxRecord(name.Name)
	values, err := lookupTXT(ctx, record)
	if err != nil {
		var dnsErr *net.DNSError
		if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
			return Endpoint{}, fmt.Errorf("look up %s: %w", record, err)
		}
		values = nil
	}
	peer, found, err := peerFromRecord(record, values)
	if err != nil {
		return Endpoint{}, err
	}
	if found {
		return Endpoint{Raw: name.Raw, Scheme: "iroh", Value: peer.Key(), IrohAddrs: name.IrohAddrs, Name: name.Name}, nil
	}
	if len(name.IrohAddrs) > 0 {
		return Endpoint{}, fmt.Errorf("endpoint %q carries ?addr=, which is a peer's sockets, but %s names no peer, so it is an https server", name.Raw, record)
	}
	return Endpoint{Raw: name.Raw, Scheme: "https", Value: "https://" + name.Name, Name: name.Name}, nil
}

// peerFromRecord reads the peer ID a record holds. A record that exists and
// holds anything else, or two different peers, is refused rather than skipped:
// somebody published it meaning a peer, and https is not what they meant.
func peerFromRecord(record string, values []string) (IrohID, bool, error) {
	var peer IrohID
	found := false
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		id, err := ParseIrohID(value)
		if err != nil {
			return IrohID{}, false, fmt.Errorf("%s holds %q, which is not a peer ID: %w", record, value, err)
		}
		if found && id != peer {
			return IrohID{}, false, fmt.Errorf("%s names two peers, %s and %s; it must name one", record, peer, id)
		}
		peer, found = id, true
	}
	return peer, found, nil
}

// isPeerIDHost reports whether a discobox:// host is written as a peer ID,
// which is what keeps it from ever being looked up. The version prefix and its
// dash decide it outright — "d1-…" is an ID or a mistake, never a name to ask
// DNS about — and the compact form counts when it parses, since no host name
// spells one by accident.
func isPeerIDHost(host string) bool {
	if strings.HasPrefix(strings.ToLower(host), PeerIDVersion+"-") {
		return true
	}
	_, err := ParseIrohID(host)
	return err == nil
}

// parseServerHost reads a discobox:// address that names its server by host
// rather than by peer ID (ADR 0114 §1). An IP address, or any host with a
// port, is an https server and is settled here; a bare name is left for
// [Resolve].
func parseServerHost(raw string, u *url.URL, addrs []string) (Endpoint, error) {
	hostname := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if net.ParseIP(hostname) != nil || u.Port() != "" {
		if len(addrs) > 0 {
			return Endpoint{}, fmt.Errorf("endpoint %q is an https server; ?addr= is a peer's sockets, and only a peer, or a name that resolves to one, takes it", raw)
		}
		return Endpoint{Raw: raw, Scheme: "https", Value: "https://" + strings.ToLower(u.Host)}, nil
	}
	if !isHostName(hostname) {
		return Endpoint{}, fmt.Errorf("endpoint %q: %q is not a peer ID, an IP address, or a host name", raw, u.Host)
	}
	return Endpoint{Raw: raw, Scheme: SchemeDiscobox, Value: hostname, IrohAddrs: addrs, Name: hostname}, nil
}

// isHostName reports whether name is a DNS host name: dot-separated labels of
// letters, digits and inner hyphens. Anything else cannot have a record to
// look up, and is better refused as an address than asked about.
func isHostName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}
