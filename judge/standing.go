package judge

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// MaxStanding is the longest an allow may stand, whatever the judge names
// (ADR 26-09-25-428 §2). A standing allow is a model's judgment about requests
// it has not read, so it covers a burst of work and not a session.
const MaxStanding = 15 * time.Minute

// Standing is an allow the judge asked to let stand: every request matching
// Route, for Seconds, is allowed without asking again (ADR 26-09-25-428).
//
// The judge proposes it and Discobox decides whether it stands: see
// Job.Admits. The host, the discobox, and the use are never the judge's to
// name — a standing allow covers the ones the answer that granted it was
// about.
type Standing struct {
	// Route is a net/http pattern: one method, a space, and an absolute path
	// whose segments may be {name} or, last, {name...}.
	Route string `json:"route"`
	// Seconds is how long it stands, as the judge asked. Duration caps it.
	Seconds int `json:"seconds"`
}

// Duration is how long this allow stands: what the judge asked for, never
// more than MaxStanding, and nothing at all for no time. Job.Admits refuses a
// route that would stand for nothing, so zero is never recorded; it is zero
// here rather than the cap so that a caller who skipped Admits fails short.
func (s Standing) Duration() time.Duration {
	if s.Seconds <= 0 {
		return 0
	}
	// Compared in seconds before multiplying: a number of seconds large
	// enough to overflow a Duration would otherwise wrap to a short one that
	// passes the cap.
	if int64(s.Seconds) > int64(MaxStanding/time.Second) {
		return MaxStanding
	}
	return time.Duration(s.Seconds) * time.Second
}

// Route is a parsed standing route. It matches a method and a path, and
// nothing else: the query and the body are not part of what it names, which
// is why the judge is told never to let an allow stand on a route whose
// operation lives in either.
type Route struct {
	method   string
	segments []routeSegment
}

type routeSegment struct {
	literal string
	// wild is a {name} segment, which matches any one non-empty segment.
	wild bool
	// rest is a final {name...} segment, which matches whatever is left.
	rest bool
}

// ParseRoute reads a standing route. It accepts the subset of net/http's
// pattern syntax a route needs and refuses the rest, rather than guessing:
// no host, no method-less pattern, no {$}, no implicit prefix match from a
// trailing slash, and no wildcard that is part of a segment.
func ParseRoute(pattern string) (Route, error) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok || method == "" {
		return Route{}, errors.New("a standing route names one method, a space, and a path")
	}
	for _, r := range method {
		if r < 'A' || r > 'Z' {
			return Route{}, fmt.Errorf("a standing route's method is upper-case letters, not %q", method)
		}
	}
	if !strings.HasPrefix(path, "/") {
		return Route{}, errors.New("a standing route's path is absolute, and names no host")
	}
	route := Route{method: method}
	parts := strings.Split(path[1:], "/")
	for i, part := range parts {
		last := i == len(parts)-1
		switch {
		case part == "" && !last:
			return Route{}, errors.New("a standing route has no empty segments")
		case strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}"):
			name := part[1 : len(part)-1]
			rest := strings.HasSuffix(name, "...")
			name = strings.TrimSuffix(name, "...")
			if !wildcardName(name) {
				return Route{}, fmt.Errorf("a standing route's wildcard %q is not {name} or {name...}", part)
			}
			if rest && !last {
				return Route{}, errors.New("only a standing route's last segment matches the rest of the path")
			}
			route.segments = append(route.segments, routeSegment{wild: !rest, rest: rest})
		case strings.ContainsAny(part, "{}"):
			return Route{}, fmt.Errorf("a standing route's wildcard is a whole segment, not part of %q", part)
		case part == "." || part == "..":
			return Route{}, errors.New("a standing route has no dot segments")
		default:
			literal, err := url.PathUnescape(part)
			if err != nil {
				return Route{}, fmt.Errorf("a standing route's segment %q is not a path segment", part)
			}
			route.segments = append(route.segments, routeSegment{literal: literal})
		}
	}
	return route, nil
}

// names reports whether the route has a literal segment that is not empty:
// "GET /" is one path, but "POST /{rest...}" and "GET /{a}/{b}" are every
// path of their shape, which is the host rather than a target on it.
func (r Route) names() bool {
	for _, segment := range r.segments {
		if !segment.wild && !segment.rest && segment.literal != "" {
			return true
		}
	}
	return false
}

func wildcardName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// Matches reports whether a request is one this route covers.
//
// The path is compared segment by segment, each unescaped. A path the
// upstream may resolve to a target the route never named matches nothing, and
// is judged rather than covered (ADR 26-09-25-428 §3): a dot segment, an empty
// one, or a segment that unescapes to a slash or a backslash. The last is not
// a separator here, but plenty of upstreams decode it into one, and
// "..%2F..%2Fadmin" is a single segment a wildcard would otherwise take.
func (r Route) Matches(method, rawURL string) bool {
	if method != r.method {
		return false
	}
	target, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	path := target.EscapedPath()
	if !strings.HasPrefix(path, "/") {
		return false
	}
	parts := strings.Split(path[1:], "/")
	for i, part := range parts {
		segment, err := url.PathUnescape(part)
		if err != nil || segment == "." || segment == ".." || strings.ContainsAny(segment, `/\`) {
			return false
		}
		if segment == "" && i != len(parts)-1 {
			return false
		}
		parts[i] = segment
	}
	for i, want := range r.segments {
		if i >= len(parts) {
			// Even {name...} needs its segment to be there: "/a/{rest...}"
			// covers "/a/" and what is under it, and not "/a" itself.
			return false
		}
		if want.rest {
			return true
		}
		switch {
		case want.wild:
			if parts[i] == "" {
				return false
			}
		case parts[i] != want.literal:
			return false
		}
	}
	return len(parts) == len(r.segments)
}

// Admits reports whether an allow's standing route may stand, which Discobox
// decides and the judge does not (ADR 26-09-25-428 §2): the job was decided on
// its first round, before any body was shown; the route names its target in
// at least one literal segment, so that no route is every path of one method
// at the host; it stands for some time; and it covers the very request it was
// granted on. A route that fails any of these was not derived from the
// evidence, and is dropped while the allow stays.
func (j Job) Admits(standing Standing) (Route, error) {
	if j.Kind != KindRequest || j.Request == nil {
		return Route{}, errors.New("only a request's allow may stand")
	}
	if j.Round != 1 || j.Request.Body.Supplied() {
		return Route{}, errors.New("an allow that needed the body was about that body, and does not stand")
	}
	if standing.Duration() <= 0 {
		return Route{}, errors.New("a standing allow stands for some seconds")
	}
	route, err := ParseRoute(standing.Route)
	if err != nil {
		return Route{}, err
	}
	if !route.names() {
		return Route{}, errors.New("a standing route names its target in at least one literal segment")
	}
	if !route.Matches(j.Request.Method, j.Request.URL) {
		return Route{}, errors.New("the standing route does not cover the request it was granted on")
	}
	return route, nil
}
