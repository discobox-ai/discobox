package secretroute

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/pkg/hostscope"
)

// Request is what a route is matched against. Body is called at most once,
// and only when a candidate route needs it: one with a protocol block, or one
// with methods on a POST whose body may carry a `_method` — a URL-encoded
// form, no Content-Type, or JSON (readBodyMethods). It returns the request
// body (decoded from gzip/deflate/identity) and true, or false when the body
// is past the capture limit, unreadable, in another encoding, or the request
// upgrades the connection. A nil Body is "no body available".
//
// An open method route may ignore a POST body's `_method` only when the body
// is unreadable, never when it is unparsable: when it is JSON and Body
// returns false (or is nil), or multipart, which is not parsed. Such a body
// is caught only by a blocked method route. That is the price of not
// refusing large JSON POSTs, such as LLM requests, and uploads under open
// method routes. A URL-encoded or untyped body Body cannot return, and any
// body read that does not parse or is ambiguous, refuses an open method
// route too.
//
// Header is read for the methods a request names besides its request line
// (methodOverrideHeaders) and for the Content-Type that says whether its body
// may name one; a nil Header names none.
type Request struct {
	Method string
	Host   string // without port, normalized as hostscope does
	URL    string // raw request URL; the path is read with RequestPaths
	Header http.Header
	Body   func() ([]byte, bool)
}

// methodOverrideHeaders are the headers servers read a method from in place
// of the request line's (Google APIs, OData, Rack, Laravel and others). A
// request's method is what the upstream executes, so a request is held to
// every method it names.
var methodOverrideHeaders = []string{"X-Http-Method-Override", "X-Http-Method", "X-Method-Override"}

// methodOverrideParam is the query or form parameter servers read a method
// from (Rack::MethodOverride, Laravel, Rails).
const methodOverrideParam = "_method"

// RequestPaths are the paths a request's URL is matched on, each with its dot
// segments resolved, so a request cannot step out of the path it names
// (/mcp/readonly/../write is /mcp/write). The first is the path decoded. An
// encoded "/" is data to a server that routes on the path as sent and a
// separator to one that decodes it first, and the proxy cannot tell which the
// upstream is, so a path holding one has a second reading that keeps it
// inside its segment: /mcp/x%2F..%2Freadonly is /mcp/readonly decoded and
// /mcp/x%2F..%2Freadonly as sent. A request is held to every reading. A URL
// that does not parse has the root path.
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

// Decide says whether a request may carry a secret, held to every route list
// it is under — the secret's, the binding's when it has any, and each
// parent's (ADR 26-10-27-880 §4) — and why not when it may not. Each list is
// read on its own, for every reading of the path (RequestPaths): its
// candidates are the routes whose hosts, methods, paths and protocol block
// all match, the most specific of them decide — host first, then path — and
// a blocked one among those refuses, so a tie with a blocked route refuses.
// No candidate refuses. A list with no routes allows nothing, and so does no
// list at all: a credential is sent where its routes say, and only there.
//
// A protocol block reads the body, and a body it cannot read is never what an
// open route allows and always what a blocked route might refuse: an open
// route's block matches only when every operation or call in the body
// matches it, and a blocked route's block matches when any one does, or when
// the body cannot be read or parsed. A carve-out therefore cannot be stepped
// around by batching, compressing or truncating what it refuses.
//
// Methods are read the same way, with one exception. A request names the
// method on its request line, every method its override headers and its
// URL's `_method` name, and, on a POST only, every `_method` its body carries
// (a URL-encoded form, a body with no Content-Type, or a JSON object), each
// value read counting even from a body that does not parse. An open route
// with methods matches only when it lists every one and nothing is unknown;
// a blocked route with methods when it lists any one, or anything is
// unknown. The exception is unreadable, never unparsable: an open route
// ignores a JSON body Body could not return, and a multipart one, since
// refusing them would refuse every large JSON POST and upload, so a
// `_method` hidden there is caught only by a blocked method route. A form or
// untyped body that cannot be read is small or a bypass, and a body read
// that does not parse or is ambiguous — invalid JSON or UTF-8, a duplicate or
// case-variant JSON `_method`, a non-string one, a form pair that does not
// parse — is unknown to every route.
//
// A refusal's reason says what refused, in a short sentence that never
// carries the body beyond an operation type, a root field, a tool or method
// name, each escaped when it is not plain: "route X refuses WHAT" for a
// blocked route, naming the override that named the method or the body it
// could not read; when no route is a candidate, the most specific open route
// that failed only on its methods or protocol block, as "route X does not
// allow WHAT" or "route X needs the body, which could not be read (...)";
// and "this credential has no route to METHOD host/path" when none came
// that close.
func Decide(lists [][]Route, req Request) (allowed bool, reason string) {
	if len(lists) == 0 {
		return false, "this credential has no routes"
	}
	match := newMatcher(req)
	type candidate struct {
		route  Route
		reason string
	}
	for _, routes := range lists {
		if len(routes) == 0 {
			return false, "this credential has no routes"
		}
		for _, requestPath := range RequestPaths(req.URL) {
			best, decided := rank{}, []candidate(nil)
			nearBest, nearReason := rank{}, ""
			for _, route := range routes {
				specificity, ok, near, why := match.route(route, requestPath)
				if near && !route.Blocked && (nearReason == "" || specificity.compare(nearBest) > 0) {
					nearBest, nearReason = specificity, "route "+Format(route)+" "+why
				}
				if !ok {
					continue
				}
				switch c := specificity.compare(best); {
				case len(decided) == 0 || c > 0:
					best, decided = specificity, []candidate{{route, why}}
				case c == 0:
					decided = append(decided, candidate{route, why})
				}
			}
			where := match.describe(requestPath)
			if len(decided) == 0 {
				if nearReason != "" {
					return false, nearReason
				}
				return false, fmt.Sprintf("this credential has no route to %s", where)
			}
			if i := slices.IndexFunc(decided, func(c candidate) bool { return c.route.Blocked }); i >= 0 {
				what := decided[i].reason
				if what == "" {
					what = where
				}
				return false, fmt.Sprintf("route %s refuses %s", Format(decided[i].route), what)
			}
		}
	}
	return true, ""
}

// HostAllowed reports whether some request to host could carry a secret held
// to lists: in every list, an open route covers the host that outranks every
// host-wide block covering it. A host-wide block — blocked, with no methods,
// no paths and no protocol block — is the most specific candidate for every
// request to the hosts it is most specific for, so a host only it answers for
// is refused here as Decide would refuse every request there. It is the check
// a value is handed out by, which knows the host and nothing else of the
// request; the request itself is held to Decide.
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

// hostOpen reports whether one list leaves some request to host open: an open
// route covers it that outranks every host-wide block covering it — on a more
// specific host pattern, or on as specific a one with a path, which outranks
// the block on the paths it names.
func hostOpen(routes []Route, host string) bool {
	shut, shutFound := 0, false
	for _, route := range routes {
		if !route.Blocked || len(route.Methods) > 0 || len(route.Paths) > 0 || route.GraphQL != nil || route.MCP != nil {
			continue
		}
		if score, ok := hostScore(route.Hosts, host); ok && (!shutFound || score > shut) {
			shut, shutFound = score, true
		}
	}
	for _, route := range routes {
		if route.Blocked {
			continue
		}
		score, ok := hostScore(route.Hosts, host)
		if ok && (!shutFound || score > shut || (score == shut && len(route.Paths) > 0)) {
			return true
		}
	}
	return false
}

// rank is how specifically a route answers for a request: its host first,
// then its path.
type rank struct {
	host int
	path pathRank
}

func (r rank) compare(other rank) int {
	if r.host != other.host {
		return cmp.Compare(r.host, other.host)
	}
	return r.path.compare(other.path)
}

// pathRank is how specific a matching path pattern is: a route with no paths
// is least; otherwise more literal segments before the first wildcard, then
// more segments (a trailing `**` not counted), then without `**`.
type pathRank struct {
	any      bool // the route names no paths
	literal  int
	segments int
	rest     bool // the pattern ends in **
}

func (r pathRank) compare(other pathRank) int {
	switch {
	case r.any != other.any:
		if r.any {
			return -1
		}
		return 1
	case r.literal != other.literal:
		return cmp.Compare(r.literal, other.literal)
	case r.segments != other.segments:
		return cmp.Compare(r.segments, other.segments)
	case r.rest != other.rest:
		if r.rest {
			return -1
		}
		return 1
	}
	return 0
}

// hostScore is how specifically a route's hosts answer for host, and whether
// any covers it: its most specific matching pattern, ranked by label count
// (a `*.suffix` counts its suffix's), an exact name above a wildcard of as
// many labels, and `*` below every other.
func hostScore(patterns []string, host string) (int, bool) {
	best, found := 0, false
	for _, pattern := range patterns {
		score, ok := hostPatternScore(pattern, host)
		if ok && (!found || score > best) {
			best, found = score, true
		}
	}
	return best, found
}

func hostPatternScore(pattern, host string) (int, bool) {
	pattern = hostscope.Normalize(pattern)
	switch {
	case host == "":
		return 0, false
	case pattern == AnyHost:
		return 0, true
	case strings.HasPrefix(pattern, "*."):
		suffix := pattern[2:]
		if !strings.HasSuffix(host, "."+suffix) {
			return 0, false
		}
		return 2 * labels(suffix), true
	case pattern == host:
		return 2*labels(pattern) + 1, true
	}
	return 0, false
}

func labels(name string) int {
	return strings.Count(name, ".") + 1
}

// pathScore is how specifically a route's paths answer for a request path,
// and whether any matches it.
func pathScore(patterns []string, requestPath string) (pathRank, bool) {
	if len(patterns) == 0 {
		return pathRank{any: true}, true
	}
	requestSegments := splitPath(requestPath)
	best, found := pathRank{}, false
	for _, pattern := range patterns {
		segments := splitPath(pattern)
		if !pathMatches(segments, requestSegments) {
			continue
		}
		// A trailing ** matches zero segments, so it never makes a pattern
		// more specific than the same pattern without it: it counts only
		// through rest, which loses a tie.
		r := pathRank{rest: len(segments) > 0 && segments[len(segments)-1] == "**"}
		if r.rest {
			segments = segments[:len(segments)-1]
		}
		r.segments = len(segments)
		r.literal = slices.Index(segments, "*")
		if r.literal < 0 {
			r.literal = len(segments)
		}
		if !found || r.compare(best) > 0 {
			best, found = r, true
		}
	}
	return best, found
}

// splitPath is a path's segments: none for "/".
func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// pathMatches reports whether a pattern's segments match a request path's,
// whole: `*` is exactly one segment, a trailing `**` zero or more, and any
// other segment is literal. Case is ignored, so a server that does not mind
// case cannot be reached around a blocked path by changing it.
func pathMatches(pattern, request []string) bool {
	for i, segment := range pattern {
		if segment == "**" {
			return true
		}
		if i >= len(request) || (segment != "*" && !strings.EqualFold(segment, request[i])) {
			return false
		}
	}
	return len(pattern) == len(request)
}

// matcher matches routes against one request, reading its body at most once.
type matcher struct {
	req    Request
	method string
	host   string

	bodyRead bool
	body     []byte
	bodyOK   bool

	graphQLRead *graphQLRequest
	mcpRead     *mcpRequest

	// The methods the request names: its request line's, its override
	// headers' and its URL's _method (methods, unknown when the URL does not
	// parse), and its body's _method, read only when a route with methods
	// asks (bodyMethods, bodyMethodsState).
	methods          []namedMethod
	methodsUnknown   bool
	bodyMethodsRead  bool
	bodyMethods      []string
	bodyMethodsState bodyState
	bodyNotRead      bool // the body _method is unknown because Body returned false
}

// namedMethod is a method a request names, and what named it: "" for the
// request line.
type namedMethod struct {
	method, by string
}

// bodyMethodParam is what names a body's _method, in a reason.
const bodyMethodParam = "_method in the body"

// unreadableBody is why a route that needs the body does not match one Body
// could not return, after "route X".
const unreadableBody = "needs the body, which could not be read (too large, of undeclared length, or in an encoding the proxy cannot decode)"

func newMatcher(req Request) *matcher {
	m := &matcher{req: req, method: strings.ToUpper(req.Method), host: hostscope.Normalize(req.Host)}
	m.methods = []namedMethod{{method: m.method}}
	for _, name := range methodOverrideHeaders {
		for _, value := range req.Header.Values(name) {
			for _, method := range overrideMethods(value) {
				m.methods = append(m.methods, namedMethod{method: method, by: displayHeader(name)})
			}
		}
	}
	if u, err := url.Parse(req.URL); err != nil {
		m.methodsUnknown = true
	} else if values, err := url.ParseQuery(u.RawQuery); err != nil {
		m.methodsUnknown = true
	} else {
		for _, value := range values[methodOverrideParam] {
			for _, method := range overrideMethods(value) {
				m.methods = append(m.methods, namedMethod{method: method, by: "_method in the URL"})
			}
		}
	}
	return m
}

// displayHeader is an override header's name as its documentation spells it.
func displayHeader(name string) string {
	return strings.ReplaceAll(name, "-Http-", "-HTTP-")
}

// named is a method and what named it, for a reason.
func (n namedMethod) named() string {
	if n.by == "" {
		return safe(n.method)
	}
	return safe(n.method) + ", named by " + n.by
}

// overrideMethods are the methods one override value names: each of its
// non-empty comma-separated items, upper-cased.
func overrideMethods(value string) []string {
	var out []string
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, strings.ToUpper(item))
		}
	}
	return out
}

// methodsMatch reports whether the request matches a route's methods. For an
// open route, every method the request names is one of them; for a blocked
// route, any one is, or what the request names is unknown. The body is read
// (readBodyMethods) only when the request line, headers and URL have not
// decided it. An open route may ignore a body only when it is unreadable,
// never when it is unparsable (bodyUnreadable): a JSON body Body could not
// return, or a multipart one. A blocked route matches either.
//
// The reason says what decided it: for an open route that does not match,
// why not, after "route X"; for a blocked route that matches, what it
// refuses, or "" when the request line's method did.
func (m *matcher) methodsMatch(methods []string, blocked bool) (bool, string) {
	in := func(method string) bool { return slices.Contains(methods, method) }
	if blocked {
		if m.methodsUnknown {
			return true, "a request whose query string does not parse"
		}
		if i := slices.IndexFunc(m.methods, func(n namedMethod) bool { return in(n.method) }); i >= 0 {
			if m.methods[i].by == "" {
				return true, ""
			}
			return true, m.methods[i].named()
		}
		bodyMethods, state := m.readBodyMethods()
		if i := slices.IndexFunc(bodyMethods, in); i >= 0 {
			return true, namedMethod{bodyMethods[i], bodyMethodParam}.named()
		}
		switch {
		case state == bodyUnreadable && m.hasContentType("multipart/"):
			return true, "a multipart POST, whose _method is not read"
		case state != bodyKnown && m.bodyNotRead:
			return true, "a POST whose body could not be read for its _method"
		case state != bodyKnown:
			return true, "a POST whose body's _method could not be parsed"
		}
		return false, ""
	}
	if m.methodsUnknown {
		return false, "cannot read the request's query string for its _method"
	}
	if i := slices.IndexFunc(m.methods, func(n namedMethod) bool { return !in(n.method) }); i >= 0 {
		return false, "does not allow " + m.methods[i].named()
	}
	bodyMethods, state := m.readBodyMethods()
	if i := slices.IndexFunc(bodyMethods, func(method string) bool { return !in(method) }); i >= 0 {
		return false, "does not allow " + namedMethod{bodyMethods[i], bodyMethodParam}.named()
	}
	switch state {
	case bodyKnown:
		return true, ""
	case bodyUnreadable:
		// A body _method the proxy could not read is caught only by a
		// blocked method route: refusing it here would refuse every large
		// JSON POST and every upload.
		return true, ""
	}
	if m.bodyNotRead {
		return false, unreadableBody
	}
	return false, "cannot tell what method the body's _method names: it does not parse, or names one twice"
}

// bodyState is what is known of the _method a request's body carries.
type bodyState int

const (
	// bodyKnown: the body carries no _method, or the ones read.
	bodyKnown bodyState = iota
	// bodyUnreadable: a JSON body Body could not return, or a multipart
	// body, which is not parsed. An open route ignores it, a blocked one
	// matches it.
	bodyUnreadable
	// bodyUnknown: a form or untyped body Body could not return, or a body
	// read that does not parse or is ambiguous. Every route treats it as
	// naming a method it does not list: an open one refuses, a blocked one
	// matches.
	bodyUnknown
)

// readBodyMethods reads the _method a body may carry. Only a POST's body may
// carry one: Symfony (Laravel) and Rack apply a body override only to POST.
// On a POST it may when the body is a URL-encoded form; has no Content-Type,
// which Rack, Rails and Sinatra parse as a form; or is JSON (a Content-Type
// holding "/json" or "+json", as Laravel reads it), whose top-level object's
// _method is read; or is multipart, which is not parsed. Any other body
// names no method, and is not read. Every _method value read counts, even
// from a body that does not parse.
func (m *matcher) readBodyMethods() ([]string, bodyState) {
	if m.bodyMethodsRead {
		return m.bodyMethods, m.bodyMethodsState
	}
	m.bodyMethodsRead = true
	m.bodyMethods, m.bodyMethodsState = m.bodyMethodOverrides()
	return m.bodyMethods, m.bodyMethodsState
}

func (m *matcher) bodyMethodOverrides() ([]string, bodyState) {
	if m.method != http.MethodPost {
		return nil, bodyKnown
	}
	// Any multipart type, not only form-data: Rack parses multipart/mixed and
	// multipart/related too, and its method override reads `_method` from them.
	if m.hasContentType("multipart/") {
		return nil, bodyUnreadable
	}
	contentTypes := slices.DeleteFunc(slices.Clone(m.req.Header.Values("Content-Type")), func(v string) bool { return strings.TrimSpace(v) == "" })
	form := m.hasContentType("application/x-www-form-urlencoded") || len(contentTypes) == 0
	isJSON := m.hasContentType("/json") || m.hasContentType("+json")
	if !form && !isJSON {
		return nil, bodyKnown
	}
	body, ok := m.readBody()
	if !ok {
		m.bodyNotRead = true
		// A form is small, and an unreadable one (chunked, past the limit)
		// is the cheapest way around an open POST route on a Rack app; only
		// JSON, which large API requests are, may go unread.
		if form {
			return nil, bodyUnknown
		}
		return nil, bodyUnreadable
	}
	var methods []string
	state := bodyKnown
	if form {
		formMethods, ok := formMethodOverride(string(body))
		methods = append(methods, formMethods...)
		if !ok {
			state = bodyUnknown
		}
	}
	if isJSON {
		jsonMethods, ok := jsonMethodOverride(body)
		methods = append(methods, jsonMethods...)
		if !ok {
			state = bodyUnknown
		}
	}
	return methods, state
}

// formMethodOverride reads every _method of a URL-encoded body, false when
// the body does not parse as url.ParseQuery reads it. Pairs are split on "&"
// and ";", as one server or another splits them, and a key or value that
// does not unescape is read as sent, so a pair a parser refuses still names
// its method.
func formMethodOverride(body string) ([]string, bool) {
	_, err := url.ParseQuery(body)
	var methods []string
	for pair := range strings.FieldsFuncSeq(body, func(r rune) bool { return r == '&' || r == ';' }) {
		key, value, _ := strings.Cut(pair, "=")
		if unescaped, err := url.QueryUnescape(key); err == nil {
			key = unescaped
		}
		if key != methodOverrideParam {
			continue
		}
		if unescaped, err := url.QueryUnescape(value); err == nil {
			value = unescaped
		}
		methods = append(methods, overrideMethods(value)...)
	}
	return methods, err == nil
}

// jsonMethodOverride reads every _method of a JSON body's top-level object —
// every key that folds to _method, as a case-insensitive decoder reads it,
// each value seen — false when the body does not parse, holds more than one
// such key, or one whose value is neither a string nor null. An empty body,
// one that is not an object, and a null _method name none.
func jsonMethodOverride(body []byte) ([]string, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, true
	}
	if !utf8.Valid(trimmed) || !json.Valid(trimmed) {
		return nil, false
	}
	if trimmed[0] != '{' {
		return nil, true
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	if _, err := decoder.Token(); err != nil {
		return nil, false
	}
	var methods []string
	ok, seen := true, 0
	want := foldKey(methodOverrideParam)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return methods, false
		}
		key, _ := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return methods, false
		}
		if foldKey(key) != want {
			continue
		}
		seen++
		var method string
		switch {
		case string(value) == "null":
		case json.Unmarshal(value, &method) == nil:
			methods = append(methods, overrideMethods(method)...)
		default:
			ok = false
		}
	}
	return methods, ok && seen <= 1
}

// hasContentType reports whether any Content-Type the request carries names
// mediaType, however it is spelled or parameterized.
func (m *matcher) hasContentType(mediaType string) bool {
	return slices.ContainsFunc(m.req.Header.Values("Content-Type"), func(value string) bool {
		return strings.Contains(strings.ToLower(value), mediaType)
	})
}

// route reports whether a route is a candidate for the request at one reading
// of its path, and how specifically it answers for it. The body is read last,
// only for a route whose hosts and paths match and whose methods, when it has
// any, are not decided without it. near is a route whose hosts and paths
// match and that fails on its methods or protocol block; reason is why, after
// "route X". For a blocked candidate, reason is what it refuses, or "" for
// the request as described.
func (m *matcher) route(route Route, requestPath string) (r rank, candidate, near bool, reason string) {
	host, ok := hostScore(route.Hosts, m.host)
	if !ok {
		return rank{}, false, false, ""
	}
	pathRank, ok := pathScore(route.Paths, requestPath)
	if !ok {
		return rank{}, false, false, ""
	}
	r = rank{host: host, path: pathRank}
	if len(route.Methods) > 0 {
		ok, why := m.methodsMatch(route.Methods, route.Blocked)
		if !ok {
			return r, false, true, why
		}
		reason = why
	}
	if route.GraphQL != nil {
		ok, why := m.graphQL(route.GraphQL, route.Blocked)
		if !ok {
			return r, false, true, why
		}
		if reason == "" {
			reason = why
		}
	}
	if route.MCP != nil {
		ok, why := m.mcp(route.MCP, route.Blocked)
		if !ok {
			return r, false, true, why
		}
		if reason == "" {
			reason = why
		}
	}
	return r, true, false, reason
}

// readBody calls the request's Body once, and is false for a nil one.
func (m *matcher) readBody() ([]byte, bool) {
	if !m.bodyRead {
		m.bodyRead = true
		if m.req.Body != nil {
			m.body, m.bodyOK = m.req.Body()
		}
	}
	return m.body, m.bodyOK
}

// describe names the request at one reading of its path, for a refusal.
func (m *matcher) describe(requestPath string) string {
	if m.method == "" {
		return safe(m.host + requestPath)
	}
	return safe(m.method) + " " + safe(m.host+requestPath)
}

// safe is text from a request as a reason may show it: as it is when it is
// printable and has no space, quoted with Go's escapes otherwise. A reason
// reaches a terminal, so nothing from a request may move it.
func safe(text string) string {
	// A request names these, so a request bounds their length only by the
	// body limit: a megabyte tool name would become a megabyte reason in the
	// sandbox's answer, the audit row and every view of it.
	if runes := []rune(text); len(runes) > reasonNameLimit {
		text = string(runes[:reasonNameLimit]) + "…"
	}
	if quoted := strconv.Quote(text); quoted == `"`+text+`"` && !strings.ContainsAny(text, " \t") && text != "" {
		return text
	}
	return strconv.QuoteToASCII(text)
}

// reasonNameLimit is the most runes of a request-derived name (a tool, a root
// field, a method, a host and path) a reason carries before it is cut short.
const reasonNameLimit = 128

// nameMatches reports whether a name matches a pattern: the name itself, or,
// for a pattern ending in `*`, any name it prefixes.
func nameMatches(pattern, name string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(name, prefix)
	}
	return pattern == name
}

// anyNameMatches reports whether name matches a pattern of a list, an empty
// list matching every name.
func anyNameMatches(patterns []string, name string) bool {
	return len(patterns) == 0 || slices.ContainsFunc(patterns, func(p string) bool { return nameMatches(p, name) })
}
