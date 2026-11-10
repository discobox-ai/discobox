package console

import (
	"fmt"
	"time"
	"unicode"
)

// The audit timeline's rules (876 §1), which the TUI's audit screen and the
// desktop's audit view both draw: which records fold into one row, when a
// record was recorded as the timeline says it, which of a record's
// recordings can be read and what each is called, and what the timeline says
// with no records to show. Which trails, and how they are read, is the data
// source's (DataSource.FollowAudit), the one reader `discobox admin audit ls
// --follow` prints from.

// AuditRun is consecutive records of a timeline that fold into one row,
// First to Last inclusive. A record that folds with nothing is a run of one.
type AuditRun struct {
	First, Last int
}

// Folds reports whether the run is more than one record, and so is drawn
// folded behind one row that opens it.
func (r AuditRun) Folds() bool { return r.Last > r.First }

// Count is how many records the run holds.
func (r AuditRun) Count() int { return r.Last - r.First + 1 }

// AuditRuns is records, oldest first, as the rows the timeline draws: each
// run of consecutive records with the same Group folds into one — http
// exchanges with one method, status and origin, which is what an agent
// polling or paging an API looks like — and every other record is a run of
// its own. A run is only ever consecutive records: one from another trail in
// between is part of what happened, and folding across it would reorder the
// timeline. An empty Group never folds.
func AuditRuns(records []AuditRecord) []AuditRun {
	runs := make([]AuditRun, 0, len(records))
	for i := 0; i < len(records); {
		j := i
		if group := records[i].Group; group != "" {
			for j+1 < len(records) && records[j+1].Group == group {
				j++
			}
		}
		runs = append(runs, AuditRun{First: i, Last: j})
		i = j + 1
	}
	return runs
}

// AuditRunSummary is what a folded run's row says: what its records have in
// common, from its newest, and how many there are.
func AuditRunSummary(newest AuditRecord, count int) string {
	return fmt.Sprintf("%s (count %d)", newest.GroupSummary, count)
}

// AuditTime is when a record was recorded, as the timeline says it: a clock
// time, as `audit ls --follow` prints one for the reason the timeline does,
// that a relative time is stale the moment it is drawn. A record from another
// day says which.
func AuditTime(at, now time.Time) string {
	at, now = at.Local(), now.Local()
	if y, mo, d := at.Date(); y == now.Year() && mo == now.Month() && d == now.Day() {
		return at.Format("15:04:05")
	}
	return at.Format("Jan 02 15:04")
}

// AuditRecording is one recording a record's detail offers to read
// (DataSource.AuditBody), by the part AuditBody takes, and what it is called.
type AuditRecording struct {
	Part  AuditPart
	Label string
}

// auditRecordings are every recording, in the order a record's detail offers
// them: the body the service answered first, which is the one most worth
// reading, then what was sent, then an upgraded stream.
var auditRecordings = []AuditRecording{
	{AuditResponseBody, "response body"},
	{AuditRequestBody, "request body"},
	{AuditStream, "stream"},
}

// AuditRecordings is what a record's detail offers to read: each recording
// the pool kept the bytes of, in the order they are offered.
func (d AuditRecordDetail) AuditRecordings() []AuditRecording {
	out := make([]AuditRecording, 0, len(d.Recordings))
	for _, r := range auditRecordings {
		for _, part := range d.Recordings {
			if part == r.Part {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// AuditRecordingLabel is what a recording is called, by its part: the part
// itself for one this console does not know.
func AuditRecordingLabel(part AuditPart) string {
	for _, r := range auditRecordings {
		if r.Part == part {
			return r.Label
		}
	}
	return string(part)
}

// AuditRecordTitle is the title a record read in full is shown under.
const AuditRecordTitle = "Audit record"

// AuditRecordingTitle is the title a recording read is shown under.
func AuditRecordingTitle(part AuditPart) string {
	r := []rune(AuditRecordingLabel(part))
	if len(r) > 0 {
		r[0] = unicode.ToUpper(r[0])
	}
	return string(r)
}

// What the timeline says with no record to show: before any read of the
// trails has finished, and after one has found nothing.
const (
	AuditReading = "reading the audit trails…"
	AuditNothing = "nothing recorded yet — records appear here as they are"
)

// AuditUnreadable is what the timeline says once the whole read failed, after
// which nothing more arrives.
func AuditUnreadable(err error) string { return "the audit trails could not be read: " + err.Error() }

// AuditRecordUnreadable is what is said when a record could not be read in
// full, against the record.
func AuditRecordUnreadable(record string, err error) string { return record + ": " + err.Error() }

// AuditRecordingUnreadable is what is said when a recording could not be
// read, against what it is called.
func AuditRecordingUnreadable(part AuditPart, err error) string {
	return AuditRecordingLabel(part) + ": " + err.Error()
}
