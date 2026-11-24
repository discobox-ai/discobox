package secrets

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// A credential may be carried in a message a client sends on a WebSocket
// connection (ADR 26-11-06-442): only as the whole of a string at a place a
// route names, in a JSON text message. The proxy reads such a message whole
// before any of it is sent (proxy/wsinspect.go); this file finds the
// sentinels in it, and swaps those its routes allow.

// MessageUses is what MatchMessage found in one message: each sentinel
// standing alone as a string value, and where it is written, to swap it there.
type MessageUses struct {
	body    BodyUses
	message *secretroute.Message
}

// Places are the uses as AuthorizeRequest carries them.
func (m MessageUses) Places() []Place { return m.body.Places() }

// Message is the message the uses are in, as AuthorizeRequest carries it.
func (m MessageUses) Message() *secretroute.Message { return m.message }

// Empty reports whether the message carries no sentinel.
func (m MessageUses) Empty() bool { return m.body.Empty() }

// Sentinels are the sentinels the message carries, each once.
func (m MessageUses) Sentinels() []string {
	var out []string
	for _, use := range m.body.uses {
		if !slices.Contains(out, use.Sentinel) {
			out = append(out, use.Sentinel)
		}
	}
	return out
}

// MessageRefusal is a message that carries a sentinel somewhere no place can
// be: inside a longer string, in a member name, base64-encoded, in a binary
// message or a text message that is not one JSON object, or a sentinel the
// client no longer holds. Such a message is never sent (ADR 26-11-06-442 §2).
type MessageRefusal struct {
	Reason string
}

func (r *MessageRefusal) Error() string { return r.Reason }

// Sentinels are clientID's sentinels now: what a connection's messages are
// read for, besides the ones it recognized before (MatchMessage's retained).
func (s *Swapper) Sentinels(clientID string) []string {
	if s == nil {
		return nil
	}
	return slices.Clone(s.settings.Load().sentinels[clientID])
}

// InspectsMessages reports whether the resolver says some open route of
// clientID's credentials with a WebSocket block covers req, an upgrade
// request: whether its connection's messages are read.
func (s *Swapper) InspectsMessages(ctx context.Context, req PlacesRequest) bool {
	if s.resolver == nil || !s.Active(req.ClientID) {
		return false
	}
	return s.resolver.InspectsMessages(ctx, req)
}

// MatchMessage finds clientID's sentinels in one complete message the client
// sent, unmasked: a text message when text, else a binary one. A sentinel
// counts as a use only where it is the whole of a string value in a text
// message that is one JSON object, read strictly (secretroute.ParseMessage).
// Found anywhere else — literally or base64-encoded, decoded from JSON
// escapes or not — the message is refused with a *MessageRefusal. So is one
// carrying a sentinel of retained, the ones the connection recognized before,
// that the client no longer holds: a sentinel taken away is not traffic to
// let through unchecked (ADR 26-11-06-442 §4). A message carrying none is
// empty, whatever it is.
func (s *Swapper) MatchMessage(clientID string, payload []byte, text bool, retained []string) (MessageUses, error) {
	current := s.Sentinels(clientID)
	all := slices.Clone(current)
	for _, sentinel := range retained {
		if !slices.Contains(all, sentinel) {
			all = append(all, sentinel)
		}
	}
	if len(all) == 0 {
		return MessageUses{}, nil
	}
	carried := carriedSentinels(string(payload), all)
	if !text {
		if len(carried) > 0 {
			return MessageUses{}, &MessageRefusal{Reason: "a binary message carries a credential's placeholder; only a string in a JSON text message is a place"}
		}
		return MessageUses{}, nil
	}
	message, err := secretroute.ParseMessage(payload)
	if err != nil {
		if len(carried) > 0 {
			return MessageUses{}, &MessageRefusal{Reason: "a text message that carries a credential's placeholder is not a JSON object a place can be read in: " + err.Error()}
		}
		return MessageUses{}, nil
	}
	var uses []bodyUse
	accounted := map[string]bool{}
	for _, str := range message.Strings() {
		inside := carriedSentinels(str.Value, all)
		for _, sentinel := range inside {
			accounted[sentinel] = true
		}
		switch {
		case len(inside) == 0:
			continue
		case str.Name:
			// Named by its parent's pointer: its own holds the placeholder.
			where := "the top level"
			at := str.At()
			if parent := at[:strings.LastIndex(at, "/")]; parent != "" {
				where = secretroute.Safe(parent)
			}
			return MessageUses{}, &MessageRefusal{Reason: "a credential's placeholder is in a member name under " + where + ", which is no place"}
		case slices.Contains(current, str.Value):
			uses = append(uses, bodyUse{Place: Place{Sentinel: str.Value, At: str.At()}, start: str.Start, end: str.End})
		case slices.Contains(all, str.Value):
			return MessageUses{}, &MessageRefusal{Reason: "the string at " + secretroute.Safe(str.At()) + " is a credential's placeholder this sandbox no longer holds"}
		default:
			return MessageUses{}, &MessageRefusal{Reason: "a credential's placeholder is inside the string at " + secretroute.Safe(str.At()) + ", not the whole of it"}
		}
	}
	for _, sentinel := range carried {
		if !accounted[sentinel] {
			return MessageUses{}, &MessageRefusal{Reason: "a credential's placeholder is in the message outside any string"}
		}
	}
	return MessageUses{body: BodyUses{body: payload, uses: uses}, message: message}, nil
}

// carriedSentinels are the sentinels of all value carries, literally or
// inside a base64 token, the way a header is scanned.
func carriedSentinels(value string, all []string) []string {
	var found []string
	swapSentinels(value, all, func(sentinel string) (string, bool) {
		if !slices.Contains(found, sentinel) {
			found = append(found, sentinel)
		}
		return "", false
	})
	return found
}

// ApplyMessage resolves each use for host, the upgrade's destination, and
// writes its value into the message as a JSON string, leaving a sentinel that
// does not resolve in place. One window, retryUntil, bounds every resolve the
// message waits on (ADR 26-10-14-482 §2). It returns the message to send, nil
// when nothing in it changed, and what was swapped; a transient failure that
// outlasted the window is in Result.Errors, and such a message is not sent.
//
// echoes are the forms of every value swapped in: the value as itself, as the
// inside of a JSON string, and in the encodings a raw value is watched for.
// The connection's capture keeps them out of what the origin sends back,
// which may repeat what it was sent (ADR 26-11-06-442 §4); they are never
// written anywhere.
func (s *Swapper) ApplyMessage(ctx context.Context, clientID, host, path string, uses MessageUses, retryUntil time.Time) (out []byte, res Result, echoes [][]byte) {
	if uses.Empty() || !s.Active(clientID) {
		return nil, res, nil
	}
	st := s.settings.Load()
	at := destination{host: extractHost(host), path: path, retryUntil: retryUntil}
	var values []string
	out, changed := s.applyBody(ctx, st, clientID, at, uses.body, &res, &values)
	dedupe(&res.SecretIDs)
	dedupe(&res.Sentinels)
	if !changed || len(res.Errors) > 0 {
		return nil, res, nil
	}
	for _, value := range values {
		forms := rawForms(value)
		if quoted := encodeJSON(value); len(quoted) > 2 {
			forms = append(forms, string(quoted[1:len(quoted)-1]))
		}
		for _, form := range forms {
			echoes = append(echoes, []byte(form))
		}
	}
	return out, res, echoes
}

// Revision moves each time Reconfigure installs a configuration. A message
// is resolved under one revision and sent only under the same one (Decide).
func (s *Swapper) Revision() uint64 {
	if s == nil {
		return 0
	}
	return s.revision.Load()
}

// Decide runs decide, the last decision on a message resolved under
// revision, unless the configuration has changed since, or a sentinel it
// carries is no longer clientID's: then it reports stale, and the message is
// to be read and decided again. While decide runs no configuration is
// installed, so a revocation lands either before that decision or after it
// (ADR 26-11-06-442 §4). decide does no network I/O: a configuration never
// waits on an origin. Nor does it wait long on anything: it is handed a
// context that ends after decideBound, which Authorize, the call it makes,
// answers within (Resolver.Authorize). The caller commits the message to the
// wire by holding its connection's write lock from before Decide until it
// has written it, so nothing it decided waits behind another write.
func (s *Swapper) Decide(ctx context.Context, revision uint64, clientID string, sentinels []string, decide func(context.Context) error) (stale bool, err error) {
	s.publish.RLock()
	defer s.publish.RUnlock()
	if s.revision.Load() != revision {
		return true, nil
	}
	st := s.settings.Load()
	for _, sentinel := range sentinels {
		if !st.watches(clientID, sentinel) {
			return true, nil
		}
	}
	bounded, cancel := context.WithTimeout(ctx, decideBound)
	defer cancel()
	return false, decide(bounded)
}

// decideBound is the longest a message's last decision may hold off the
// installing of a configuration for the whole pool.
const decideBound = time.Second
