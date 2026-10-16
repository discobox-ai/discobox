// Package gateway is a project's gateways (ADR 26-10-16-470): routes that send
// a provider's API, matched by origin and path prefix, to another origin and
// prefix, with request headers removed, set, and added on the way.
//
// It is shared because the control plane validates what people set, the pool
// agent records it, and the proxy applies it, and a route that one of them
// reads differently either sends a request somewhere nobody named or lets one
// past the gateway it was meant to go through.
package gateway

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// EnvPrefix begins every server environment variable a header may take its
// value from (§4). A gateway can point at any host, so a reference to any
// other variable — a database URL, a signing key — would send it there.
const EnvPrefix = "DISCOBOX_GATEWAY_"

// Route is one gateway: a request whose origin is Match's and whose path is
// beneath Match's path goes to Target's origin, with Match's path replaced by
// Target's, and RequestHeaders applied.
type Route struct {
	Name           string         `json:"name"`
	Match          string         `json:"match"`
	Target         string         `json:"target"`
	RequestHeaders HeaderModifier `json:"requestHeaders,omitzero"`
}

// HeaderModifier is Gateway API's RequestHeaderModifier, applied in the order
// its fields are listed: Remove, then Set (replacing any value), then Add
// (appending).
type HeaderModifier struct {
	Remove []string `json:"remove,omitempty"`
	Set    []Header `json:"set,omitempty"`
	Add    []Header `json:"add,omitempty"`
}

// Header is one header a gateway sets or adds. Exactly one of Value and Env
// is non-empty: Value is the header's value, Env the server environment
// variable the value is resolved from when a request needs it.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
	Env   string `json:"env,omitempty"`
}

// Env lists the environment variables a route's headers take values from, in
// the order they appear, without duplicates.
func (r Route) Env() []string {
	var out []string
	for _, header := range slices.Concat(r.RequestHeaders.Set, r.RequestHeaders.Add) {
		if header.Env != "" && !slices.Contains(out, header.Env) {
			out = append(out, header.Env)
		}
	}
	return out
}

// HeaderNames lists every header a route touches, canonical, which is what
// the proxy redacts from what it records.
func (r Route) HeaderNames() []string {
	var out []string
	add := func(name string) {
		name = http.CanonicalHeaderKey(name)
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	for _, name := range r.RequestHeaders.Remove {
		add(name)
	}
	for _, header := range slices.Concat(r.RequestHeaders.Set, r.RequestHeaders.Add) {
		add(header.Name)
	}
	return out
}

// Normalize trims every field, writes Match and Target in their canonical
// form — lowercase scheme and host, the default port dropped, no trailing
// slash — and header names in canonical case. A URL that does not parse is
// left as given for Validate to refuse.
func Normalize(r Route) Route {
	r.Name = strings.TrimSpace(r.Name)
	r.Match = normalizeURL(r.Match)
	r.Target = normalizeURL(r.Target)
	var remove []string
	for _, name := range r.RequestHeaders.Remove {
		remove = append(remove, http.CanonicalHeaderKey(strings.TrimSpace(name)))
	}
	r.RequestHeaders = HeaderModifier{
		Remove: remove,
		Set:    normalizeHeaders(r.RequestHeaders.Set),
		Add:    normalizeHeaders(r.RequestHeaders.Add),
	}
	return r
}

func normalizeHeaders(in []Header) []Header {
	var out []Header
	for _, header := range in {
		out = append(out, Header{
			Name:  http.CanonicalHeaderKey(strings.TrimSpace(header.Name)),
			Value: header.Value,
			Env:   strings.TrimSpace(header.Env),
		})
	}
	return out
}

func normalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, err := parseOrigin(raw)
	if err != nil {
		return raw
	}
	return parsed.String()
}

var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// refusedHeaders are the names a route may not touch: the hop-by-hop headers,
// which describe the sandbox's connection to the proxy and are dropped before
// a gateway applies, and Host, which is the target's.
var refusedHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	"Host",
}

// ValidateName refuses a name that is not a DNS label.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("gateway name %q must be a DNS label: lowercase letters, digits and '-', at most 63", name)
	}
	return nil
}

// Validate refuses a route the proxy could not apply as written. It expects a
// normalized route.
func (r Route) Validate() error {
	if err := ValidateName(r.Name); err != nil {
		return err
	}
	if _, err := parseOrigin(r.Match); err != nil {
		return fmt.Errorf("gateway %s: match: %w", r.Name, err)
	}
	if _, err := parseOrigin(r.Target); err != nil {
		return fmt.Errorf("gateway %s: target: %w", r.Name, err)
	}
	for _, name := range r.RequestHeaders.Remove {
		if err := validateHeaderName(name); err != nil {
			return fmt.Errorf("gateway %s: remove: %w", r.Name, err)
		}
	}
	for _, header := range slices.Concat(r.RequestHeaders.Set, r.RequestHeaders.Add) {
		if err := header.validate(); err != nil {
			return fmt.Errorf("gateway %s: %w", r.Name, err)
		}
	}
	return nil
}

func (h Header) validate() error {
	if err := validateHeaderName(h.Name); err != nil {
		return err
	}
	switch {
	case h.Value != "" && h.Env != "":
		return fmt.Errorf("header %s has both a value and an environment variable", h.Name)
	case h.Env != "":
		return ValidateEnv(h.Env)
	case h.Value == "":
		return fmt.Errorf("header %s needs a value or an environment variable; remove takes a header away", h.Name)
	case !httpguts.ValidHeaderFieldValue(h.Value):
		return fmt.Errorf("header %s has a value no request may carry", h.Name)
	}
	return nil
}

// ValidateEnv refuses an environment variable a header may not name.
func ValidateEnv(name string) error {
	if !envPattern.MatchString(name) || !strings.HasPrefix(name, EnvPrefix) || name == EnvPrefix {
		return fmt.Errorf("environment variable %q must be named %s<NAME>", name, EnvPrefix)
	}
	return nil
}

func validateHeaderName(name string) error {
	if !httpguts.ValidHeaderFieldName(name) {
		return fmt.Errorf("%q is not a header name", name)
	}
	for _, refused := range refusedHeaders {
		if strings.EqualFold(name, refused) {
			return fmt.Errorf("header %s belongs to the connection, not the request, and a gateway may not change it", name)
		}
	}
	return nil
}

// origin is a parsed Match or Target.
type origin struct {
	scheme string
	host   string // lowercase, with a port only when it is not the scheme's
	path   string // escaped, no trailing slash; empty for the root
}

func (o origin) String() string { return o.scheme + "://" + o.host + o.path }

func parseOrigin(raw string) (origin, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return origin{}, fmt.Errorf("%q is not a URL", raw)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return origin{}, fmt.Errorf("%q is not an http or https URL", raw)
	}
	if parsed.Hostname() == "" {
		return origin{}, fmt.Errorf("%q has no host", raw)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return origin{}, fmt.Errorf("%q may be only a scheme, a host, and a path", raw)
	}
	// Cleaned as a request's path is (cleanPath), so a match and the requests
	// it names are compared in one spelling.
	path, err := cleanPath(parsed.EscapedPath())
	if err != nil {
		return origin{}, fmt.Errorf("%q: %w", raw, err)
	}
	path = strings.TrimRight(path, "/")
	return origin{scheme: scheme, host: originHost(scheme, parsed.Host), path: path}, nil
}

// originHost lowercases a host, drops a trailing dot, and drops the scheme's
// default port, so https://h, https://H.:443 and https://h:443 are one origin:
// the host as policy reads it, or a sandbox could name the matched host in a
// spelling no gateway matches and reach it directly.
func originHost(scheme, hostport string) string {
	hostport = strings.ToLower(hostport)
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return strings.TrimSuffix(hostport, ".")
	}
	host = strings.TrimSuffix(host, ".")
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		return host
	}
	return host + ":" + port
}

// ErrAmbiguousPath is a request under a gateway's origin whose path a server
// could read as another path: an encoded slash or a backslash, which some
// servers turn into a separator after the gateway has matched. It is refused
// rather than matched one way and read another.
var ErrAmbiguousPath = errors.New("path has an encoded slash or a backslash")

// decodeUnreserved decodes every percent-encoded unreserved character —
// letters, digits, and -._~ — as RFC 3986 §6.2.2.2 says is equivalent, so
// /%761 is /v1 to the match as it is to the upstream. Every other escape is
// kept as it was sent.
func decodeUnreserved(escaped string) string {
	var b strings.Builder
	for i := 0; i < len(escaped); i++ {
		if escaped[i] == '%' && i+2 < len(escaped) {
			if c, err := strconv.ParseUint(escaped[i+1:i+3], 16, 8); err == nil && unreserved(byte(c)) {
				b.WriteByte(byte(c))
				i += 2
				continue
			}
		}
		b.WriteByte(escaped[i])
	}
	return b.String()
}

func unreserved(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

// cleanPath resolves an escaped path as RFC 3986 §5.2.4 does — and as a
// server does before routing it — with empty segments merged, so a gateway
// matches the path the request will be read as: /./v1 and //v1 are /v1, and
// /v1/../x is /x, which no /v1 gateway can be stepped out of. An unreserved
// character spelled with percent-encoding is decoded first (decodeUnreserved),
// so %2e%2e is a dot segment and /%761 is /v1. A trailing slash is kept.
func cleanPath(escaped string) (string, error) {
	lower := strings.ToLower(escaped)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(escaped, `\`) {
		return "", ErrAmbiguousPath
	}
	escaped = decodeUnreserved(escaped)
	var out []string
	for _, segment := range strings.Split(escaped, "/") {
		switch segment {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, segment)
		}
	}
	cleaned := "/" + strings.Join(out, "/")
	if len(out) > 0 && strings.HasSuffix(escaped, "/") {
		cleaned += "/"
	}
	return cleaned, nil
}

// Table is one client's routes, ready to match.
type Table struct {
	routes []compiled
}

type compiled struct {
	Route
	match  origin
	target origin
}

// NewTable compiles routes. A route that does not validate is skipped, and of
// two with one match the first is kept: routes are validated when written
// (ADR 26-10-16-470 §1), so only a writer of another version gets here, and
// one route the proxy cannot read must not stop the others applying.
func NewTable(routes []Route) *Table {
	table := &Table{}
	seen := map[string]bool{}
	for _, route := range routes {
		if route.Validate() != nil || seen[route.Match] {
			continue
		}
		seen[route.Match] = true
		match, err := parseOrigin(route.Match)
		if err != nil {
			continue
		}
		target, err := parseOrigin(route.Target)
		if err != nil {
			continue
		}
		table.routes = append(table.routes, compiled{Route: route, match: match, target: target})
	}
	// Longest prefix first, so the first route that matches is the one that
	// wins; the seen set above keeps two routes from having one match.
	slices.SortStableFunc(table.routes, func(a, b compiled) int {
		return len(b.match.path) - len(a.match.path)
	})
	return table
}

// Lookup is the route named name.
func (t *Table) Lookup(name string) (Route, bool) {
	if t == nil {
		return Route{}, false
	}
	for _, route := range t.routes {
		if route.Name == name {
			return route.Route, true
		}
	}
	return Route{}, false
}

// Match is the route a request URL falls under and the URL it is forwarded
// to, or false when no route matches. The host is matched as policy reads it
// and the path as a server reads it (cleanPath), and the cleaned path is what
// is forwarded. A request to a gateway's origin whose path is ambiguous is
// ErrAmbiguousPath, whichever prefix it names.
func (t *Table) Match(u *url.URL) (Route, *url.URL, bool, error) {
	if t == nil || u == nil {
		return Route{}, nil, false, nil
	}
	scheme := strings.ToLower(u.Scheme)
	host := originHost(scheme, u.Host)
	var path string
	var pathErr error
	cleaned := false
	for _, route := range t.routes {
		if route.match.scheme != scheme || route.match.host != host {
			continue
		}
		if !cleaned {
			path, pathErr = cleanPath(u.EscapedPath())
			cleaned = true
		}
		if pathErr != nil {
			return route.Route, nil, false, pathErr
		}
		rest, ok := underPrefix(path, route.match.path)
		if !ok {
			continue
		}
		return route.Route, route.rewrite(u, rest), true, nil
	}
	return Route{}, nil, false, nil
}

// underPrefix is what is left of path beneath prefix, on a segment boundary:
// /v1 is beneath /v1 and /v1/x, never /v1beta.
func underPrefix(path, prefix string) (string, bool) {
	if prefix == "" {
		if path == "/" {
			return "", true
		}
		return path, true
	}
	if path == prefix {
		return "", true
	}
	if strings.HasPrefix(path, prefix+"/") {
		return path[len(prefix):], true
	}
	return "", false
}

func (c compiled) rewrite(u *url.URL, rest string) *url.URL {
	escaped := c.target.path + rest
	if escaped == "" {
		escaped = "/"
	}
	out := *u
	out.Scheme = c.target.scheme
	out.Host = c.target.host
	out.Path, _ = url.PathUnescape(escaped)
	out.RawPath = escaped
	return &out
}
