// Package endpoint resolves how a client reaches the Discobox control plane,
// and how the control plane listens for it, from a URL scheme alone.
//
// One vocabulary serves both sides. [Listen] turns an endpoint into the
// listener a server binds; [HTTPClient] turns the same endpoint into the base
// URL and client a caller dials. Everything above them — the generated API
// client, websocket attach, and git over [StartLoopbackProxy] — is written
// against net.Listener and *http.Client, so teaching this package a scheme
// reaches the whole product rather than part of it.
//
//	unix      unix:///run/discobox/server.sock   a socket on this machine
//	npipe     npipe:////./pipe/discobox         a named pipe on Windows
//	http      http://127.0.0.1:8080             an address a URL can name
//	discobox  discobox://d1-…                   a peer, over iroh (ADR 0097)
//	          discobox://10.0.0.5:8443           https, by IP or host and port
//	          discobox://example.com             a name, whose DNS decides
//	          discobox+http://127.0.0.1:8081     http, said outright
//
// A name is the one endpoint whose transport the address does not say:
// [Parse] leaves it unresolved and [Resolve] looks it up (ADR 0116 §1).
//
// An endpoint's capabilities are asked, not inferred: see
// [Endpoint.AutoLaunchable] and [Endpoint.DirectlyDialable]. The pool agent's
// own hop is resolved the same way by pool-agent/wire.
package endpoint

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const LogicalHTTPBaseURL = "http://discobox.local"

// SchemeDiscobox is the address scheme a user is given: "discobox://" and a
// peer ID, naming a Discobox server rather than the transport that reaches it
// (ADR 0097). It resolves to the iroh transport here, so it appears nowhere
// below Parse.
const SchemeDiscobox = "discobox"

// SchemeDiscoboxHTTP and SchemeDiscoboxHTTPS are the same address family with
// the transport said outright, for a server whose transport the rules cannot
// infer: a bare host is https, and a plain-http server — a development server
// on a port, a deployment behind something that terminates TLS elsewhere — has
// no other way to be written as a Discobox address, which is what carries a
// discobox (ADR 0116 §1).
const (
	SchemeDiscoboxHTTP  = SchemeDiscobox + "+http"
	SchemeDiscoboxHTTPS = SchemeDiscobox + "+https"
)

type Endpoint struct {
	Raw    string
	Scheme string
	Value  string
	// IrohAddrs are direct socket addresses to try for an iroh peer, carried
	// as repeated ?addr= parameters. Resolving a peer ID on its own needs a
	// discovery service, so a deployment without one carries the addresses
	// alongside it — which is also what makes two peers on one machine
	// reachable without waiting for discovery to propagate. A server offers
	// this form beside its address rather than as it (ADR 0097 §3).
	IrohAddrs []string
	// Name is the DNS name a discobox:// address named its server by, when a
	// lookup decides whether that is a peer or an https server. Unresolved,
	// Scheme is SchemeDiscobox and nothing can dial it; [Resolve] settles it
	// and keeps the name, so what a reader sees can say how it was decided.
	Name string
}

func Parse(raw string) (Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Endpoint{}, fmt.Errorf("endpoint is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Endpoint{}, err
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https":
		if u.Host == "" {
			return Endpoint{}, fmt.Errorf("%s endpoint %q must include a host", scheme, raw)
		}
		return Endpoint{Raw: raw, Scheme: scheme, Value: strings.TrimRight(raw, "/")}, nil
	case "unix":
		if u.Path == "" {
			return localDefault(raw, scheme)
		}
		return Endpoint{Raw: raw, Scheme: scheme, Value: u.Path}, nil
	case "npipe":
		value := npipePath(u)
		if value == "" {
			return localDefault(raw, scheme)
		}
		return Endpoint{Raw: raw, Scheme: scheme, Value: value}, nil
	case SchemeDiscoboxHTTP, SchemeDiscoboxHTTPS:
		// The transport is named, so nothing is inferred and nothing is looked
		// up: the address says what carries it and Parse says the same.
		return parseDiscoboxTransport(raw, u, strings.TrimPrefix(scheme, SchemeDiscobox+"+"))
	case SchemeDiscobox, "iroh":
		// Both spell the same thing. "discobox://<peer-id>" is the address a
		// user is given and the one a server prints; "iroh://<peer-id>" names
		// the transport, for someone who is debugging it (ADR 0097 §§2, 7).
		// Either with no ID is the listen form: a server's identity comes from
		// its key file, so there is nothing about the address to configure.
		//
		// The result says "iroh" whichever was written, so nothing below Parse
		// has a second scheme to learn.
		//
		// discobox:// also names a server by host (ADR 0116 §1). A peer ID is
		// recognized first, so it never reaches a resolver; an IP or a host
		// with a port is https; a bare name is left for Resolve.
		host := strings.TrimSpace(u.Host)
		if host == "" {
			if path := strings.Trim(u.Path, "/"); path != "" {
				return Endpoint{}, fmt.Errorf("endpoint %q must be %s://<peer-id>, or %s:// to listen", raw, scheme, scheme)
			}
			return Endpoint{Raw: raw, Scheme: "iroh"}, nil
		}
		if path := strings.Trim(u.Path, "/"); path != "" {
			return Endpoint{}, fmt.Errorf("endpoint %q names a discobox; a server's address ends at the server, %s://%s", raw, scheme, host)
		}
		addrs := u.Query()["addr"]
		// iroh:// names the transport, so it takes nothing but a peer ID.
		if scheme == "iroh" || isPeerIDHost(host) {
			id, err := ParseIrohID(host)
			if err != nil {
				return Endpoint{}, err
			}
			return Endpoint{Raw: raw, Scheme: "iroh", Value: id.Key(), IrohAddrs: addrs}, nil
		}
		return parseServerHost(raw, u, addrs)
	default:
		return Endpoint{}, fmt.Errorf("unsupported endpoint scheme %q in %q", u.Scheme, raw)
	}
}

// parseDiscoboxTransport reads a discobox+http:// or discobox+https:// address:
// the Discobox address family with its transport named rather than inferred
// (ADR 0116 §1). It is an http endpoint like any other, so it is settled here
// and nothing below Parse sees the spelling.
func parseDiscoboxTransport(raw string, u *url.URL, transport string) (Endpoint, error) {
	host := strings.TrimSpace(u.Host)
	if host == "" {
		return Endpoint{}, fmt.Errorf("endpoint %q must name a host: %s://<host>[:<port>]", raw, SchemeDiscobox+"+"+transport)
	}
	if path := strings.Trim(u.Path, "/"); path != "" {
		return Endpoint{}, fmt.Errorf("endpoint %q names a discobox; a server's address ends at the server, %s://%s", raw, SchemeDiscobox+"+"+transport, host)
	}
	if len(u.Query()["addr"]) > 0 {
		return Endpoint{}, fmt.Errorf("endpoint %q is reached over %s; ?addr= is a peer's sockets, and only a peer, or a name that resolves to one, takes it", raw, transport)
	}
	return Endpoint{Raw: raw, Scheme: transport, Value: transport + "://" + strings.ToLower(host)}, nil
}

// localDefault resolves the empty form of a local IPC scheme — "unix://" or
// "npipe://" — to the endpoint this machine uses when nobody names one. It is
// the shorthand "iroh://" already has: the scheme names the transport, and the
// address is derived rather than chosen, so writing it out is busywork that
// invites a wrong path. It is what lets DISCOBOX_SERVER_LISTEN say
// "unix://,iroh://" — where I always listen, plus iroh.
//
// A scheme this platform has no default for is an error rather than a silent
// substitution: "unix://" on Windows is a configuration written for somewhere
// else, and quietly binding a named pipe would hide that rather than say it.
func localDefault(raw, scheme string) (Endpoint, error) {
	parsed, err := Parse(DefaultEndpoint())
	if err != nil {
		return Endpoint{}, fmt.Errorf("resolve the default endpoint for %q: %w", raw, err)
	}
	if parsed.Scheme != scheme {
		return Endpoint{}, fmt.Errorf(
			"%s endpoint %q has no default on this platform; the local endpoint here is %s",
			scheme, raw, DefaultEndpoint())
	}
	// The resolved endpoint is what Raw carries, not the shorthand that was
	// typed: Raw is what a listener displays, and an operator reading
	// "listening on unix://" learns nothing about where their server is. The
	// iroh listener resolves its display for the same reason.
	return parsed, nil
}

// AutoLaunchable reports whether a server on this endpoint is one the CLI may
// start for itself. Only an endpoint that names a filesystem object on this
// machine qualifies: a server reached over the network belongs to whoever runs
// it, and a client that cannot see its filesystem cannot be the one to launch
// it.
func (e Endpoint) AutoLaunchable() bool {
	return e.Scheme == "unix" || e.Scheme == "npipe"
}

// DirectlyDialable reports whether a tool that speaks only URLs can reach this
// endpoint as configured. git is the case that matters: the CLI shells out to
// it for source push and apply, and it has no way to dial a socket, a pipe, or
// a peer-to-peer address. Everything else is reached through
// [StartLoopbackProxy].
//
// This is deliberately not the negation of [Endpoint.AutoLaunchable]. The two
// agree for every local scheme and diverge for a remote non-IP transport, which
// needs the bridge and must never be launched.
func (e Endpoint) DirectlyDialable() bool {
	return e.Scheme == "http" || e.Scheme == "https"
}

// Resolved reports whether this endpoint's transport is known. Every endpoint
// but a discobox:// name is resolved by Parse; a name is not until [Resolve]
// has looked it up, and nothing can dial it before then.
func (e Endpoint) Resolved() bool {
	return e.Scheme != SchemeDiscobox
}

// IrohID returns the endpoint ID this endpoint dials. It reports an error for
// the listen form, which names no peer, and for any other scheme.
func (e Endpoint) IrohID() (IrohID, error) {
	if e.Scheme != "iroh" {
		return IrohID{}, fmt.Errorf("endpoint %q is not an iroh endpoint", e.Raw)
	}
	if e.Value == "" {
		return IrohID{}, fmt.Errorf("iroh endpoint %q names no endpoint ID to dial", e.Raw)
	}
	return ParseIrohID(e.Value)
}

func npipePath(u *url.URL) string {
	value := u.Host + u.Path
	if value == "" {
		value = u.Opaque
	}
	value = strings.ReplaceAll(value, "/", `\`)
	if strings.HasPrefix(value, `\\.\pipe\`) {
		return value
	}
	value = strings.TrimPrefix(value, `\`)
	if value == "" {
		return ""
	}
	return `\\.\pipe\` + value
}

// HTTPClient returns the base URL and the client that reach parsed. A name has
// to have been through [Resolve] first: which transport it takes is DNS's
// answer, and asking needs a context this has no business inventing.
func HTTPClient(parsed Endpoint, base http.RoundTripper) (baseURL string, client *http.Client, err error) {
	switch parsed.Scheme {
	case "http", "https":
		if base == nil {
			return parsed.Value, http.DefaultClient, nil
		}
		return parsed.Value, &http.Client{Transport: base}, nil
	case "unix":
		transport, err := unixRoundTripper(parsed.Value, base)
		if err != nil {
			return "", nil, err
		}
		return LogicalHTTPBaseURL, &http.Client{Transport: transport}, nil
	case "npipe":
		transport, err := npipeRoundTripper(parsed.Value, base) //nolint:staticcheck // SA4023: npipeRoundTripper is a stub that always errors on non-Windows; the check is meaningful on Windows.
		if err != nil {                                         //nolint:staticcheck // SA4023: see above; comparison is only tautological on non-Windows builds.
			return "", nil, err
		}
		return LogicalHTTPBaseURL, &http.Client{Transport: transport}, nil
	case "iroh":
		transport, err := irohRoundTripper(parsed, base)
		if err != nil {
			return "", nil, err
		}
		return LogicalHTTPBaseURL, &http.Client{Transport: transport}, nil
	case SchemeDiscobox:
		return "", nil, fmt.Errorf("endpoint %q is a name that has not been looked up; Resolve decides whether it is a peer or an https server", parsed.Raw)
	default:
		return "", nil, fmt.Errorf("unsupported endpoint scheme %q", parsed.Scheme)
	}
}
