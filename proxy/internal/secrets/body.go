package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A body place is a JSON pointer into a request's JSON body that a route of
// the client's credentials names (secretroute.Body; #1012, ADR 26-11-11-080
// §9). A sentinel is swapped in a body only there, and only when it is the
// whole of the JSON string there: never inside a longer string, a member
// name, or anywhere a route does not name. Everything else in the body is
// sent as the client wrote it, byte for byte.

// BodyPlacesRequest asks which body places a client's routes name for a
// request, before its body is read.
type BodyPlacesRequest struct {
	ClientID string
	Method   string
	// Host is the destination without its port, as AuthorizeRequest states it.
	Host string
	URL  string
}

// Place is one sentinel found at one body place, as it is authorized.
type Place struct {
	Sentinel string
	// At is the JSON pointer the sentinel was found at.
	At string
}

// BodyUses is what MatchBody found in a body: each sentinel standing alone
// at a place, and where it is written, to swap it there.
type BodyUses struct {
	body []byte
	uses []bodyUse
}

type bodyUse struct {
	Place
	// start and end are the JSON string's bytes in the body, quotes included.
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

// maxBodyDepth bounds how deeply a body is walked for its places.
const maxBodyDepth = 64

// BodyPlaces are the body places the resolver says clientID's routes name for
// a request. None, for nearly every request, is no reason to read the body.
func (s *Swapper) BodyPlaces(ctx context.Context, req BodyPlacesRequest) []string {
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
func (s *Swapper) applyBody(ctx context.Context, st *settings, clientID string, at destination, uses BodyUses, res *Result) ([]byte, bool) {
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
		var encoded bytes.Buffer
		enc := json.NewEncoder(&encoded)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(value); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("encode the value for %s: %v", use.At, err))
			continue
		}
		out.Write(uses.body[last:use.start])
		out.Write(bytes.TrimSuffix(encoded.Bytes(), []byte("\n")))
		last = use.end
		res.BodyPlaces = append(res.BodyPlaces, use.At)
	}
	if len(res.BodyPlaces) == 0 {
		return nil, false
	}
	out.Write(uses.body[last:])
	return out.Bytes(), true
}
