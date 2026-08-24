package cli

import "testing"

// A pool host's failure is quoted verbatim into the status line, and those
// arrive with embedded newlines. On a line rewritten in place they are fatal:
// \r\x1b[K erases one row, so every row the text spilled onto stays on the
// screen under whatever the caller drew next.
func TestOneLineFoldsAMultiLineFailure(t *testing.T) {
	got := oneLine("the discobox failed: connect to guest port 3002:\n  Connection reset by peer\nUserInfo={\n}", 0)
	want := "the discobox failed: connect to guest port 3002: Connection reset by peer UserInfo={ }"
	if got != want {
		t.Fatalf("oneLine() = %q, want %q", got, want)
	}
}

// Too long is the same bug without the newlines: the text wraps onto rows the
// erase cannot reach.
func TestOneLineFitsTheTerminal(t *testing.T) {
	const width = 40
	got := oneLine("pulling discobox-harness-claude-code:v0.1.0-alpha.7 — 146.7 MiB of 1.8 GiB, 1/40 layers", width)
	if runeLen(got) > width-1 {
		t.Fatalf("oneLine() is %d runes wide, want at most %d: %q", runeLen(got), width-1, got)
	}
	if got[len(got)-len("…"):] != "…" {
		t.Fatalf("oneLine() = %q, want it to end in an ellipsis", got)
	}
}

func TestOneLineLeavesAFittingLineAlone(t *testing.T) {
	const text = "waiting for a pool to take it"
	if got := oneLine(text, 80); got != text {
		t.Fatalf("oneLine() = %q, want %q", got, text)
	}
}

// An unknown width is not a reason to wrap: off a terminal there is no cursor
// to confuse, and the whole line is the record worth keeping.
func TestOneLineKeepsEverythingWhenWidthIsUnknown(t *testing.T) {
	long := "pulling an image with a very long reference indeed, and rather a lot of layers to go with it"
	if got := oneLine(long, 0); got != long {
		t.Fatalf("oneLine() truncated with no width: %q", got)
	}
}
