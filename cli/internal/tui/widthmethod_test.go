package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Every cluster keeps the cells it was laid out with, so the text after it
// sits in the same column under WcWidth as the layout put it in.
func TestForWidthMethod(t *testing.T) {
	family := "\U0001f468\u200d\U0001f469\u200d\U0001f467"
	for _, tc := range []struct {
		name, line, want string
	}{
		{"ascii", "\x1b[1mplain\x1b[m", "\x1b[1mplain\x1b[m"},
		{"agreed", "⏺ ✻ ⎿ ● │ 中 é", "⏺ ✻ ⎿ ● │ 中 é"},
		{"vs16 keeps its style", "\x1b[31m⚠️\x1b[m|", "\x1b[31m⚠ \x1b[m|"},
		{"keycap", "1️⃣|", "1 |"},
		{"zwj", family + "|", "\U0001f468|"},
		{"link kept", "\x1b]8;;https://é.example\x07⚠️\x1b]8;;\x07|", "\x1b]8;;https://é.example\x07⚠ \x1b]8;;\x07|"},
		{"8-bit csi", "\x9b31m⚠️|", "\x9b31m⚠ |"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := forWidthMethod(tc.line, ansi.WcWidth)
			if got != tc.want {
				t.Errorf("forWidthMethod(%q) = %q, want %q", tc.line, got, tc.want)
			}
			if at := strings.LastIndex(tc.line, "|"); at >= 0 {
				laidOut := ansi.StringWidth(tc.line[:at])
				if drawn := ansi.StringWidthWc(got[:strings.LastIndex(got, "|")]); drawn != laidOut {
					t.Errorf("the text after it is at column %d under WcWidth, laid out at %d", drawn, laidOut)
				}
			}
			if same := forWidthMethod(tc.line, ansi.GraphemeWidth); same != tc.line {
				t.Errorf("under grapheme width forWidthMethod changed %q to %q", tc.line, same)
			}
		})
	}
}

func BenchmarkForWidthMethod(b *testing.B) {
	line := "│ " + strings.Repeat("⏺ text with box drawing ─ and ⎿ glyphs ", 4) + "│\n"
	frame := strings.Repeat(line, 50)
	b.SetBytes(int64(len(frame)))
	for b.Loop() {
		forWidthMethod(frame, ansi.WcWidth)
	}
}
