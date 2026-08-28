package tui

import (
	"net"
	"net/url"
	"strconv"
)

// forwardedURL is where a URL printed inside a pane actually reaches from here.
//
// A server in a sandbox prints the address it bound — `http://localhost:8080`,
// `http://0.0.0.0:8080` — and both halves of that are wrong on this side of the
// forward. The port is only 8080 here if 8080 was free when the forward took
// it, and the bind address a server prints is not one a browser can open. So a
// link on that text points at the local end instead: the port the forward
// actually bound, on localhost.
//
// It is the name and not the address deliberately. Under WSL2 the Windows side
// reaches a Linux listener by the name — `localhost` is what the port proxy is
// published under — and a literal 127.0.0.1 handed to a browser over there is
// that machine's own loopback, which is nothing. See also portEntry, which
// spells the same URL out in the header.
//
// Anything else is returned as it came: a URL to somewhere that is not this
// sandbox is already right, and saying so is what keeps [termpane.Model] from
// linking it. Only the ports the forward has bound are moved — a link to a
// local port nothing is listening on is worse than no link, which is the rule
// the header's arrows follow too.
func (m *Model) forwardedURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	// Ahead of the bindings, which are a fresh map each time: this is called
	// for every URL on the screen, every frame, and most of them are somewhere
	// else entirely.
	remote, ok := sandboxPort(parsed)
	if !ok {
		return raw
	}
	local, ok := m.forwardedPorts()[remote]
	if !ok {
		return raw
	}
	parsed.Host = net.JoinHostPort("localhost", itoa(local))
	return parsed.String()
}

// sandboxPort is the port a URL names inside the sandbox, and whether the URL
// is one the forward could be standing in for at all.
//
// Loopback and the wildcard both qualify: they are what a server prints, and
// they name the sandbox's own network namespace either way. A port left out is
// the scheme's — `http://localhost/` is port 80 there, and 80 is as forwardable
// as anything else.
func sandboxPort(parsed *url.URL) (int, bool) {
	var fallback int
	switch parsed.Scheme {
	case "http":
		fallback = 80
	case "https":
		fallback = 443
	default:
		return 0, false
	}
	host := parsed.Hostname()
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || (!ip.IsLoopback() && !ip.IsUnspecified()) {
			return 0, false
		}
	}
	port := parsed.Port()
	if port == "" {
		return fallback, true
	}
	number, err := strconv.Atoi(port)
	if err != nil || number <= 0 {
		return 0, false
	}
	return number, true
}
