package secretroute

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/pkg/hostscope"
)

// WebSocket holds a route to places in the messages a client sends on the
// WebSocket connection the route's request upgrades (ADR 26-11-06-442): the
// credential is carried there, as the whole of the JSON string at one of
// Places, and swapped there. A route with a WebSocket block grants those
// places and nothing else: it is no route for a credential in the
// handshake's headers or body, and a route without one is no route for a
// credential in a message, `*` included. A blocked one refuses the
// credential at its places.
type WebSocket struct {
	// Places are where in a message the credential may be. None is refused:
	// a WebSocket block names where.
	Places []MessagePlace `json:"places"`
}

// MessagePlace is one place in a client's JSON text message: the string at
// At, in a message whose top-level members hold every condition of When.
type MessagePlace struct {
	// At is an RFC 6901 JSON pointer into the message, such as "/d/token".
	At string `json:"at"`
	// When are top-level member names and the JSON scalar (string, number,
	// true, false or null) each must hold, compared exactly: strings as
	// decoded, numbers as exact decimals (2 and 2.0 are equal). A missing
	// member is not null. None is every message.
	When map[string]json.RawMessage `json:"when,omitempty"`
}

// The bounds of one WebSocket block, so a route stays small enough to show a
// person and to prove contained.
const (
	maxMessagePlaces     = 64
	maxPlaceConditions   = 16
	maxMessagePlaceBytes = 1024
)

// normalizeWebSocket validates a WebSocket block and returns it canonical:
// each condition's value in its one spelling (canonicalScalar), and the
// places sorted and deduplicated.
func normalizeWebSocket(ws *WebSocket) (*WebSocket, error) {
	if len(ws.Places) == 0 {
		return nil, fmt.Errorf("a route's WebSocket block must name at least one message place")
	}
	if len(ws.Places) > maxMessagePlaces {
		return nil, fmt.Errorf("a route's WebSocket block names more than %d message places", maxMessagePlaces)
	}
	out := &WebSocket{}
	for _, place := range ws.Places {
		if err := validateMessagePointer(place.At); err != nil {
			return nil, err
		}
		if len(place.When) > maxPlaceConditions {
			return nil, fmt.Errorf("route message place %s has more than %d conditions", Safe(place.At), maxPlaceConditions)
		}
		size := len(place.At)
		next := MessagePlace{At: place.At}
		for name, raw := range place.When {
			if !utf8.ValidString(name) || hasControl(name) {
				return nil, fmt.Errorf("route message place %s has a condition on %s, which is not a printable member name", Safe(place.At), Safe(name))
			}
			scalar, err := canonicalScalar(bytes.TrimSpace(raw))
			if err != nil {
				return nil, fmt.Errorf("route message place %s: condition %s: %w", Safe(place.At), Safe(name), err)
			}
			if hasControl(scalar) {
				return nil, fmt.Errorf("route message place %s: condition %s holds a control character", Safe(place.At), Safe(name))
			}
			if next.When == nil {
				next.When = map[string]json.RawMessage{}
			}
			next.When[name] = json.RawMessage(scalar)
			size += len(name) + len(scalar)
		}
		if size > maxMessagePlaceBytes {
			return nil, fmt.Errorf("route message place %s is longer than %d bytes with its conditions", Safe(place.At), maxMessagePlaceBytes)
		}
		out.Places = append(out.Places, next)
	}
	slices.SortFunc(out.Places, func(a, b MessagePlace) int { return strings.Compare(placeKey(a), placeKey(b)) })
	out.Places = slices.CompactFunc(out.Places, func(a, b MessagePlace) bool { return placeKey(a) == placeKey(b) })
	return out, nil
}

// placeKey is a normalized place as one comparable string.
func placeKey(place MessagePlace) string {
	return string(marshalPlaces([]MessagePlace{place}))
}

// validateMessagePointer checks one message place: a nonempty RFC 6901 JSON
// pointer, every "~" in it "~0" or "~1", without the append index "-" or
// a segment that reads as a glob ("*", "**"). It is taken as written: a
// pointer names one exact place, and an array index only in its canonical
// decimal.
func validateMessagePointer(at string) error {
	if !strings.HasPrefix(at, "/") {
		return fmt.Errorf("route message place %s must be a JSON pointer starting with /", Safe(at))
	}
	if !utf8.ValidString(at) || hasControl(at) {
		return fmt.Errorf("route message place %s must be printable", Safe(at))
	}
	for i := 0; i < len(at); i++ {
		if at[i] == '~' && (i+1 >= len(at) || (at[i+1] != '0' && at[i+1] != '1')) {
			return fmt.Errorf("route message place %s has a ~ that is not ~0 or ~1", Safe(at))
		}
	}
	for segment := range strings.SplitSeq(at[1:], "/") {
		switch segment {
		case "-":
			return fmt.Errorf("route message place %s names the append index -, which is no place in a message", Safe(at))
		case "*", "**":
			return fmt.Errorf("route message place %s has a %s segment: a place is one exact pointer, with no wildcards", Safe(at), segment)
		}
	}
	return nil
}

// marshalPlaces is places as compact JSON, conditions in key order, HTML
// characters unescaped: the typed form's ws= value.
func marshalPlaces(places []MessagePlace) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(places); err != nil {
		// Places are strings and raw scalars Normalize read; an unnormalized
		// condition that is not JSON is shown as nothing rather than as text
		// that would read back as another route.
		return []byte("[]")
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// wsWord finds the ws= word of a typed route: "ws=" at the start of a word.
var wsWord = regexp.MustCompile(`(^|\s)ws=`)

// cutWebSocket takes the ws= word out of a typed route, reading its value as
// one JSON array whatever whitespace is inside it, and returns the text
// without it. No ws= word is a nil block.
func cutWebSocket(text string) (string, *WebSocket, error) {
	loc := wsWord.FindStringIndex(text)
	if loc == nil {
		return text, nil, nil
	}
	start := loc[1]
	if start >= len(text) || text[start] != '[' {
		return "", nil, fmt.Errorf("ws=PLACES needs a JSON array of places, such as ws=[{\"at\":\"/token\"}]")
	}
	dec := json.NewDecoder(strings.NewReader(text[start:]))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return "", nil, fmt.Errorf("ws=PLACES is not a JSON array: %w", err)
	}
	end := start + int(dec.InputOffset())
	if end < len(text) && !strings.ContainsRune(" \t\n\r", rune(text[end])) {
		return "", nil, fmt.Errorf("ws=PLACES must end its word")
	}
	if _, err := ParseDocument(raw); err != nil {
		return "", nil, fmt.Errorf("ws=PLACES: %w", err)
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	var places []MessagePlace
	if err := strict.Decode(&places); err != nil {
		return "", nil, fmt.Errorf("ws=PLACES must be an array of {\"at\": POINTER, \"when\": {NAME: SCALAR}}: %w", err)
	}
	rest := text[:loc[0]] + " " + text[end:]
	if wsWord.MatchString(rest) {
		return "", nil, fmt.Errorf("a route has one ws=")
	}
	return rest, &WebSocket{Places: places}, nil
}

// allows reports whether a place lets the credential be at at in m: its
// pointer is at, and m's top-level members hold every condition.
func (p MessagePlace) allows(at string, m *Message) bool {
	if p.At != at {
		return false
	}
	for name, scalar := range p.When {
		if !m.holds(name, string(scalar)) {
			return false
		}
	}
	return true
}

// Describe is a place in plain words, for a person approving it: where the
// credential goes, and in which messages.
//
// The pointer and every condition's member name are JSON-quoted, as the
// value already is: a name may hold spaces and words such as "is" and "and",
// and shown bare it could read as conditions the place does not have.
func (p MessagePlace) Describe() string {
	at := encodeJSONString(p.At)
	if len(p.When) == 0 {
		return "the string at " + at + " in any message"
	}
	names := make([]string, 0, len(p.When))
	for name := range p.When {
		names = append(names, name)
	}
	slices.Sort(names)
	conditions := make([]string, 0, len(names))
	for _, name := range names {
		conditions = append(conditions, fmt.Sprintf("%s is %s", encodeJSONString(name), p.When[name]))
	}
	return "the string at " + at + " in messages where " + strings.Join(conditions, " and ")
}

// messageAllowed reports whether some place of a block lets the credential be
// at at in m, and, when none does, why not, after "route X".
func (ws *WebSocket) messageAllowed(at string, m *Message) (bool, string) {
	named := false
	for _, place := range ws.Places {
		if place.allows(at, m) {
			return true, ""
		}
		named = named || place.At == at
	}
	if named {
		return false, "names the message place " + Safe(at) + " only in messages this one is not (its conditions do not hold)"
	}
	return false, "does not name the message place " + Safe(at)
}

// placeContained reports whether a child place fits a parent: the same
// pointer, and every parent condition among the child's with an equal value,
// so the child's messages are some of the parent's.
func placeContained(child, parent MessagePlace) bool {
	if child.At != parent.At {
		return false
	}
	for name, scalar := range parent.When {
		if got, ok := child.When[name]; !ok || !bytes.Equal(got, scalar) {
			return false
		}
	}
	return true
}

// placesOverlap reports whether some message could be at both places: the
// same pointer, and no condition one names that the other names otherwise.
func placesOverlap(a, b MessagePlace) bool {
	if a.At != b.At {
		return false
	}
	for name, scalar := range a.When {
		if other, ok := b.When[name]; ok && !bytes.Equal(other, scalar) {
			return false
		}
	}
	return true
}

// WebSocketCandidate reports whether some open route of a grant live at now
// has a WebSocket block and covers the upgrade request req: its host and path
// match, and its methods, when it has any, list req's request-line method. It
// is what tells the proxy to inspect the connection's messages (ADR
// 26-11-06-442 §3), and nothing more: it need not pass every list, since a
// connection not inspected gets no message swapped, and whatever is found in
// a message is decided by DecideGrants.
func WebSocketCandidate(grants []Grant, now time.Time, req Request) bool {
	host := hostscope.Normalize(req.Host)
	method := strings.ToUpper(req.Method)
	paths := RequestPaths(req.URL)
	for _, grant := range grants {
		if !grant.Live(now) {
			continue
		}
		for _, routes := range grant.Lists {
			for _, route := range routes {
				if route.Blocked || route.WebSocket == nil {
					continue
				}
				if _, ok := hostScore(route.Hosts, host); !ok {
					continue
				}
				if len(route.Methods) > 0 && !slices.Contains(route.Methods, method) {
					continue
				}
				if slices.ContainsFunc(paths, func(p string) bool { _, ok := pathScore(route.Paths, p); return ok }) {
					return true
				}
			}
		}
	}
	return false
}
