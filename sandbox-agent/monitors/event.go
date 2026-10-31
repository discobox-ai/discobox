package monitors

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxBody is the most of an event's body that is kept. A body over it is cut
// there and the event says so (ADR 26-10-31-073 §6).
const MaxBody = 1 << 20

// maxLine bounds one line a monitor prints: a body at MaxBody, JSON-escaped,
// with room for the rest of the event. What is past it is dropped.
const maxLine = 4 * MaxBody

// Kinds of the events a runner queues about its own instance, rather than ones
// its monitor printed.
const (
	KindFailed    = "monitor.failed"
	KindRecovered = "monitor.recovered"
	KindDropped   = "monitor.dropped"
)

// Event is one queued event: what a monitor printed, and what the queue added
// to it.
type Event struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Instance string    `json:"instance"`
	Monitor  string    `json:"monitor,omitempty"`

	Kind    string          `json:"kind,omitempty"`
	Title   string          `json:"title"`
	Summary string          `json:"summary,omitempty"`
	URL     string          `json:"url,omitempty"`
	Key     string          `json:"key,omitempty"`
	Body    string          `json:"body,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Final   bool            `json:"final,omitempty"`

	// BodyTruncated says the body was cut at MaxBody.
	BodyTruncated bool `json:"bodyTruncated,omitempty"`
	// Count is how many events this one stands for: more than one when newer
	// events with its key replaced older ones still pending.
	Count int `json:"count,omitempty"`
}

// line is what a monitor may print. Anything else in the object is ignored.
type line struct {
	Kind    string          `json:"kind"`
	Title   string          `json:"title"`
	Summary string          `json:"summary"`
	URL     string          `json:"url"`
	Key     string          `json:"key"`
	Body    string          `json:"body"`
	Data    json.RawMessage `json:"data"`
	Final   bool            `json:"final"`
}

// ParseLine reads one line a monitor printed. A JSON object is an event, and
// only its title is required; any other non-blank line is the event whose
// title is the line, so `grep --line-buffered ERROR` is a monitor as written.
// A blank line is no event.
func ParseLine(raw []byte) (Event, bool) {
	text := bytes.TrimSpace(raw)
	if len(text) == 0 {
		return Event{}, false
	}
	if text[0] == '{' {
		var l line
		if err := json.Unmarshal(text, &l); err == nil {
			ev := Event{
				Kind:    strings.TrimSpace(l.Kind),
				Title:   strings.TrimSpace(l.Title),
				Summary: strings.TrimSpace(l.Summary),
				URL:     strings.TrimSpace(l.URL),
				Key:     strings.TrimSpace(l.Key),
				Body:    l.Body,
				Final:   l.Final,
			}
			if len(l.Data) > 0 && !bytes.Equal(l.Data, []byte("null")) {
				ev.Data = l.Data
			}
			if ev.Title == "" {
				// An object with no title still happened. Its kind says more
				// than nothing; failing that, the line itself is the title.
				ev.Title = ev.Kind
				if ev.Title == "" {
					ev.Title = string(text)
				}
			}
			return ev.clipped(), true
		}
	}
	return Event{Title: toValidUTF8(string(text))}.clipped(), true
}

// clipped holds an event to MaxBody.
func (ev Event) clipped() Event {
	if len(ev.Body) > MaxBody {
		cut := MaxBody
		// Never split a UTF-8 sequence.
		for cut > 0 && !utf8.RuneStart(ev.Body[cut]) {
			cut--
		}
		ev.Body = ev.Body[:cut]
		ev.BodyTruncated = true
	}
	return ev
}

func toValidUTF8(s string) string { return strings.ToValidUTF8(s, "�") }
