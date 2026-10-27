// Package secretroute answers where a secret may be sent (ADR 26-10-02-924):
// which of the routes a secret carries a request falls on, if any, and whether
// that route refuses it.
//
// A route is a host, which covers itself and every host beneath it the way a
// hostscope does, and an optional path prefix matched on whole segments. A
// blocked route refuses every request it is the most specific route for: it
// is what a route a judge used to read became, so that removing the judge
// sent nothing on it unjudged (ADR 26-10-27-880). AnyHost, `*`, covers every
// host (ADR 26-10-27-939). A credential is held to several lists at once — its
// secret's, its binding's and each parent's — and every one must allow a
// request. The control plane normalizes the routes a person or a harness image
// writes, and the pool proxy matches every request a secret is swapped into
// against them.
// Both go through this package so the route a person wrote is the route that
// is enforced.
package secretroute

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/discobox-ai/discobox/pkg/hostscope"
)

// Route is where a secret may be sent: a host and everything beneath it, or
// AnyHost, and within that, an optional path prefix. A blocked route refuses
// the requests it is the most specific route for.
type Route struct {
	Host    string `json:"host"`
	Path    string `json:"path,omitempty"`
	Blocked bool   `json:"blocked,omitempty"`
}

// Normalize is the one reading of a route's host and path. The host is
// normalized as hostscope does and must not be empty. The path must start
// with "/", loses a trailing "/", and "/" alone is no path: the whole host.
func Normalize(host, routePath string) (string, string, error) {
	host = hostscope.Normalize(host)
	if host == AnyHost {
		return normalizedPath(host, routePath)
	}
	if host == "" {
		return "", "", fmt.Errorf("a route must name a host")
	}
	if strings.ContainsAny(host, "/?#@:= \t*") || hasControl(host) {
		return "", "", fmt.Errorf("route host %q is not a host name", host)
	}
	return normalizedPath(host, routePath)
}

// AnyHost is the route host that covers every host (ADR 26-10-27-939). It is
// the least specific route there is: a route naming any host is more specific
// than it, so within a list a narrower route still answers for what it covers.
const AnyHost = "*"

// normalizedPath normalizes a route's path for an already normalized host.
func normalizedPath(host, routePath string) (string, string, error) {
	routePath = strings.TrimSpace(routePath)
	if routePath == "" {
		return host, "", nil
	}
	if !strings.HasPrefix(routePath, "/") {
		return "", "", fmt.Errorf("route path %q must start with /", routePath)
	}
	if strings.ContainsAny(routePath, "?# \t") || hasControl(routePath) {
		return "", "", fmt.Errorf("route path %q must be a plain path, without a query, fragment or spaces", routePath)
	}
	trimmed := strings.TrimRight(routePath, "/")
	if strings.Contains(trimmed, "//") {
		return "", "", fmt.Errorf("route path %q has an empty segment", routePath)
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "." || segment == ".." {
			return "", "", fmt.Errorf("route path %q has a %q segment", routePath, segment)
		}
	}
	return host, trimmed, nil
}

// BlockedSuffix marks a blocked route in the form a person types.
const BlockedSuffix = "=blocked"

// Parse reads a route as a person types it, HOST[/PATH][=blocked]: the host up
// to the first "/", the path after it, and the suffix to block it. The host
// and path are normalized. Anything else after an "=" is refused: it is the
// use a route once carried, which nothing reads any more.
func Parse(text string) (Route, error) {
	where, suffix, cut := strings.Cut(strings.TrimSpace(text), "=")
	blocked := false
	if cut {
		if strings.TrimSpace(suffix) != strings.TrimPrefix(BlockedSuffix, "=") {
			return Route{}, fmt.Errorf("route %q: a route is HOST[/PATH] or HOST[/PATH]%s; a route no longer carries a use", text, BlockedSuffix)
		}
		blocked = true
	}
	where = strings.TrimSpace(where)
	host, routePath := where, ""
	if i := strings.Index(where, "/"); i >= 0 {
		host, routePath = where[:i], where[i:]
	}
	host, routePath, err := Normalize(host, routePath)
	if err != nil {
		return Route{}, err
	}
	return Route{Host: host, Path: routePath, Blocked: blocked}, nil
}

// Format is a route as Parse reads it.
func Format(route Route) string {
	if route.Blocked {
		return route.Host + route.Path + BlockedSuffix
	}
	return route.Host + route.Path
}

// RequestPaths are the paths a request's URL is matched on, each with its dot
// segments resolved, so a request cannot step out of the prefix it names
// (/mcp/readonly/../write is /mcp/write). The first is the path decoded. An
// encoded "/" is data to a server that routes on the path as sent and a
// separator to one that decodes it first, and the proxy cannot tell which the
// upstream is, so a path holding one has a second reading that keeps it
// inside its segment: /mcp/x%2F..%2Freadonly is /mcp/readonly decoded and
// /mcp/x%2F..%2Freadonly as sent. A request is held to every reading. A URL
// that does not parse has the root path, which only a whole-host route covers.
func RequestPaths(rawURL string) []string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Path == "" {
		return []string{"/"}
	}
	decoded := path.Clean("/" + u.Path)
	segments := strings.Split(u.EscapedPath(), "/")
	for i, segment := range segments {
		// Every escape but a "/" is read as the server would, so an encoded
		// dot segment (%2e%2e) still resolves.
		if unescaped, err := url.PathUnescape(segment); err == nil && !strings.Contains(unescaped, "/") {
			segments[i] = unescaped
		}
	}
	asSent := path.Clean("/" + strings.Join(segments, "/"))
	if asSent == decoded {
		return []string{decoded}
	}
	return []string{decoded, asSent}
}

// PathCovers reports whether a route's path prefix covers a request path, on
// whole segments: /mcp covers /mcp, /mcp/ and /mcp/readonly, never /mcpx. An
// empty prefix covers every path. Case is ignored, so a server that does not
// mind case cannot be reached around a prefix by changing it.
func PathCovers(prefix, requestPath string) bool {
	if prefix == "" {
		return true
	}
	if len(requestPath) < len(prefix) || !strings.EqualFold(requestPath[:len(prefix)], prefix) {
		return false
	}
	return len(requestPath) == len(prefix) || requestPath[len(prefix)] == '/'
}

// Match returns the routes a request to host at requestPath falls on: those
// covering it at the most specific host — the one with the most labels — and,
// among them, the longest path. The routes returned tie, and any one of them
// answers for the request. None returned is a request no route covers.
func Match(routes []Route, host, requestPath string) []Route {
	host = hostscope.Normalize(host)
	bestLabels, bestPath := -1, -1
	var out []Route
	for _, route := range routes {
		scope := hostscope.Normalize(route.Host)
		if !routeCovers(scope, host) || !PathCovers(route.Path, requestPath) {
			continue
		}
		labels := hostLabels(scope)
		switch {
		case labels > bestLabels || (labels == bestLabels && len(route.Path) > bestPath):
			bestLabels, bestPath = labels, len(route.Path)
			out = []Route{route}
		case labels == bestLabels && len(route.Path) == bestPath:
			out = append(out, route)
		}
	}
	return out
}

// routeCovers reports whether a route's normalized host covers host: AnyHost
// covers every host, any other host itself and the hosts beneath it.
func routeCovers(scope, host string) bool {
	if scope == AnyHost {
		return host != ""
	}
	return scope != "" && hostscope.Covers(scope, host)
}

// Decide says whether a request to host at rawURL may carry a secret, held to
// every route list it is under — the secret's, the binding's when it has any,
// and each parent's (ADR 26-10-27-880 §4) — and why not when it may not. Each
// list is read on its own: every reading of the path (RequestPaths) must fall
// on a route of it, and the most specific routes it falls on must not be
// blocked; a tie with a blocked route refuses. A list with no routes allows
// nothing, and so does no list at all: a credential is sent where its routes
// say, and only there.
func Decide(lists [][]Route, host, rawURL string) (allowed bool, reason string) {
	if len(lists) == 0 {
		return false, "this credential has no routes"
	}
	for _, routes := range lists {
		if len(routes) == 0 {
			return false, "this credential has no routes"
		}
		for _, requestPath := range RequestPaths(rawURL) {
			matched := Match(routes, host, requestPath)
			if len(matched) == 0 {
				return false, fmt.Sprintf("this credential has no route to %s%s", host, requestPath)
			}
			for _, route := range matched {
				if route.Blocked {
					return false, fmt.Sprintf("this credential's route %s refuses %s%s", Format(route), host, requestPath)
				}
			}
		}
	}
	return true, ""
}

// HostAllowed reports whether some request to host could carry a secret held
// to lists: in every list, an open route covers the host and no blocked route
// shuts the whole of it. A blocked route without a path shuts the host it
// covers against every route less specific than it, so a host only it answers
// for is refused here as Decide would refuse every path on it. It is the check
// a value is handed out by, which knows the host and not yet the path; the
// request's own path is held to Decide.
func HostAllowed(lists [][]Route, host string) bool {
	if len(lists) == 0 {
		return false
	}
	host = hostscope.Normalize(host)
	for _, routes := range lists {
		if !hostOpen(routes, host) {
			return false
		}
	}
	return true
}

// hostOpen reports whether one list leaves some path on host open: an open
// route covers it that outranks every blocked route covering the whole host —
// on a more specific host, or on the same host with a path.
func hostOpen(routes []Route, host string) bool {
	shut := -1
	for _, route := range routes {
		scope := hostscope.Normalize(route.Host)
		if route.Blocked && route.Path == "" && routeCovers(scope, host) {
			shut = max(shut, hostLabels(scope))
		}
	}
	for _, route := range routes {
		scope := hostscope.Normalize(route.Host)
		if route.Blocked || !routeCovers(scope, host) {
			continue
		}
		// More specific than every host-wide block, or as specific with a
		// path, which outranks the block on the paths it covers.
		if labels := hostLabels(scope); labels > shut || (labels == shut && route.Path != "") {
			return true
		}
	}
	return false
}

// hostLabels is how specific a route's host is: its label count, and 0 for
// AnyHost, which is less specific than any host it could be compared with.
func hostLabels(scope string) int {
	if scope == AnyHost {
		return 0
	}
	return strings.Count(scope, ".") + 1
}

// hasControl reports whether text holds a control character. A route is
// shown to the person who approves or reads it, so nothing in it may move
// their terminal; no host or path needs one.
func hasControl(text string) bool {
	return strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) })
}
