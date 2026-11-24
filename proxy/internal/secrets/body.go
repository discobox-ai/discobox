package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// A body place is a JSON pointer into a request's JSON body, or a field of its
// URL-encoded form body, that a route of the client's credentials names
// (secretroute.Body; #1012, #1577, ADR 26-11-11-080 §9). A sentinel is
// swapped in a body only there, and only when it is the whole of the JSON
// string or the field's decoded value there: never inside a longer string, a
// member or field name, or anywhere a route does not name. Everything else in
// the body is sent as the client wrote it, byte for byte.

// PlacesRequest asks what places a client's routes name for a request,
// before its body is read: places in its body (BodyPlaces), or in the
// messages of the WebSocket connection it upgrades (InspectsMessages).
type PlacesRequest struct {
	ClientID string
	Method   string
	// Host is the destination without its port, as AuthorizeRequest states it.
	Host string
	URL  string
}

// Place is one sentinel found at one body place, as it is authorized.
type Place struct {
	Sentinel string
	// At is the body place the sentinel was found at: a JSON pointer, or a
	// form place (secretroute.FormPlace).
	At string
}

// BodyUses is what MatchBody or MatchForm found in a body: each sentinel
// standing alone at a place, and where it is written, to swap it there.
type BodyUses struct {
	body []byte
	uses []bodyUse
	// form is a URL-encoded form body, whose values are written
	// percent-encoded rather than as JSON strings.
	form bool
}

type bodyUse struct {
	Place
	// start and end are the value's bytes in the body: the JSON string,
	// quotes included, or the form field's encoded value.
	start, end int
}

// Places are the uses as AuthorizeRequest carries them.
func (b BodyUses) Places() []Place {
	out := make([]Place, 0, len(b.uses))
	for _, use := range b.uses {
		out = append(out, use.Place)
	}
	return out
}

// Raw is the body MatchBody read, as the client sent it.
func (b BodyUses) Raw() []byte { return b.body }

// Empty reports whether no sentinel was found at a place.
func (b BodyUses) Empty() bool { return len(b.uses) == 0 }

// ErrBodyNotJSON is a body that carries a sentinel of the client's but is not
// JSON a place can be read in: not JSON, not UTF-8, nested past
// maxBodyDepth, or naming a member twice in one object, which leaves a
// pointer two places. The proxy refuses such a request rather than guess
// which one the upstream reads.
var ErrBodyNotJSON = errors.New("the body carries a credential's sentinel but is not JSON a body place can be read in")

// ErrBodyNotForm is a URL-encoded form body that carries a sentinel of the
// client's but cannot be read for its form places as every upstream would
// read it: a pair that does not unescape or holds a ";" (which some servers
// split pairs on), or a field a place names written more than once, in any
// case. The
// proxy refuses such a request rather than guess which one the upstream
// reads.
var ErrBodyNotForm = errors.New("the body carries a credential's sentinel but is not a URL-encoded form a body place can be read in")

// maxBodyDepth bounds how deeply a body is walked for its places.
const maxBodyDepth = 64

// BodyPlaces are the body places the resolver says clientID's routes name for
// a request. None, for nearly every request, is no reason to read the body.
func (s *Swapper) BodyPlaces(ctx context.Context, req PlacesRequest) []string {
	if s.resolver == nil || !s.Active(req.ClientID) {
		return nil
	}
	return s.resolver.BodyPlaces(ctx, req)
}

// MatchBody finds clientID's sentinels at places in body, which is the request
// body as the client sent it, decoded. A sentinel counts only where it is
// written literally as a whole JSON string at one of places. A body that holds
// none of the client's sentinels anywhere is not parsed at all; one that holds
// one and cannot be walked is ErrBodyNotJSON.
func (s *Swapper) MatchBody(clientID string, body []byte, places []string) (BodyUses, error) {
	if len(places) == 0 || !s.Active(clientID) {
		return BodyUses{}, nil
	}
	sentinels := s.settings.Load().sentinels[clientID]
	if !slices.ContainsFunc(sentinels, func(sentinel string) bool { return bytes.Contains(body, []byte(sentinel)) }) {
		return BodyUses{}, nil
	}
	if !utf8.Valid(body) {
		return BodyUses{}, ErrBodyNotJSON
	}
	uses, err := walkBody(body, func(at string, raw []byte) (string, bool) {
		if !slices.Contains(places, at) || len(raw) < 2 {
			return "", false
		}
		sentinel := string(raw[1 : len(raw)-1])
		return sentinel, slices.Contains(sentinels, sentinel)
	})
	if err != nil {
		return BodyUses{}, err
	}
	return BodyUses{body: body, uses: uses}, nil
}

// MatchForm finds clientID's sentinels at form places (secretroute.FormPlace)
// in body, which is a URL-encoded form body as the client sent it, decoded of
// any Content-Encoding. A sentinel counts only as the whole decoded value of a
// field a place names. A body in which none of the client's sentinels
// appears, written as it is or percent-encoded, is not judged at all; one
// that carries one and cannot be read for its places is ErrBodyNotForm.
func (s *Swapper) MatchForm(clientID string, body []byte, places []string) (BodyUses, error) {
	if len(places) == 0 || !s.Active(clientID) {
		return BodyUses{}, nil
	}
	sentinels := s.settings.Load().sentinels[clientID]
	type pair struct {
		name, value string
		start, end  int
		bad         bool
	}
	var (
		pairs   []pair
		carries bool
	)
	holds := func(text string) bool {
		return slices.ContainsFunc(sentinels, func(sentinel string) bool { return strings.Contains(text, sentinel) })
	}
	for start := 0; start <= len(body); {
		end := bytes.IndexByte(body[start:], '&')
		if end < 0 {
			end = len(body)
		} else {
			end += start
		}
		raw := string(body[start:end])
		if raw != "" {
			rawName, rawValue, _ := strings.Cut(raw, "=")
			name, nameErr := url.QueryUnescape(rawName)
			value, valueErr := url.QueryUnescape(rawValue)
			p := pair{name: name, value: value, start: end - len(rawValue), end: end,
				bad: nameErr != nil || valueErr != nil || strings.Contains(raw, ";")}
			carries = carries || holds(raw) || holds(name) || holds(value)
			pairs = append(pairs, p)
		}
		start = end + 1
	}
	if !carries {
		return BodyUses{}, nil
	}
	// A field is matched by its exact name, but counted by its name in any
	// case: a server that reads names without case (ASP.NET's Request.Form)
	// would join client_secret and CLIENT_SECRET into one value. Folded
	// through upper and then lower case, so a rune that only one of them
	// joins (ſ upper-cases to S) is counted with what it joins.
	fields, folded := map[string]bool{}, map[string]bool{}
	for _, place := range places {
		if name, ok := secretroute.FormPlace(place); ok {
			fields[name] = true
			folded[foldName(name)] = true
		}
	}
	seen := map[string]bool{}
	var uses []bodyUse
	for _, p := range pairs {
		if p.bad {
			return BodyUses{}, ErrBodyNotForm
		}
		fold := foldName(p.name)
		if !folded[fold] {
			continue
		}
		if seen[fold] {
			return BodyUses{}, ErrBodyNotForm
		}
		seen[fold] = true
		if fields[p.name] && slices.Contains(sentinels, p.value) {
			uses = append(uses, bodyUse{Place: Place{Sentinel: p.value, At: secretroute.FormPlacePrefix + p.name}, start: p.start, end: p.end})
		}
	}
	return BodyUses{body: body, uses: uses, form: true}, nil
}

// foldName is a form field's name as MatchForm counts it, in any case.
func foldName(name string) string { return strings.ToLower(strings.ToUpper(name)) }

// walkBody walks a JSON document, calling found with the pointer and raw
// bytes (quotes included) of every string value, and keeping those it names
// a sentinel for.
func walkBody(body []byte, found func(at string, raw []byte) (string, bool)) ([]bodyUse, error) {
	type frame struct {
		object    bool
		expectKey bool
		segment   string
		index     int
		keys      map[string]struct{}
	}
	var (
		stack []*frame
		uses  []bodyUse
	)
	pointer := func() string {
		var b strings.Builder
		for _, f := range stack {
			b.WriteByte('/')
			if f.object {
				b.WriteString(f.segment)
			} else {
				b.WriteString(strconv.Itoa(f.index))
			}
		}
		return b.String()
	}
	afterValue := func() {
		if len(stack) == 0 {
			return
		}
		if top := stack[len(stack)-1]; top.object {
			top.expectKey = true
		} else {
			top.index++
		}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	values := 0
	for {
		before := dec.InputOffset()
		token, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, ErrBodyNotJSON
		}
		if len(stack) == 0 {
			if values > 0 {
				return nil, ErrBodyNotJSON
			}
			values++
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.object && top.expectKey {
			if token == json.Delim('}') {
				stack = stack[:len(stack)-1]
				afterValue()
				continue
			}
			key, _ := token.(string)
			if _, twice := top.keys[key]; twice {
				return nil, ErrBodyNotJSON
			}
			top.keys[key] = struct{}{}
			top.segment = strings.NewReplacer("~", "~0", "/", "~1").Replace(key)
			top.expectKey = false
			continue
		}
		switch token {
		case json.Delim('{'), json.Delim('['):
			if len(stack) >= maxBodyDepth {
				return nil, ErrBodyNotJSON
			}
			object := token == json.Delim('{')
			stack = append(stack, &frame{object: object, expectKey: object, keys: map[string]struct{}{}})
			continue
		case json.Delim(']'):
			stack = stack[:len(stack)-1]
			afterValue()
			continue
		}
		if _, ok := token.(string); ok {
			end := int(dec.InputOffset())
			start := int(before) + bytes.IndexByte(body[before:end], '"')
			if sentinel, ok := found(pointer(), body[start:end]); ok {
				uses = append(uses, bodyUse{Place: Place{Sentinel: sentinel, At: pointer()}, start: start, end: end})
			}
		}
		afterValue()
	}
	if values == 0 || len(stack) > 0 {
		return nil, ErrBodyNotJSON
	}
	return uses, nil
}

// applyBody resolves each use and writes its value into the body as a JSON
// string, leaving a sentinel that does not resolve in place. It returns the
// body to send and whether anything in it changed.
// Each value written is also added to values, when it is not nil.
func (s *Swapper) applyBody(ctx context.Context, st *settings, clientID string, at destination, uses BodyUses, res *Result, values *[]string) ([]byte, bool) {
	if uses.Empty() {
		return nil, false
	}
	var out bytes.Buffer
	last := 0
	for _, use := range uses.uses {
		value, ok := s.resolve(ctx, st, clientID, use.Sentinel, at, res)
		if !ok {
			continue
		}
		encoded := encodeJSON(value)
		if uses.form {
			encoded = []byte(url.QueryEscape(value))
		}
		if encoded == nil {
			res.Errors = append(res.Errors, "encode the value for "+use.At)
			continue
		}
		if values != nil {
			*values = append(*values, value)
		}
		out.Write(uses.body[last:use.start])
		out.Write(encoded)
		last = use.end
		res.BodyPlaces = append(res.BodyPlaces, use.At)
	}
	if len(res.BodyPlaces) == 0 {
		return nil, false
	}
	out.Write(uses.body[last:])
	return out.Bytes(), true
}

// encodeJSON is value as a JSON string, HTML characters unescaped; nil when
// it cannot be encoded.
func encodeJSON(value string) []byte {
	var encoded bytes.Buffer
	enc := json.NewEncoder(&encoded)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil
	}
	return bytes.TrimSuffix(encoded.Bytes(), []byte("\n"))
}
