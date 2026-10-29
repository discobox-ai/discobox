// Package secretroute answers where a secret may be sent (ADR 26-10-02-924,
// ADR 26-10-27-880 §4): which of the routes a secret carries a request falls
// on, if any, and whether that route refuses it.
//
// A route names hosts — exact names, `*.suffix` for the hosts strictly beneath
// a suffix, or `*` alone for any host (ADR 26-10-27-939) — and, optionally,
// the methods, the paths (segment globs) and the GraphQL operations or MCP
// tools it allows. A blocked route refuses what it is the most specific
// candidate for: it is a carve-out inside a wider route of the same list, and
// what a route a judge used to read became, so that removing the judge sent
// nothing on it unjudged. A credential is held to several lists at once — its
// secret's, its binding's and each parent's — and every one must allow a
// request.
//
// Matching is asymmetric on purpose, so a carve-out cannot be stepped around.
// An open route matches a request only when every item of what it constrains
// is readable and matches: every method the request names (its request line,
// its method-override headers, `_method` in its URL, and on a POST `_method`
// in a URL-encoded form, untyped or JSON body), every GraphQL operation and
// root field, every MCP message. A blocked route matches when any one item
// does, or when what it constrains cannot be read — a body past the capture
// limit, in an encoding the proxy cannot decode, or one that does not parse.
// One exception keeps large requests working, and it is "unreadable, never
// unparsable": an open method route ignores a POST body that could not be
// read at all when it is JSON, or multipart, so a `_method` hidden there is
// caught only by a blocked method route. A form or untyped body that could
// not be read, and any body read that does not parse, refuses an open
// method route like every other unknown.
//
// The control plane normalizes the routes a person, an agent or a
// harness image writes, and the pool proxy decides every request a secret is
// swapped into against them. Both go through this package so the route a
// person wrote is the route that is enforced.
package secretroute

import (
	"fmt"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/pkg/hostscope"
)

// Route is where a secret may be sent. Every part but Hosts is optional, and
// an absent part allows anything: no methods is any method, no paths is any
// path, and no protocol block is any body. A blocked route refuses the
// requests it is the most specific candidate for.
type Route struct {
	Hosts   []string `json:"hosts"`
	Methods []string `json:"methods,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	GraphQL *GraphQL `json:"graphql,omitempty"`
	MCP     *MCP     `json:"mcp,omitempty"`
	Blocked bool     `json:"blocked,omitempty"`
}

// GraphQL holds a route to the GraphQL documents a request executes: the
// operation types and the root fields each operation selects. None of either
// is any.
type GraphQL struct {
	Operations []string `json:"operations,omitempty"` // query|mutation|subscription; none = any
	Fields     []string `json:"fields,omitempty"`     // root fields, trailing * allowed; none = any
}

// MCP holds a route to the MCP tools a request calls. None is any tool. An
// open MCP route also allows what calls no tool: the protocol's own methods
// (initialize, ping, tools/list, notifications/*), responses to the server's
// requests, and the streamable HTTP transport's GET of the server's stream and
// DELETE of the session, each with an empty body. A blocked one refuses only
// calls of the tools it names, and whatever body it cannot read.
type MCP struct {
	Tools []string `json:"tools,omitempty"` // tools/call params.name, trailing * allowed; none = any tool
}

// AnyHost is the route host that covers every host (ADR 26-10-27-939). It is
// the least specific host there is: a route naming any host is more specific
// than it, so within a list a narrower route still answers for what it covers.
const AnyHost = "*"

// BlockedSuffix marks a blocked route in the form a person types.
const BlockedSuffix = "=blocked"

// Syntax is the typed form of a route, as Parse reads it and Format writes it,
// for help text and errors to show.
const Syntax = "[METHODS ]HOSTS[PATHS][ graphql=OPS[:FIELDS]][ mcp=TOOLS][" + BlockedSuffix + "]"

// The GraphQL operation types a route may name.
var graphqlOperations = []string{"query", "mutation", "subscription"}

// Normalize is the one reading of a route: it validates every part, lower-cases
// hosts and operation types, upper-cases methods, and sorts and dedupes every
// list. A `*` among a block's fields or tools is every one, so the list is
// dropped. A route without hosts, or with both a GraphQL and an MCP block,
// which no body could satisfy, is refused.
func Normalize(r Route) (Route, error) {
	out := Route{Blocked: r.Blocked}
	if len(r.Hosts) == 0 {
		return Route{}, fmt.Errorf("a route must name a host")
	}
	for _, host := range r.Hosts {
		normalized, err := normalizeHost(host)
		if err != nil {
			return Route{}, err
		}
		out.Hosts = append(out.Hosts, normalized)
	}
	for _, method := range r.Methods {
		normalized, err := normalizeMethod(method)
		if err != nil {
			return Route{}, err
		}
		out.Methods = append(out.Methods, normalized)
	}
	for _, routePath := range r.Paths {
		normalized, err := normalizePath(routePath)
		if err != nil {
			return Route{}, err
		}
		out.Paths = append(out.Paths, normalized)
	}
	if r.GraphQL != nil && r.MCP != nil {
		return Route{}, fmt.Errorf("a route matches GraphQL or MCP, not both: no request body is both")
	}
	if r.GraphQL != nil {
		graphql := &GraphQL{}
		for _, operation := range r.GraphQL.Operations {
			operation = strings.ToLower(strings.TrimSpace(operation))
			if !slices.Contains(graphqlOperations, operation) {
				return Route{}, fmt.Errorf("route GraphQL operation %q is not one of query, mutation or subscription", operation)
			}
			graphql.Operations = append(graphql.Operations, operation)
		}
		fields, err := normalizeNames("GraphQL field", r.GraphQL.Fields, isGraphQLNameRune)
		if err != nil {
			return Route{}, err
		}
		graphql.Fields = fields
		graphql.Operations = sortedSet(graphql.Operations)
		out.GraphQL = graphql
	}
	if r.MCP != nil {
		tools, err := normalizeNames("MCP tool", r.MCP.Tools, isToolNameRune)
		if err != nil {
			return Route{}, err
		}
		out.MCP = &MCP{Tools: tools}
	}
	out.Hosts = sortedSet(out.Hosts)
	out.Methods = sortedSet(out.Methods)
	out.Paths = sortedSet(out.Paths)
	return out, nil
}

// normalizeHost reads one host pattern: an exact name, `*.suffix`, or `*`.
func normalizeHost(host string) (string, error) {
	host = hostscope.Normalize(host)
	if host == AnyHost {
		return host, nil
	}
	name := strings.TrimPrefix(host, "*.")
	if name == "" {
		return "", fmt.Errorf("a route must name a host")
	}
	if strings.ContainsAny(name, "/?#@:=, \t*") || hasControl(name) {
		if strings.Contains(name, "*") {
			return "", fmt.Errorf("route host %q may hold * only as `*` alone or a leading `*.`", host)
		}
		return "", fmt.Errorf("route host %q is not a host name", host)
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") {
		return "", fmt.Errorf("route host %q has an empty label", host)
	}
	return host, nil
}

// normalizeMethod reads one HTTP method, upper-cased: a token of letters,
// digits, `-` and `_`.
func normalizeMethod(method string) (string, error) {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" || strings.ContainsFunc(method, func(r rune) bool {
		return (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_'
	}) {
		return "", fmt.Errorf("route method %q is not an HTTP method", method)
	}
	return method, nil
}

// normalizePath reads one path pattern: it starts with "/", loses a trailing
// "/", and its segments are literal but for `*`, exactly one segment, and
// `**`, any number of segments and only last. It holds no query, fragment,
// space, "=" (the blocked suffix) or ",/" (the separator between paths).
func normalizePath(routePath string) (string, error) {
	routePath = strings.TrimSpace(routePath)
	if !strings.HasPrefix(routePath, "/") {
		return "", fmt.Errorf("route path %q must start with /", routePath)
	}
	if strings.ContainsAny(routePath, "?#= \t") || strings.Contains(routePath, ",/") || hasControl(routePath) {
		return "", fmt.Errorf("route path %q must be a plain path, without a query, fragment, spaces, = or ,/", routePath)
	}
	if routePath == "/" {
		return routePath, nil
	}
	trimmed := strings.TrimSuffix(routePath, "/")
	segments := strings.Split(trimmed, "/")[1:]
	for i, segment := range segments {
		switch {
		case segment == "":
			return "", fmt.Errorf("route path %q has an empty segment", routePath)
		case segment == "." || segment == "..":
			return "", fmt.Errorf("route path %q has a %q segment", routePath, segment)
		case segment == "**" && i != len(segments)-1:
			return "", fmt.Errorf("route path %q has ** before its last segment", routePath)
		case segment != "*" && segment != "**" && strings.Contains(segment, "*"):
			return "", fmt.Errorf("route path %q has * inside a segment: * is a whole segment, ** the rest", routePath)
		}
	}
	return trimmed, nil
}

// normalizeNames reads a list of GraphQL fields or MCP tools: names of
// allowed runes, each optionally ending in one `*`. A `*` alone is every
// name, so it drops the list.
func normalizeNames(what string, names []string, allowed func(rune) bool) ([]string, error) {
	var out []string
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "*" {
			return nil, nil
		}
		base := strings.TrimSuffix(name, "*")
		if base == "" || strings.ContainsFunc(base, func(r rune) bool { return !allowed(r) }) {
			return nil, fmt.Errorf("route %s %q is not a name, optionally ending in *", what, name)
		}
		out = append(out, name)
	}
	return sortedSet(out), nil
}

func isGraphQLNameRune(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isToolNameRune(r rune) bool {
	return isGraphQLNameRune(r) || r == '-' || r == '.' || r == '/'
}

// sortedSet sorts and dedupes a list, and is nil for an empty one.
func sortedSet(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	list = slices.Clone(list)
	slices.Sort(list)
	return slices.Compact(list)
}

// Parse reads a route as a person types it (Syntax):
//
//	[METHODS ]HOSTS[PATHS][ graphql=OPS[:FIELDS]][ mcp=TOOLS][=blocked]
//
// METHODS is an optional first word of upper-case methods joined by commas.
// HOSTS is a comma list up to the first "/", and PATHS from it, several
// joined by a "," immediately followed by "/". OPS is a comma list of
// operation types, or `*` for any, and FIELDS a comma list of root fields;
// TOOLS is a comma list of tool names, or `*` for any. The `=blocked` suffix
// on the whole text blocks the route. The route is normalized. Any other word
// is refused: an "=" anywhere else is the use a route once carried, which
// nothing reads any more.
func Parse(text string) (Route, error) {
	text = strings.TrimSpace(text)
	if rest, ok := strings.CutSuffix(text, "blocked"); ok {
		if rest, ok := strings.CutSuffix(strings.TrimSpace(rest), "="); ok {
			route, err := parseOpen(rest)
			if err == nil {
				route.Blocked = true
				return route, nil
			}
			// A route whose last tool is named "blocked" reads as written:
			// stripping the suffix left a bare `mcp`, which is not a route.
			if again, againErr := parseOpen(text); againErr == nil {
				return again, nil
			}
			return Route{}, fmt.Errorf("route %q: %w", text, err)
		}
	}
	route, err := parseOpen(text)
	if err != nil {
		return Route{}, fmt.Errorf("route %q: %w", text, err)
	}
	return route, nil
}

// parseOpen reads a route without its blocked suffix.
func parseOpen(text string) (Route, error) {
	words := strings.Fields(text)
	if len(words) == 0 {
		return Route{}, fmt.Errorf("a route must name a host")
	}
	// A lone word of nothing but methods (`GET`, `GET,POST`) is methods
	// missing their host, not hosts that happen to be spelled like methods.
	if len(words) == 1 && allMethods(words[0]) {
		methods := strings.ToUpper(words[0])
		verb := "is a method"
		if strings.Contains(methods, ",") {
			verb = "are methods"
		}
		return Route{}, fmt.Errorf("%s %s; a route needs a host: `%s api.example.com/...`", methods, verb, methods)
	}
	if len(words) > 1 && !isMethodsWord(words[0]) && allMethods(words[0]) {
		return Route{}, fmt.Errorf("methods are upper case: %s", strings.ToUpper(words[0]))
	}
	var route Route
	if len(words) > 1 && isMethodsWord(words[0]) {
		route.Methods = strings.Split(words[0], ",")
		words = words[1:]
	}
	where := words[0]
	hosts, paths := where, ""
	if i := strings.Index(where, "/"); i >= 0 {
		hosts, paths = where[:i], where[i:]
	}
	if strings.Contains(hosts, "=") {
		return Route{}, fmt.Errorf("a route is %s; a route no longer carries a use", Syntax)
	}
	route.Hosts = strings.Split(hosts, ",")
	if paths != "" {
		for i, routePath := range strings.Split(paths, ",/") {
			if i > 0 {
				routePath = "/" + routePath
			}
			route.Paths = append(route.Paths, routePath)
		}
	}
	for _, word := range words[1:] {
		key, value, _ := strings.Cut(word, "=")
		switch key {
		case "graphql":
			if route.GraphQL != nil {
				return Route{}, fmt.Errorf("a route has one graphql=")
			}
			graphql, err := parseGraphQL(value)
			if err != nil {
				return Route{}, err
			}
			route.GraphQL = graphql
		case "mcp":
			if route.MCP != nil {
				return Route{}, fmt.Errorf("a route has one mcp=")
			}
			tools, err := splitList("mcp=TOOLS", value)
			if err != nil {
				return Route{}, err
			}
			route.MCP = &MCP{Tools: tools}
		default:
			return Route{}, fmt.Errorf("%q is not part of a route: a route is %s", word, Syntax)
		}
	}
	return Normalize(route)
}

// knownMethods are the HTTP methods a word is read as, whatever its case, to
// tell a person who typed one where a host belongs that it is not one.
var knownMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}

func knownMethod(word string) bool {
	return slices.Contains(knownMethods, strings.ToUpper(word))
}

// allMethods reports whether a word is a comma list of nothing but known
// methods, in any case.
func allMethods(word string) bool {
	return !slices.ContainsFunc(strings.Split(word, ","), func(w string) bool { return !knownMethod(w) })
}

// isMethodsWord reports whether a route's first word is its methods: upper
// case, without anything a host or path holds.
func isMethodsWord(word string) bool {
	return word == strings.ToUpper(word) && !strings.ContainsAny(word, "./*=:") &&
		strings.ContainsFunc(word, func(r rune) bool { return r >= 'A' && r <= 'Z' })
}

// parseGraphQL reads the value of a graphql= word: OPS[:FIELDS].
func parseGraphQL(value string) (*GraphQL, error) {
	ops, fields, hasFields := strings.Cut(value, ":")
	operations, err := splitList("graphql=OPS", ops)
	if err != nil {
		return nil, err
	}
	graphql := &GraphQL{}
	if !slices.Contains(operations, "*") {
		graphql.Operations = operations
	}
	if hasFields {
		if graphql.Fields, err = splitList("graphql=OPS:FIELDS", fields); err != nil {
			return nil, err
		}
	}
	return graphql, nil
}

// splitList reads a comma list that must name at least one item.
func splitList(what, value string) ([]string, error) {
	items := strings.Split(value, ",")
	if slices.Contains(items, "") {
		return nil, fmt.Errorf("%s needs a comma list without empty items (`*` for any)", what)
	}
	return items, nil
}

// Format is a route as Parse reads it, canonical when the route is normalized.
func Format(r Route) string {
	var b strings.Builder
	if len(r.Methods) > 0 {
		b.WriteString(strings.Join(r.Methods, ","))
		b.WriteByte(' ')
	}
	b.WriteString(strings.Join(r.Hosts, ","))
	b.WriteString(strings.Join(r.Paths, ","))
	if r.GraphQL != nil {
		b.WriteString(" graphql=")
		if len(r.GraphQL.Operations) == 0 {
			b.WriteString("*")
		}
		b.WriteString(strings.Join(r.GraphQL.Operations, ","))
		if len(r.GraphQL.Fields) > 0 {
			b.WriteByte(':')
			b.WriteString(strings.Join(r.GraphQL.Fields, ","))
		}
	}
	if r.MCP != nil {
		b.WriteString(" mcp=")
		if len(r.MCP.Tools) == 0 {
			b.WriteString("*")
		}
		b.WriteString(strings.Join(r.MCP.Tools, ","))
	}
	if r.Blocked {
		b.WriteString(BlockedSuffix)
	}
	return b.String()
}

// FromLegacy converts a route stored before routes named host patterns
// (ADR 26-10-27-880 §8) without narrowing or widening it: a host covered
// itself and every host beneath it, so h becomes [h, *.h], and `*` stays `*`;
// a path was a prefix on whole segments, so p becomes p/**, and no path (or
// "/") stays no path. A legacy path holding `*` was literal and would now be
// a glob, so it is refused rather than widened.
func FromLegacy(host, path string, blocked bool) (Route, error) {
	host = hostscope.Normalize(host)
	route := Route{Hosts: []string{host}, Blocked: blocked}
	if host != AnyHost {
		if strings.Contains(host, "*") {
			return Route{}, fmt.Errorf("legacy route host %q is not a host name", host)
		}
		route.Hosts = append(route.Hosts, "*."+host)
	}
	path = strings.TrimSuffix(strings.TrimSpace(path), "/")
	if path != "" {
		if strings.Contains(path, "*") {
			return Route{}, fmt.Errorf("legacy route path %q holds *, which a route now reads as a glob", path)
		}
		route.Paths = []string{path + "/**"}
	}
	return Normalize(route)
}

// hasControl reports whether text holds a control character. A route is
// shown to the person who approves or reads it, so nothing in it may move
// their terminal; no host or path needs one.
func hasControl(text string) bool {
	return strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) })
}
