package provisionspec

import (
	"strings"
	"testing"
)

func TestParseProgressLine(t *testing.T) {
	for _, tc := range []struct {
		line string
		want Progress
		ok   bool
	}{
		{line: "", ok: false},
		{line: "   ", ok: false},
		{line: "installing @anthropic-ai/claude-code", want: Progress{Detail: "installing @anthropic-ai/claude-code"}, ok: true},
		{line: "percent=62 installing nodejs", want: Progress{Detail: "installing nodejs", Percent: 62}, ok: true},
		{line: "bytes=1024/4096 fetching node.tar.gz", want: Progress{Detail: "fetching node.tar.gz", Done: 1024, Total: 4096}, ok: true},
		{line: "bytes=1024 percent=150", want: Progress{Done: 1024, Percent: 100}, ok: true},
		{line: "percent=-3 x", want: Progress{Detail: "x"}, ok: true},
		// A word that only looks like ours is the detail's.
		{line: "bytes=lots of them", want: Progress{Detail: "bytes=lots of them"}, ok: true},
		{line: "key=value said", want: Progress{Detail: "key=value said"}, ok: true},
		{line: "  folded \t  spaces  ", want: Progress{Detail: "folded spaces"}, ok: true},
		// apt's APT::Status-Fd lines.
		{line: "dlstatus:3:25.0000:Retrieving file 3 of 12", want: Progress{Detail: "retrieving file 3 of 12", Percent: 25}, ok: true},
		{line: "pmstatus:nodejs:62.5:Installing nodejs (amd64)", want: Progress{Detail: "installing nodejs (amd64)", Percent: 62}, ok: true},
		{line: "pmconffile:/etc/x:/etc/x.dpkg-new:'ask'", ok: false},
		{line: "pmerror:pkg:10:failed", ok: false},
		{line: "pmstatus:broken", ok: false},
	} {
		got, ok := ParseProgressLine(tc.line)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseProgressLine(%q) = %+v, %v; want %+v, %v", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseProgressLineBoundsTheDetail(t *testing.T) {
	got, ok := ParseProgressLine(strings.Repeat("é", MaxProgressDetail))
	if !ok || len(got.Detail) > MaxProgressDetail || !strings.HasPrefix(got.Detail, "éé") || strings.ContainsRune(got.Detail, '\uFFFD') {
		t.Fatalf("detail = %q (%d bytes), want at most %d bytes of whole runes", got.Detail, len(got.Detail), MaxProgressDetail)
	}
	got, _ = ParseProgressLine("bad \xff byte")
	if got.Detail != "bad \uFFFD byte" {
		t.Fatalf("detail = %q, want invalid UTF-8 replaced", got.Detail)
	}
}
