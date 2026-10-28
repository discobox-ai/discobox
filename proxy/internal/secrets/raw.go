package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A raw secret is one a person handed the sandbox the value of, rather than a
// sentinel this proxy swaps (ADR 26-10-27-840). The proxy cannot swap what the
// sandbox already holds, so it holds the value too — in memory only — and
// watches the sandbox's requests for it: one carrying it off its routes is
// refused, one on them is recorded as spending it, and the audit trail never
// keeps it. That is a guard against mistakes, not a boundary: a sandbox
// holding the value can encode it any way the scan does not know.
//
// Each raw binding has a sentinel the sandbox never sees: the handle the
// resolver answers its value by, and the key its routes are held under, so
// the route check is the one every sentinel gets (Resolver.Authorize).

// RawRequest asks for the value of a client's raw binding, by its sentinel.
// There is no host: the value goes to the sandbox itself.
type RawRequest struct {
	ClientID string
	Sentinel string
}

// RawResult is a raw binding's value as the resolver answers it. A zero
// ExpiresAt is a binding and a value that do not lapse.
type RawResult struct {
	Value     string
	ExpiresAt time.Time
	SecretID  string
	BindingID string
}

// RawValue is a raw binding's value as the sandbox is handed it.
type RawValue struct {
	Sentinel  string
	Value     string
	ExpiresAt time.Time
	SecretID  string
	BindingID string
}

// RawMinLength is the shortest value watched for. A shorter one would match
// ordinary text far too often to refuse requests over, and is neither watched
// for nor redacted.
const RawMinLength = 8

// rawRetiredGrace is how long a value the sandbox may still hold is watched
// for after it stopped being the binding's: replaced by a rotation, or its
// binding gone. A process started with it keeps it in its environment, so it
// is still the sandbox's to leak; one whose binding is gone has no routes, so
// it is refused everywhere.
const rawRetiredGrace = 24 * time.Hour

// rawResolveTimeout bounds a background resolve, as refreshTimeout bounds a
// sentinel's.
const rawResolveTimeout = 15 * time.Second

type rawKey struct {
	clientID string
	sentinel string
}

type rawEntry struct {
	value     string
	secretID  string
	bindingID string
	// expiresAt is the binding's or the value's own lapse; zero is none.
	expiresAt time.Time
	// refreshAt is when the value is asked for again before it is handed out.
	refreshAt time.Time
}

type rawRetired struct {
	rawKey
	value     string
	secretID  string
	bindingID string
	until     time.Time
}

// rawWatch holds the values of every client's raw bindings, and the values a
// client may still hold from before (rawRetired).
type rawWatch struct {
	resolver Resolver
	now      func() time.Time

	mu        sync.Mutex
	refresh   time.Duration
	sentinels map[string][]string
	entries   map[rawKey]rawEntry
	retired   []rawRetired
	inflight  map[rawKey]chan struct{}
	// denied is when each key was last denied with nothing held, so a client
	// whose binding is gone is not asked about on every request (await).
	denied map[rawKey]time.Time
	// sets is each client's RawSet, rebuilt on every change under mu and read
	// without it on every request.
	sets atomic.Pointer[map[string]*RawSet]
}

func newRawWatch(resolver Resolver, now func() time.Time) *rawWatch {
	w := &rawWatch{
		resolver: resolver,
		now:      now,
		entries:  map[rawKey]rawEntry{},
		inflight: map[rawKey]chan struct{}{},
		denied:   map[rawKey]time.Time{},
	}
	empty := map[string]*RawSet{}
	w.sets.Store(&empty)
	return w
}

// reconfigure installs each client's raw sentinels. A value whose sentinel
// left is retired rather than forgotten, and a sentinel new to the set is
// resolved in the background, so a request is watched for it before anything
// asks for it.
func (w *rawWatch) reconfigure(sentinels map[string][]string, refresh time.Duration) {
	if w == nil {
		return
	}
	cleaned := make(map[string][]string, len(sentinels))
	for clientID, list := range sentinels {
		var out []string
		for _, sentinel := range list {
			if sentinel = strings.TrimSpace(sentinel); sentinel != "" && !slices.Contains(out, sentinel) {
				out = append(out, sentinel)
			}
		}
		if len(out) > 0 {
			cleaned[clientID] = out
		}
	}
	w.mu.Lock()
	w.refresh = refresh
	w.sentinels = cleaned
	now := w.now()
	for key, entry := range w.entries {
		if !slices.Contains(cleaned[key.clientID], key.sentinel) {
			w.retireLocked(key, entry, now)
			delete(w.entries, key)
		}
	}
	for key := range w.denied {
		if !slices.Contains(cleaned[key.clientID], key.sentinel) {
			delete(w.denied, key)
		}
	}
	var missing []rawKey
	for clientID, list := range cleaned {
		for _, sentinel := range list {
			key := rawKey{clientID: clientID, sentinel: sentinel}
			if _, ok := w.entries[key]; !ok {
				missing = append(missing, key)
			}
		}
	}
	w.rebuildLocked(now)
	w.mu.Unlock()
	for _, key := range missing {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), rawResolveTimeout)
			defer cancel()
			_, _ = w.resolve(ctx, key)
		}()
	}
}

// await makes sure every raw sentinel configured for clientID has an answer
// before its request is let through: a value held, or a denial within
// negTTL. One with neither is asked for until retryUntil. What it returns
// are the ones still without an answer, which the request must not be sent
// past: what to watch for is the configured set, not what happened to be
// fetched, so a value never fetched is never a value not watched for.
func (w *rawWatch) await(ctx context.Context, clientID string, retryUntil time.Time, negTTL time.Duration) []string {
	if w == nil {
		return nil
	}
	now := w.now()
	w.mu.Lock()
	var missing []rawKey
	for _, sentinel := range w.sentinels[clientID] {
		key := rawKey{clientID: clientID, sentinel: sentinel}
		if _, ok := w.entries[key]; ok {
			continue
		}
		if at, ok := w.denied[key]; ok && now.Sub(at) < negTTL {
			continue
		}
		missing = append(missing, key)
	}
	w.mu.Unlock()
	var errs []string
	for _, key := range missing {
		_, err := RetryTransient(ctx, retryUntil, func(attempt context.Context) (rawEntry, error) {
			return w.resolve(attempt, key)
		}, func(err error) bool { return errors.Is(err, ErrDenied) })
		if err != nil && !errors.Is(err, ErrDenied) {
			errs = append(errs, "raw secret value unavailable: "+err.Error())
		}
	}
	return errs
}

// values is every raw value clientID holds now, each asked for again once its
// refresh is due. A value the resolver could not be asked about is served
// until it lapses; one the resolver denies is retired and left out.
func (w *rawWatch) values(ctx context.Context, clientID string) []RawValue {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	sentinels := slices.Clone(w.sentinels[clientID])
	w.mu.Unlock()
	out := make([]RawValue, 0, len(sentinels))
	for _, sentinel := range sentinels {
		key := rawKey{clientID: clientID, sentinel: sentinel}
		entry, ok := w.current(ctx, key)
		if !ok {
			continue
		}
		out = append(out, RawValue{
			Sentinel:  sentinel,
			Value:     entry.value,
			ExpiresAt: entry.expiresAt,
			SecretID:  entry.secretID,
			BindingID: entry.bindingID,
		})
	}
	return out
}

// current is key's value, asked for again when its refresh is due or there
// is none.
func (w *rawWatch) current(ctx context.Context, key rawKey) (rawEntry, bool) {
	now := w.now()
	w.mu.Lock()
	entry, ok := w.entries[key]
	w.mu.Unlock()
	if ok && now.Before(entry.refreshAt) {
		return entry, true
	}
	fresh, err := w.resolve(ctx, key)
	if err == nil {
		return fresh, true
	}
	if errors.Is(err, ErrDenied) || !ok {
		return rawEntry{}, false
	}
	// The resolver is unreachable: what is cached stands until it lapses.
	if !entry.expiresAt.IsZero() && !now.Before(entry.expiresAt) {
		return rawEntry{}, false
	}
	return entry, true
}

// resolve asks for key's value, once at a time per key, and records the
// answer. A denial retires what was held.
func (w *rawWatch) resolve(ctx context.Context, key rawKey) (rawEntry, error) {
	for {
		w.mu.Lock()
		if !slices.Contains(w.sentinels[key.clientID], key.sentinel) {
			w.mu.Unlock()
			return rawEntry{}, ErrDenied
		}
		if entry, ok := w.entries[key]; ok && w.now().Before(entry.refreshAt) {
			// Answered since the caller looked.
			w.mu.Unlock()
			return entry, nil
		}
		wait, busy := w.inflight[key]
		if !busy {
			done := make(chan struct{})
			w.inflight[key] = done
			w.mu.Unlock()
			entry, err := w.ask(ctx, key)
			w.mu.Lock()
			delete(w.inflight, key)
			close(done)
			w.mu.Unlock()
			return entry, err
		}
		w.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return rawEntry{}, ctx.Err()
		}
		// The ask that was in flight has answered: the loop takes its
		// answer if it left one, and asks again if it did not.
	}
}

func (w *rawWatch) ask(ctx context.Context, key rawKey) (rawEntry, error) {
	result, err := w.resolver.ResolveRaw(ctx, key.request())
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	still := slices.Contains(w.sentinels[key.clientID], key.sentinel)
	if errors.Is(err, ErrDenied) || (err == nil && result.Value == "") {
		w.denied[key] = now
		if old, ok := w.entries[key]; ok {
			w.retireLocked(key, old, now)
			delete(w.entries, key)
			w.rebuildLocked(now)
		}
		return rawEntry{}, ErrDenied
	}
	if err != nil {
		return rawEntry{}, err
	}
	entry := rawEntry{
		value:     result.Value,
		secretID:  result.SecretID,
		bindingID: result.BindingID,
		expiresAt: result.ExpiresAt,
		refreshAt: now.Add(w.refresh),
	}
	if !entry.expiresAt.IsZero() && entry.expiresAt.Before(entry.refreshAt) {
		entry.refreshAt = entry.expiresAt
	}
	if !still {
		// The sentinel left while it was asked about: what came back is
		// nothing the sandbox is handed, but it may hold it already.
		w.retireLocked(key, entry, now)
		w.rebuildLocked(now)
		return rawEntry{}, ErrDenied
	}
	if old, ok := w.entries[key]; ok && old.value != entry.value {
		w.retireLocked(key, old, now)
	}
	delete(w.denied, key)
	w.entries[key] = entry
	w.rebuildLocked(now)
	return entry, nil
}

func (k rawKey) request() RawRequest {
	return RawRequest{ClientID: k.clientID, Sentinel: k.sentinel}
}

func (w *rawWatch) retireLocked(key rawKey, entry rawEntry, now time.Time) {
	w.retired = append(w.retired, rawRetired{
		rawKey:    key,
		value:     entry.value,
		secretID:  entry.secretID,
		bindingID: entry.bindingID,
		until:     now.Add(rawRetiredGrace),
	})
}

// rebuildLocked publishes each client's RawSet from what is held, dropping
// retired values past their grace.
func (w *rawWatch) rebuildLocked(now time.Time) {
	kept := w.retired[:0]
	for _, retired := range w.retired {
		if now.Before(retired.until) {
			kept = append(kept, retired)
		}
	}
	w.retired = kept
	byClient := map[string][]rawWatched{}
	add := func(clientID string, watched rawWatched) {
		if len(watched.value) < RawMinLength {
			return
		}
		for _, have := range byClient[clientID] {
			if have.value == watched.value {
				return
			}
		}
		byClient[clientID] = append(byClient[clientID], watched)
	}
	for key, entry := range w.entries {
		add(key.clientID, rawWatched{sentinel: key.sentinel, secretID: entry.secretID, bindingID: entry.bindingID, value: entry.value})
	}
	for _, retired := range w.retired {
		add(retired.clientID, rawWatched{sentinel: retired.sentinel, secretID: retired.secretID, bindingID: retired.bindingID, value: retired.value})
	}
	sets := make(map[string]*RawSet, len(byClient))
	for clientID, watched := range byClient {
		sets[clientID] = newRawSet(watched)
	}
	w.sets.Store(&sets)
}

// set is clientID's RawSet, nil when it holds no value worth watching for.
func (w *rawWatch) set(clientID string) *RawSet {
	if w == nil {
		return nil
	}
	return (*w.sets.Load())[clientID]
}

// rawWatched is one value a client holds, with the forms it is looked for in.
type rawWatched struct {
	sentinel  string
	secretID  string
	bindingID string
	value     string
	forms     []string
}

// RawHit is a raw value found in a request: the binding it is the value of.
// The sentinel is the binding's handle, which its routes are held under.
type RawHit struct {
	Sentinel  string
	SecretID  string
	BindingID string
}

// RawSet is the raw values one client holds, as they are looked for in its
// requests and kept out of its audit trail. It is immutable.
type RawSet struct {
	watched  []rawWatched
	patterns [][]byte
	maxLen   int
}

func newRawSet(watched []rawWatched) *RawSet {
	set := &RawSet{}
	for _, w := range watched {
		w.forms = rawForms(w.value)
		for _, form := range w.forms {
			set.patterns = append(set.patterns, []byte(form))
			set.maxLen = max(set.maxLen, len(form))
		}
		set.watched = append(set.watched, w)
	}
	// Longest first, so a form that contains another is replaced whole.
	slices.SortStableFunc(set.patterns, func(a, b []byte) int { return len(b) - len(a) })
	return set
}

// rawForms are the spellings a value is looked for in: itself, its base64 in
// both alphabets with and without padding, and its percent-encodings.
func rawForms(value string) []string {
	forms := []string{value}
	add := func(form string) {
		if form != "" && !slices.Contains(forms, form) {
			forms = append(forms, form)
		}
	}
	data := []byte(value)
	add(base64.StdEncoding.EncodeToString(data))
	add(base64.RawStdEncoding.EncodeToString(data))
	add(base64.URLEncoding.EncodeToString(data))
	add(base64.RawURLEncoding.EncodeToString(data))
	add(url.QueryEscape(value))
	add(url.PathEscape(value))
	return forms
}

// Find reports which raw values text carries, in any of their forms or inside
// a base64 token whose decoded text holds one (an `Authorization: Basic`
// password).
func (s *RawSet) Find(text string) []RawHit {
	if s == nil || text == "" {
		return nil
	}
	var hits []RawHit
	for _, w := range s.watched {
		if w.foundIn(text) {
			hits = append(hits, RawHit{Sentinel: w.sentinel, SecretID: w.secretID, BindingID: w.bindingID})
		}
	}
	return hits
}

func (w rawWatched) foundIn(text string) bool {
	for _, form := range w.forms {
		if strings.Contains(text, form) {
			return true
		}
	}
	for _, token := range base64Token.FindAllString(text, -1) {
		if len(token) > maxEncodedToken {
			continue
		}
		if decoded, err := tokenEncoding(token).DecodeString(token); err == nil && strings.Contains(string(decoded), w.value) {
			return true
		}
	}
	return false
}

// rawRedacted is what a raw value is recorded as.
const rawRedacted = "[REDACTED]"

// Redact is text with every raw value in it, in any form, replaced, and every
// base64 token whose decoded text holds one replaced whole.
func (s *RawSet) Redact(text string) string {
	if s == nil || text == "" {
		return text
	}
	for _, pattern := range s.patterns {
		text = strings.ReplaceAll(text, string(pattern), rawRedacted)
	}
	return base64Token.ReplaceAllStringFunc(text, func(token string) string {
		if len(token) > maxEncodedToken {
			return token
		}
		decoded, err := tokenEncoding(token).DecodeString(token)
		if err != nil {
			return token
		}
		for _, w := range s.watched {
			if strings.Contains(string(decoded), w.value) {
				return rawRedacted
			}
		}
		return token
	})
}

// Patterns are the byte spellings a stream of bytes is redacted of, longest
// first; MaxLen is the longest.
func (s *RawSet) Patterns() [][]byte {
	if s == nil {
		return nil
	}
	return s.patterns
}

// MaxLen is the length of the longest pattern.
func (s *RawSet) MaxLen() int {
	if s == nil {
		return 0
	}
	return s.maxLen
}

// FindBytes is Find over the forms alone, for a body: decoding every base64
// run in a body is not worth what it would find.
func (s *RawSet) FindBytes(data []byte) []RawHit {
	if s == nil || len(data) == 0 {
		return nil
	}
	var hits []RawHit
	for _, w := range s.watched {
		for _, form := range w.forms {
			if bytes.Contains(data, []byte(form)) {
				hits = append(hits, RawHit{Sentinel: w.sentinel, SecretID: w.secretID, BindingID: w.bindingID})
				break
			}
		}
	}
	return hits
}
