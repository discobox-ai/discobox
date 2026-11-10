package programstatus

import (
	"bytes"
	"strings"
	"time"
)

// MaxRecords is how many records a Set holds, the spec's minimum. A record
// past it evicts the one least recently updated.
const MaxRecords = 64

// Set is one terminal's records, by the spec's rules: a report replaces its
// record whole, a clear removes the addressed record and those under it, and
// records outlive nothing the caller does not drop.
//
// Each record carries the group it was reported from, an opaque number the
// caller chooses (the shim's is the PTY's foreground process group), so the
// caller can drop what a process left behind once it is gone (Drop).
//
// A Set is not safe for concurrent use.
type Set struct {
	records    map[string]entry
	seq        uint64
	reportedAt time.Time
}

type entry struct {
	record Record
	group  int
	seq    uint64
}

// Apply takes one report, reported at at from group. A query is not a report
// and changes nothing.
func (s *Set) Apply(report Report, at time.Time, group int) {
	if report.Query {
		return
	}
	s.reportedAt = at
	if report.Clear {
		s.clear(report.Record.ID)
		return
	}
	if s.records == nil {
		s.records = map[string]entry{}
	}
	id := report.Record.ID
	if _, ok := s.records[id]; !ok && len(s.records) >= MaxRecords {
		s.evictOldest()
	}
	s.seq++
	s.records[id] = entry{record: report.Record, group: group, seq: s.seq}
}

// Drop removes every record drop reports true for.
func (s *Set) Drop(drop func(state State, group int) bool) {
	for id, e := range s.records {
		if drop(e.record.State, e.group) {
			delete(s.records, id)
		}
	}
}

// Live reports whether a record in state is one the spec drops when the
// process that reported it is gone, or a shell prompt begins: `working` and
// `blocked`, and `idle`, which it may drop. `done` and `error` are results
// the person has not seen, and stay.
func Live(state State) bool {
	return state == Working || state == Blocked || state == Idle
}

// PromptCode is OSC 133, a shell's prompt marks: its `A` is a prompt
// beginning, which ends what the job before it reported as live.
const PromptCode = 133

// Seen is what one OSC sequence was to a Set (Set.Observe).
type Seen int

const (
	// SeenNothing is a sequence that is no report: another OSC, a 133 that
	// is not a prompt beginning, or a 7501 body the spec says to ignore.
	SeenNothing Seen = iota
	// SeenQuery is a feature query, which a terminal that knows the protocol
	// answers with ReplySequence.
	SeenQuery
	// SeenReport is a report taken: a record stored or cleared.
	SeenReport
	// SeenPrompt is a prompt beginning, which dropped the live records.
	SeenPrompt
)

// Observe takes the data of one OSC sequence a terminal's program wrote, as
// an emulator hands an OSC handler (`7501;…`, `133;A…`), at at from group,
// by the protocol's rules: a report is applied; a query changes nothing and
// is said to be one, for a caller that answers it; and a prompt beginning
// drops every live record, since the job that said them is over. Every
// reader of a program's output — the shim, the TUI's pane, the desktop's
// bridge — takes it through here, so none of them can differ on what a
// prompt does.
func (s *Set) Observe(data []byte, at time.Time, group int) Seen {
	switch {
	case bytes.HasPrefix(data, []byte("7501;")):
		report, ok := Parse(data)
		switch {
		case !ok:
			return SeenNothing
		case report.Query:
			return SeenQuery
		}
		s.Apply(report, at, group)
		return SeenReport
	case bytes.HasPrefix(data, []byte("133;A")):
		s.Drop(func(state State, _ int) bool { return Live(state) })
		return SeenPrompt
	}
	return SeenNothing
}

// ReportedAt is when the set last took a report, a clear included; zero
// before it has taken one. A prompt's drop is not a report.
func (s *Set) ReportedAt() time.Time { return s.reportedAt }

// Reset removes every record, as RIS does.
func (s *Set) Reset() { s.records = nil }

// Len is how many records the set holds.
func (s *Set) Len() int { return len(s.records) }

// Effective is the one record that stands for the terminal: the root record,
// or with none the most pressing of the others — blocked, then error,
// working, done, idle — and the most recent of those. A record with no app
// takes its nearest ancestor's. It reports false with no records.
func (s *Set) Effective() (Status, bool) {
	var best entry
	found := false
	if root, ok := s.records[""]; ok {
		best, found = root, true
	} else {
		for _, e := range s.records {
			if !found || pressing(e.record.State) > pressing(best.record.State) ||
				pressing(e.record.State) == pressing(best.record.State) && e.seq > best.seq {
				best, found = e, true
			}
		}
	}
	if !found {
		return Status{}, false
	}
	rec := best.record
	status := Status{
		State:      rec.State,
		Kind:       rec.Kind,
		App:        s.app(rec),
		Title:      rec.Title,
		Msg:        rec.Msg,
		ReportedAt: s.reportedAt,
	}
	if rec.Progress >= 0 {
		progress := rec.Progress
		status.Progress = &progress
	}
	return status, true
}

// pressing orders states by how much they want a person.
func pressing(state State) int {
	switch state {
	case Blocked:
		return 5
	case Error:
		return 4
	case Working:
		return 3
	case Done:
		return 2
	default:
		return 1
	}
}

// app is rec's app, or its nearest ancestor's when it names none.
func (s *Set) app(rec Record) string {
	if rec.App != "" {
		return rec.App
	}
	id := rec.ID
	for id != "" {
		cut := strings.LastIndexByte(id, '/')
		if cut < 0 {
			id = ""
		} else {
			id = id[:cut]
		}
		if parent, ok := s.records[id]; ok && parent.record.App != "" {
			return parent.record.App
		}
	}
	return ""
}

func (s *Set) clear(id string) {
	if id == "" {
		s.records = nil
		return
	}
	for other := range s.records {
		if other == id || strings.HasPrefix(other, id+"/") {
			delete(s.records, other)
		}
	}
}

func (s *Set) evictOldest() {
	var oldest string
	var oldestSeq uint64
	first := true
	for id, e := range s.records {
		if first || e.seq < oldestSeq {
			oldest, oldestSeq, first = id, e.seq, false
		}
	}
	delete(s.records, oldest)
}
