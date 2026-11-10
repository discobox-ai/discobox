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
