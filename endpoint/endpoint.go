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
//	unix    unix:///run/discobox/server.sock   a socket on this machine
//	npipe   npipe:////./pipe/discobox         a named pipe on Windows
//	http    http://127.0.0.1:8080             an address a URL can name
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

type Endpoint struct {
	Raw    string
	Scheme string
	Value  string
	// IrohAddrs are direct socket addresses to try for an iroh peer, carried
	// as repeated ?addr= parameters. An endpoint ID is not routable on its own:
	// resolving one needs a discovery service, and until a deployment has one
	// the address has to travel with the ID. This is the ticket idea in URL
	// form, and it is also what makes two peers on one machine reachable.
	IrohAddrs []string
}

func Parse(raw string) (Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Endpoint{}, fmt.Errorf("endpoint is required")
	}
	// An iroh ticket is not a URL. It carries the same address the iroh://
	// form spells out, in the single token iroh tools paste around, so it is
	// accepted wherever an endpoint is.
	if strings.HasPrefix(raw, irohTicketPrefix) && !strings.Contains(raw, "://") {
		return parseIrohTicket(raw)
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
	case "iroh":
		// "iroh://" with no ID is the listen form: a server's identity comes
		// from its key file, so there is nothing about the address to
		// configure. "iroh://<endpoint-id>" is the dial form, where the ID is
		// the entire address.
		host := strings.TrimSpace(u.Host)
		if host == "" {
			if path := strings.Trim(u.Path, "/"); path != "" {
				return Endpoint{}, fmt.Errorf("iroh endpoint %q must be iroh://<endpoint-id>, or iroh:// to listen", raw)
			}
			return Endpoint{Raw: raw, Scheme: scheme}, nil
		}
		id, err := ParseIrohID(host)
		if err != nil {
			return Endpoint{}, err
		}
		return Endpoint{Raw: raw, Scheme: scheme, Value: id.String(), IrohAddrs: u.Query()["addr"]}, nil
	default:
		return Endpoint{}, fmt.Errorf("unsupported endpoint scheme %q in %q", u.Scheme, raw)
	}
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

func HTTPClient(endpoint string, base http.RoundTripper) (baseURL string, client *http.Client, err error) {
	parsed, err := Parse(endpoint)
	if err != nil {
		return "", nil, err
	}
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
	default:
		return "", nil, fmt.Errorf("unsupported endpoint scheme %q", parsed.Scheme)
	}
}
