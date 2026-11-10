package console

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAuditRunsFoldOnlyConsecutiveRecordsOfOneGroup(t *testing.T) {
	g := func(id, group string) AuditRecord { return AuditRecord{ID: id, Group: group} }
	records := []AuditRecord{
		g("a", "GET 200 api"), g("b", "GET 200 api"), g("c", "GET 200 api"),
		g("d", ""), g("e", ""),
		g("f", "GET 200 api"),
		g("g", "POST 201 api"), g("h", "POST 201 api"),
	}
	want := []AuditRun{{0, 2}, {3, 3}, {4, 4}, {5, 5}, {6, 7}}
	if got := AuditRuns(records); !reflect.DeepEqual(got, want) {
		t.Fatalf("runs = %v, want %v", got, want)
	}
	if !want[0].Folds() || want[1].Folds() || want[0].Count() != 3 {
		t.Fatalf("a run of three folds and counts three; a record alone does not fold")
	}
	if got := AuditRuns(nil); got == nil || len(got) != 0 {
		t.Fatalf("no records are no runs, never nil: %#v", got)
	}
}

func TestAuditRunSummarySaysTheNewestAndTheCount(t *testing.T) {
	if got := AuditRunSummary(AuditRecord{GroupSummary: "GET 200 https://api.github.com/…"}, 4); got != "GET 200 https://api.github.com/… (count 4)" {
		t.Fatalf("summary = %q", got)
	}
}

func TestAuditTimeIsAClockTimeTodayAndADateOtherwise(t *testing.T) {
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.Local)
	if got := AuditTime(now.Add(-time.Hour), now); got != "14:00:00" {
		t.Fatalf("today = %q", got)
	}
	if got := AuditTime(now.Add(-24*time.Hour), now); got != "Nov 09 15:00" {
		t.Fatalf("yesterday = %q", got)
	}
}

func TestARecordsRecordingsAreOfferedResponseFirst(t *testing.T) {
	d := AuditRecordDetail{Recordings: []AuditPart{AuditStream, AuditRequestBody, AuditResponseBody, "trailers"}}
	want := []AuditRecording{{AuditResponseBody, "response body"}, {AuditRequestBody, "request body"}, {AuditStream, "stream"}}
	if got := d.AuditRecordings(); !reflect.DeepEqual(got, want) {
		t.Fatalf("recordings = %v, want %v", got, want)
	}
	if got := (AuditRecordDetail{}).AuditRecordings(); got == nil || len(got) != 0 {
		t.Fatalf("a record with none offers none, never nil: %#v", got)
	}
	if AuditRecordingTitle(AuditResponseBody) != "Response body" || AuditRecordingTitle("trailers") != "Trailers" {
		t.Fatalf("titles = %q, %q", AuditRecordingTitle(AuditResponseBody), AuditRecordingTitle("trailers"))
	}
	if AuditUnreadable(errors.New("no route")) != "the audit trails could not be read: no route" {
		t.Fatalf("unreadable = %q", AuditUnreadable(errors.New("no route")))
	}
	if got := AuditRecordUnreadable("http-1", errors.New("gone")); got != "http-1: gone" {
		t.Fatalf("a record unreadable = %q", got)
	}
	if got := AuditRecordingUnreadable(AuditRequestBody, errors.New("not recorded")); got != "request body: not recorded" {
		t.Fatalf("a recording unreadable = %q", got)
	}
}

// A search lets a record through by what its row says, or by its ID on its
// own — never by letters borrowed from an ID nothing on the row shows.
func TestAuditSearchMatchesTheRowOrTheIDAlone(t *testing.T) {
	records := []AuditRecord{
		{ID: "http_1", Source: "http", Summary: "GET 200 https://api.github.com/user"},
		{ID: "http_2", Source: "http", Summary: "GET 200 https://registry.npmjs.org/react"},
		{ID: "cvd_needle", Source: "creds", Summary: "use github (at use): approved"},
	}
	for _, c := range []struct {
		query string
		want  []int
	}{
		{"", []int{0, 1, 2}},
		{"ghub", []int{0, 2}},
		{"gith", []int{0, 2}},
		{"needle", []int{2}},
		{"http_2", []int{1}},
		{"GitHub", []int{}},
		{"xz", []int{}},
	} {
		if got := AuditSearch(ParseQuery(c.query), records); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%q lets through %v, want %v", c.query, got, c.want)
		}
	}
}

// What a search lights is split between the trail and the line, by rune
// index into each; a row found by its ID alone lights nothing.
func TestAuditLitSplitsTheTrailFromTheLine(t *testing.T) {
	src, sum := AuditLit(ParseQuery("hg 2é"), "http", "GET 2é0")
	if !reflect.DeepEqual(src, []int{0}) || !reflect.DeepEqual(sum, []int{0, 4, 5}) {
		t.Fatalf("lit %v / %v", src, sum)
	}
	if src, sum := AuditLit(ParseQuery("needle"), "creds", "use github"); src != nil || sum != nil {
		t.Fatalf("a row found by its ID lights %v / %v", src, sum)
	}
	if src, sum := AuditLit(ParseQuery(""), "http", "GET"); src != nil || sum != nil {
		t.Fatalf("an empty query lights %v / %v", src, sum)
	}
}

func TestAuditSearchSaysWhatItLetThrough(t *testing.T) {
	if got := AuditTally(2, 40); got != "2 of 40" {
		t.Fatalf("tally %q", got)
	}
	for _, c := range []struct {
		n         int
		searching bool
		want      string
	}{
		{1, false, "1 newer record below"}, {3, false, "3 newer records below"},
		{1, true, "1 newer match below"}, {0, true, "0 newer matches below"},
	} {
		if got := AuditBelow(c.n, c.searching); got != c.want {
			t.Fatalf("AuditBelow(%d, %v) = %q, want %q", c.n, c.searching, got, c.want)
		}
	}
}
