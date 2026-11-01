package audit

import (
	"bytes"
	"net/http"
)

// Redactor keeps values out of what a client's audit trail records: the raw
// secrets the client holds (ADR 26-10-27-840), which this proxy watches its
// requests for but must never write down. Redact is applied to the text of a
// row — its URLs, headers and reason — and Patterns to the bytes spooled for
// its bodies and streams, longest first; MaxLen is the longest.
type Redactor interface {
	Redact(text string) string
	Patterns() [][]byte
	MaxLen() int
}

// redactedBytes is what a pattern is spooled as.
var redactedBytes = []byte("[REDACTED]")

// SetRedactor installs what each client's trail is kept clean of. A nil
// answer for a client is nothing to keep out. It applies to rows recorded,
// and spools begun, from then on.
func (r *Recorder) SetRedactor(redactor func(clientID string) Redactor) {
	if r == nil {
		return
	}
	r.redactor.Store(&redactor)
}

func (r *Recorder) redactorFor(clientID string) Redactor {
	if r == nil {
		return nil
	}
	fn := r.redactor.Load()
	if fn == nil || *fn == nil {
		return nil
	}
	return (*fn)(clientID)
}

// redactHTTP is event with the client's redactor applied to every text the
// row keeps. The headers are copied: the event's are the live request's.
func redactHTTP(event HTTPEvent, redactor Redactor) HTTPEvent {
	if redactor == nil {
		return event
	}
	event.URL = redactor.Redact(event.URL)
	event.ForwardedURL = redactor.Redact(event.ForwardedURL)
	event.BlockedReason = redactor.Redact(event.BlockedReason)
	event.CacheKey = redactor.Redact(event.CacheKey)
	event.RequestHeaders = redactHeaders(event.RequestHeaders, redactor)
	event.ResponseHeaders = redactHeaders(event.ResponseHeaders, redactor)
	// A passthrough tunnel's ClientHello is the client's own text, read
	// before anything else could have been (ADR 26-10-31-819 §4), and a dial
	// error repeats the authority it was given.
	event.Tunnel.ServerName = redactor.Redact(event.Tunnel.ServerName)
	event.Tunnel.ALPN = redactor.Redact(event.Tunnel.ALPN)
	event.Tunnel.Error = redactor.Redact(event.Tunnel.Error)
	return event
}

func redactHeaders(headers http.Header, redactor Redactor) http.Header {
	if headers == nil {
		return nil
	}
	out := make(http.Header, len(headers))
	for name, values := range headers {
		copied := make([]string, len(values))
		for i, value := range values {
			copied[i] = redactor.Redact(value)
		}
		out[name] = copied
	}
	return out
}

// redactPatterns replaces every pattern in data.
func redactPatterns(data []byte, patterns [][]byte) []byte {
	for _, pattern := range patterns {
		if len(pattern) > 0 && bytes.Contains(data, pattern) {
			data = bytes.ReplaceAll(data, pattern, redactedBytes)
		}
	}
	return data
}

// spoolRedaction redacts a spool written in pieces: it holds back the last
// MaxLen-1 bytes of what it has seen, so a pattern split across two writes is
// still whole when it is replaced. What it holds is written on flush.
type spoolRedaction struct {
	patterns [][]byte
	keep     int
	held     []byte
}

func newSpoolRedaction(redactor Redactor) *spoolRedaction {
	if redactor == nil || len(redactor.Patterns()) == 0 {
		return nil
	}
	return &spoolRedaction{patterns: redactor.Patterns(), keep: max(redactor.MaxLen()-1, 0)}
}

// next is what may be written of p now.
func (s *spoolRedaction) next(p []byte) []byte {
	buf := redactPatterns(append(s.held, p...), s.patterns)
	cut := max(len(buf)-s.keep, 0)
	s.held = append([]byte(nil), buf[cut:]...)
	return buf[:cut]
}

// flush is what is held, to write once nothing more is coming.
func (s *spoolRedaction) flush() []byte {
	out := s.held
	s.held = nil
	return out
}
