package desktop

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// The desktop's access boundary (ADR 26-11-06-851 §1): who may use the page,
// its API and its stream.
//
// The viewer listens on loopback, and what reaches it from outside the sandbox
// is a forward someone was authorized to open: the person's own end of it is
// `localhost:<port>` on their machine. That is not a boundary a browser
// respects by itself. Any page the person has open can send requests to a
// loopback address, and a page on a domain an attacker points at 127.0.0.1
// (DNS rebinding) is, as far as the browser is concerned, same-origin with
// whatever it reaches there. The stream carries keyboard and pointer input and
// the clipboard, so a page that could open it would be typing on the desktop.
//
// So every request, the WebSocket upgrade included, is held to two rules:
//
//   - **Its Host names this machine**: localhost, a loopback address, or a
//     name the viewer was told to answer for. A rebound domain arrives with
//     its own name and is refused.
//   - **Its Origin, when it has one, is the request's own origin.** A browser
//     sends Origin on every WebSocket and on every cross-site write, so a page
//     elsewhere is refused whatever it targets. A request with no Origin is a
//     program, not a page, and is as trusted as anything else that can reach
//     the port.
//
// Nothing behind the viewer is trusted to do this itself: voxl's own listener
// is reached only through this one, with a token it is never shown.
type accessPolicy struct {
	// hosts are names answered for besides localhost and loopback addresses,
	// lower case, without a port.
	hosts map[string]bool
}

func newAccessPolicy(extra []string) accessPolicy {
	hosts := map[string]bool{}
	for _, host := range extra {
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" {
			hosts[host] = true
		}
	}
	return accessPolicy{hosts: hosts}
}

// guard refuses a request that fails either rule, before it reaches a route.
func (p accessPolicy) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := p.check(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (p accessPolicy) check(r *http.Request) error {
	if !p.allowedHost(r.Host) {
		return fmt.Errorf("desktop: refusing a request for host %q: the desktop answers for localhost", r.Host)
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || !strings.EqualFold(parsed.Host, r.Host) {
		return fmt.Errorf("desktop: refusing a request from origin %q: only the desktop's own page may use it", origin)
	}
	return nil
}

func (p accessPolicy) allowedHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	if host == "" {
		return false
	}
	if host == "localhost" || p.hosts[host] {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// splitHostPort is net.SplitHostPort with an error that names the setting.
func splitHostPort(address string) (string, string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", "", fmt.Errorf("desktop: stream address %q: %w", address, err)
	}
	return host, port, nil
}
