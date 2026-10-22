// Package programstatus is OSC 7501, the Program Status Protocol (draft 0.4,
// https://www.superlogical.com/rex/docs/build/program-status): a program
// saying over its terminal whether it is idle, working, done, blocked on a
// person, or failed.
//
// It is the protocol and nothing of Discobox's: parsing a report by the
// spec's grammar and limits, keeping one terminal's records by its
// replacement and clearing rules, choosing the one record that stands for the
// terminal, and encoding a record to send on. The shim's screen emulator
// keeps the records of the program it runs, and the console re-emits the
// focused box's to the terminal it runs in (ADR 26-10-16-424).
package programstatus

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Code is the OSC number the protocol is carried on.
const Code = 7501

// QuerySequence asks a terminal whether it supports the protocol. A terminal
// that does answers with ReplySequence.
const QuerySequence = "\x1b]7501;?\x1b\\"

// ReplySequence is a supporting terminal's answer to QuerySequence: the bare
// `?`, which by the spec means every state and every kind.
const ReplySequence = "\x1b]7501;?\x1b\\"

// ClearSequence removes every record on the terminal it is written to.
const ClearSequence = "\x1b]7501;state=clear\x1b\\"

// State is what a record says the program is doing.
type State string

const (
	// Idle is at rest, waiting for the person's next instruction.
	Idle State = "idle"
	// Working is running, optionally with a progress.
	Working State = "working"
	// Done is finished, with a result the person has not seen yet.
	Done State = "done"
	// Blocked cannot go on until the person acts; Kind says how.
	Blocked State = "blocked"
	// Error is failed and stopped.
	Error State = "error"
)

// clearState is not a state: a report carrying it removes records.
const clearState = "clear"

// Kind is what a blocked program needs from the person.
type Kind string

const (
	Permission Kind = "permission"
	Question   Kind = "question"
	Auth       Kind = "auth"
)

// Limits from the spec. A report that breaks one is discarded whole.
const (
	// maxBody is the largest body a sequence of at most 4096 bytes, from
	// `ESC ]` through a two-byte ST, can carry after its `7501;`.
	maxBody       = 4096 - len("\x1b]7501;") - len("\x1b\\")
	maxKey        = 16
	maxMsgEncoded = 2732
	// MaxMsg is the most a decoded msg may hold, in bytes.
	MaxMsg          = 2048
	maxTitleEncoded = 256
	// MaxTitle is the most a decoded title may hold, in bytes.
	MaxTitle    = 192
	maxApp      = 32
	maxID       = 128
	maxSegment  = 32
	maxDepth    = 8
	maxProgress = 100
)

// Record is one record a program reported. ID is empty for the root record.
// Progress is -1 when absent or indeterminate.
type Record struct {
	ID       string
	State    State
	Kind     Kind
	Progress int
	App      string
	Title    string
	Msg      string
}

// Report is one parsed OSC 7501 body: a query, a clear of ID and everything
// under it (every record when ID is empty), or a record to store.
type Report struct {
	Query  bool
	Clear  bool
	Record Record
}

// Parse reads the data of an OSC 7501 sequence — `7501;` and the body, as an
// emulator hands an OSC handler, or the body alone — by the spec's rules, and
// reports false for one the spec says to ignore whole.
func Parse(data []byte) (Report, bool) {
	body := bytes.TrimPrefix(data, []byte("7501;"))
	if len(body) > maxBody {
		return Report{}, false
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '?' {
		return Report{Query: true}, true
	}
	pairs := map[string]string{}
	for _, pair := range strings.Split(string(body), ":") {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !validKey(key) {
			continue
		}
		if !validValue(value) {
			// A malformed pair is skipped, except where skipping it would
			// change what the report means rather than lose a detail of it:
			// an id that went missing would address the root record, and
			// text that went missing would be text replaced by none. The
			// spec ignores a malformed id, and text that does not decode,
			// whole; a byte outside the alphabet is both.
			if wholeReportKeys[key] {
				return Report{}, false
			}
			continue
		}
		pairs[key] = value
	}

	rec := Record{Progress: -1}
	id, hasID := pairs["id"]
	if hasID {
		if !validID(id) {
			return Report{}, false
		}
		rec.ID = id
	}
	switch state := pairs["state"]; state {
	case clearState:
		return Report{Clear: true, Record: Record{ID: rec.ID, Progress: -1}}, true
	case string(Idle), string(Working), string(Done), string(Blocked), string(Error):
		rec.State = State(state)
	default:
		return Report{}, false
	}
	if rec.State == Blocked {
		switch kind := Kind(pairs["kind"]); kind {
		case Permission, Question, Auth:
			rec.Kind = kind
		}
	}
	if rec.State == Working || rec.State == Blocked {
		if p, err := strconv.Atoi(pairs["progress"]); err == nil && p >= 0 && p <= maxProgress {
			rec.Progress = p
		}
	}
	if app := pairs["app"]; validApp(app) {
		rec.App = app
	}
	var ok bool
	if rec.Title, ok = decodeText(pairs["title"], maxTitleEncoded, MaxTitle); !ok {
		return Report{}, false
	}
	if rec.Msg, ok = decodeText(pairs["msg"], maxMsgEncoded, MaxMsg); !ok {
		return Report{}, false
	}
	return Report{Record: rec}, true
}

// wholeReportKeys are the keys whose malformed value fails the report rather
// than the pair (Parse).
var wholeReportKeys = map[string]bool{"id": true, "msg": true, "title": true}

// Sequence encodes r as the root record of the terminal it is written to,
// whatever its ID: the console speaks for one box, not for that box's
// records. Text past the spec's limits is cut at a character boundary rather
// than sent to be discarded.
func (r Record) Sequence() string {
	pairs := []string{"state=" + string(r.State)}
	if r.State == Blocked && r.Kind != "" {
		pairs = append(pairs, "kind="+string(r.Kind))
	}
	if (r.State == Working || r.State == Blocked) && r.Progress >= 0 && r.Progress <= maxProgress {
		pairs = append(pairs, "progress="+strconv.Itoa(r.Progress))
	}
	if validApp(r.App) {
		pairs = append(pairs, "app="+r.App)
	}
	if title := Displayable(r.Title, MaxTitle); title != "" {
		pairs = append(pairs, "title="+base64.StdEncoding.EncodeToString([]byte(title)))
	}
	if msg := Displayable(r.Msg, MaxMsg); msg != "" {
		pairs = append(pairs, "msg="+base64.StdEncoding.EncodeToString([]byte(msg)))
	}
	return "\x1b]7501;" + strings.Join(pairs, ":") + "\x1b\\"
}

// Displayable is s made safe to show outside a terminal's grid and cut to at
// most limit bytes: control characters, bidirectional overrides and invisible
// formatting are dropped, so a program's text cannot reorder or hide what is
// drawn around it. A limit of zero or less does not cut.
func Displayable(s string, limit int) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if limit > 0 && b.Len()+utf8.RuneLen(r) > limit {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

func validKey(key string) bool {
	if key == "" || len(key) > maxKey {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 'a' || key[i] > 'z' {
			return false
		}
	}
	return true
}

// validValue is the spec's value alphabet, `[A-Za-z0-9_.,+/=-]*`.
func validValue(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !isAlnum(c) && !strings.ContainsRune("_.,+/=-", rune(c)) {
			return false
		}
	}
	return true
}

// validName is `[A-Za-z0-9_.+-]{1,limit}`, the alphabet of an app and of an id
// segment.
func validName(s string, limit int) bool {
	if s == "" || len(s) > limit {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !isAlnum(c) && c != '_' && c != '.' && c != '+' && c != '-' {
			return false
		}
	}
	return true
}

func validApp(app string) bool { return validName(app, maxApp) }

func validID(id string) bool {
	if id == "" || len(id) > maxID {
		return false
	}
	segments := strings.Split(id, "/")
	if len(segments) > maxDepth {
		return false
	}
	for _, segment := range segments {
		if !validName(segment, maxSegment) {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// decodeText decodes a base64 text value. The encoded size is checked before
// decoding, padding is optional, and text that is not UTF-8 or holds a
// control character fails the report.
func decodeText(value string, maxEncoded, maxDecoded int) (string, bool) {
	if value == "" {
		return "", true
	}
	if len(value) > maxEncoded {
		return "", false
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(value, "="))
	if err != nil || len(raw) > maxDecoded || !utf8.Valid(raw) {
		return "", false
	}
	for _, r := range string(raw) {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return string(raw), true
}

// Status is the one record that stands for a terminal (Set.Effective).
// ReportedAt is when the terminal last took a report. Progress is nil when
// absent or indeterminate.
//
// It is the protocol's, not any wire's: the API's shape for it is
// SandboxAgentProgramStatus in the OpenAPI document, and each end that puts
// it on a wire maps it there.
type Status struct {
	State      State
	Kind       Kind
	Progress   *int
	App        string
	Title      string
	Msg        string
	ReportedAt time.Time
}

// Record is s as a root record, to send on.
func (s Status) Record() Record {
	rec := Record{State: s.State, Kind: s.Kind, Progress: -1, App: s.App, Title: s.Title, Msg: s.Msg}
	if s.Progress != nil {
		rec.Progress = *s.Progress
	}
	return rec
}
