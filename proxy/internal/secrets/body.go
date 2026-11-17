package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// A body place is a JSON pointer into a request's JSON body that a route of
// the client's credentials names (secretroute.Body; #1012, ADR 26-11-11-080
// §9). A sentinel is swapped in a body only there, and only when it is the
// whole of the JSON string there: never inside a longer string, a member
// name, or anywhere a route does not name. Everything else in the body is
// sent as the client wrote it, byte for byte.

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
const maxBodyDepth = secretroute.MaxMessageDepth

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
	doc, err := secretroute.ParseDocument(body)
	if err != nil {
		return BodyUses{}, ErrBodyNotJSON
	}
	var uses []bodyUse
	for _, str := range doc.Strings() {
		// Only a string written literally as the sentinel counts: one spelled
		// with escapes is sent as written, like one anywhere else.
		literal := str.End-str.Start == len(str.Value)+2 && string(body[str.Start+1:str.End-1]) == str.Value
		if str.Name || !literal || !slices.Contains(places, str.At) || !slices.Contains(sentinels, str.Value) {
			continue
		}
		uses = append(uses, bodyUse{Place: Place{Sentinel: str.Value, At: str.At}, start: str.Start, end: str.End})
	}
	return BodyUses{body: body, uses: uses}, nil
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
