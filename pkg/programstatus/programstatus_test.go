package programstatus

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestParseReadsTheSpecsExample(t *testing.T) {
	msg := "Apply 3 to add, 1 to change, 0 to destroy?"
	report, ok := Parse([]byte("7501;state=blocked:kind=permission:app=terraform:msg=" + b64(msg)))
	if !ok {
		t.Fatal("report refused")
	}
	want := Record{State: Blocked, Kind: Permission, Progress: -1, App: "terraform", Msg: msg}
	if report.Query || report.Clear || report.Record != want {
		t.Fatalf("report = %+v, want %+v", report, want)
	}
}

func TestParseQuery(t *testing.T) {
	for _, body := range []string{"7501;?", "?", " ? "} {
		if report, ok := Parse([]byte(body)); !ok || !report.Query {
			t.Errorf("Parse(%q) = %+v, %v; want a query", body, report, ok)
		}
	}
}

func TestParseKeepsWhatIsValidAndSkipsWhatIsNot(t *testing.T) {
	report, ok := Parse([]byte(" state = working : noequals : BAD=x : progress=42 : future=thing : app=a b : state=idle"))
	if !ok {
		t.Fatal("report refused")
	}
	// The last state wins, progress is dropped with it (idle carries none),
	// and the app with a space is not in the alphabet.
	if report.Record.State != Idle || report.Record.Progress != -1 || report.Record.App != "" {
		t.Fatalf("record = %+v", report.Record)
	}
}

func TestParseFieldsMeaningfulOnlyWithSomeStates(t *testing.T) {
	report, _ := Parse([]byte("state=working:kind=permission:progress=40"))
	if report.Record.Kind != "" || report.Record.Progress != 40 {
		t.Fatalf("working = %+v", report.Record)
	}
	report, _ = Parse([]byte("state=blocked:kind=nonsense:progress=101"))
	if report.Record.Kind != "" || report.Record.Progress != -1 {
		t.Fatalf("blocked = %+v", report.Record)
	}
	report, _ = Parse([]byte("state=done:progress=10"))
	if report.Record.Progress != -1 {
		t.Fatalf("done = %+v", report.Record)
	}
}

func TestParseDiscardsWhole(t *testing.T) {
	for name, body := range map[string]string{
		"no state":         "app=x",
		"unknown state":    "state=sleeping",
		"bad id":           "state=idle:id=a//b",
		"deep id":          "state=idle:id=" + strings.Repeat("a/", 8) + "a",
		"long segment":     "state=idle:id=" + strings.Repeat("a", 33),
		"invalid base64":   "state=idle:msg=a,b",
		"control in msg":   "state=idle:msg=" + b64("one\ntwo"),
		"C1 in title":      "state=idle:title=" + b64("a\u0085b"),
		"not utf-8":        "state=idle:msg=" + base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}),
		"msg too long":     "state=idle:msg=" + b64(strings.Repeat("x", MaxMsg+1)),
		"title too long":   "state=idle:title=" + b64(strings.Repeat("x", MaxTitle+1)),
		"sequence too big": "state=idle:pad=" + strings.Repeat("x", 4096),
		// A byte outside the alphabet would otherwise drop the id and make
		// the report the root's, or drop the text and replace it with none.
		"id outside the alphabet":    "state=clear:id=sub@agent",
		"msg outside the alphabet":   "state=idle:msg=a@b",
		"title outside the alphabet": "state=idle:title=a b",
	} {
		if report, ok := Parse([]byte(body)); ok {
			t.Errorf("%s: Parse kept %+v", name, report)
		}
	}
}

func TestParseTakesUnpaddedBase64(t *testing.T) {
	report, ok := Parse([]byte("state=idle:msg=" + strings.TrimRight(b64("ab"), "=")))
	if !ok || report.Record.Msg != "ab" {
		t.Fatalf("report = %+v, %v", report, ok)
	}
}

func TestParseClear(t *testing.T) {
	report, ok := Parse([]byte("state=clear:id=build:app=x"))
	if !ok || !report.Clear || report.Record.ID != "build" {
		t.Fatalf("report = %+v, %v", report, ok)
	}
}

func TestSequenceRoundTrips(t *testing.T) {
	rec := Record{ID: "ignored/child", State: Blocked, Kind: Question, Progress: 30, App: "claude-code", Title: "box one", Msg: "Which branch?"}
	seq := rec.Sequence()
	if !strings.HasPrefix(seq, "\x1b]7501;") || !strings.HasSuffix(seq, "\x1b\\") {
		t.Fatalf("sequence = %q", seq)
	}
	report, ok := Parse([]byte(strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b]"), "\x1b\\")))
	if !ok {
		t.Fatalf("own sequence refused: %q", seq)
	}
	rec.ID = ""
	if report.Record != rec {
		t.Fatalf("round trip = %+v, want %+v", report.Record, rec)
	}
}

func TestSequenceCutsAndCleansText(t *testing.T) {
	rec := Record{State: Idle, Progress: -1, Msg: "a\u202eb\x1bc" + strings.Repeat("é", MaxMsg)}
	report, ok := Parse([]byte(strings.TrimSuffix(strings.TrimPrefix(rec.Sequence(), "\x1b]"), "\x1b\\")))
	if !ok {
		t.Fatal("own sequence refused")
	}
	if !strings.HasPrefix(report.Record.Msg, "abc") || len(report.Record.Msg) > MaxMsg {
		t.Fatalf("msg = %q (%d bytes)", report.Record.Msg[:8], len(report.Record.Msg))
	}
}

func TestDisplayable(t *testing.T) {
	if got := Displayable("ok\u200b\u2066x\x07", 0); got != "okx" {
		t.Fatalf("Displayable = %q", got)
	}
	if got := Displayable("ééé", 5); got != "éé" {
		t.Fatalf("cut = %q", got)
	}
}

func apply(t *testing.T, s *Set, body string, group int) {
	t.Helper()
	report, ok := Parse([]byte(body))
	if !ok {
		t.Fatalf("refused %q", body)
	}
	s.Apply(report, time.Unix(int64(s.Len()), 0), group)
}

func TestSetReplacesWhole(t *testing.T) {
	var s Set
	apply(t, &s, "state=working:app=claude-code:msg="+b64("thinking"), 1)
	apply(t, &s, "state=idle", 1)
	got, ok := s.Effective()
	if !ok || got.State != Idle || got.App != "" || got.Msg != "" {
		t.Fatalf("effective = %+v, %v", got, ok)
	}
}

func TestSetRootStandsForTheTerminal(t *testing.T) {
	var s Set
	apply(t, &s, "state=working:app=deploy", 1)
	apply(t, &s, "state=blocked:id=eu", 1)
	if got, _ := s.Effective(); got.State != Working {
		t.Fatalf("effective = %+v, want the root", got)
	}
}

func TestSetWithoutRootPicksTheMostPressing(t *testing.T) {
	var s Set
	apply(t, &s, "state=working:id=build:app=cargo", 1)
	apply(t, &s, "state=done:id=build/test", 1)
	apply(t, &s, "state=blocked:id=build/approve:kind=auth", 1)
	got, _ := s.Effective()
	if got.State != Blocked || got.Kind != Auth || got.App != "cargo" {
		t.Fatalf("effective = %+v, want blocked inheriting cargo", got)
	}
}

func TestSetClear(t *testing.T) {
	var s Set
	apply(t, &s, "state=working:id=build", 1)
	apply(t, &s, "state=working:id=build/test", 1)
	apply(t, &s, "state=working:id=builder", 1)
	apply(t, &s, "state=clear:id=build", 1)
	if s.Len() != 1 {
		t.Fatalf("after clearing build: %d records, want builder alone", s.Len())
	}
	apply(t, &s, "state=clear", 1)
	if _, ok := s.Effective(); ok {
		t.Fatal("a clear with no id left records")
	}
}

func TestSetEvictsTheLeastRecentlyUpdated(t *testing.T) {
	var s Set
	for i := range MaxRecords {
		apply(t, &s, "state=idle:id=r"+strconv.Itoa(i), 1)
	}
	apply(t, &s, "state=idle:id=r0", 1)
	apply(t, &s, "state=idle:id=extra", 1)
	if s.Len() != MaxRecords {
		t.Fatalf("len = %d", s.Len())
	}
	if _, ok := s.records["r1"]; ok {
		t.Fatal("r1 was the least recently updated and should be gone")
	}
	if _, ok := s.records["r0"]; !ok {
		t.Fatal("r0 was updated again and should stay")
	}
}

func TestSetDropLive(t *testing.T) {
	var s Set
	apply(t, &s, "state=working:id=a", 1)
	apply(t, &s, "state=done:id=b", 1)
	apply(t, &s, "state=blocked:id=c", 2)
	s.Drop(func(state State, group int) bool { return group == 1 && Live(state) })
	if s.Len() != 2 {
		t.Fatalf("len = %d, want b (done) and c (another group)", s.Len())
	}
}

func TestQueryIsNotAReport(t *testing.T) {
	var s Set
	apply(t, &s, "?", 1)
	if s.Len() != 0 || !s.reportedAt.IsZero() {
		t.Fatal("a query changed the set")
	}
}
