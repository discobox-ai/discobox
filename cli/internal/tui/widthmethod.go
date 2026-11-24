package tui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// forWidthMethod rewrites a frame so every grapheme cluster in it measures,
// under the renderer's width method, the cells the frame was laid out with.
//
// Everything the window draws is laid out by grapheme cluster: lipgloss
// measures that way, and so does every pane's emulator, whose grid is what the
// program on the far end drew for. Bubble Tea's renderer measures that way
// only once the terminal answers the mode 2027 query; until then — and on the
// Windows 10 console, for good — it measures rune by rune. Where the two
// disagree a line measures short or long to the renderer: an emoji with VS16
// (⚠️, two cells laid out, one rune by rune), a keycap (1️⃣, two and one), a ZWJ
// sequence (a family, two and six). The border beside it shifts, and the
// renderer's later diffs land beside the cells they meant to overwrite.
//
// Such a cluster is drawn as its first rune, padded to the cells it was laid
// out with: the base of an emoji sequence is what most of it says. A first rune
// that cannot stand for it — one the two methods themselves measure
// differently, or wider than the cluster — is drawn as blanks instead. What
// matters more than the glyph is that everything after it stays where the
// layout put it, so the hit map, the cursor and every pane's selection still
// name the cells on screen. Escape sequences and every cluster the methods
// agree on pass through untouched, and under grapheme width nothing is done.
func forWidthMethod(frame string, method ansi.Method) string {
	if method == ansi.GraphemeWidth || isASCII(frame) {
		return frame
	}
	var (
		b     strings.Builder
		state byte
		// kept is how far the frame has been carried into b: nothing is
		// copied until the first cluster that needs rewriting.
		kept int
	)
	for at := 0; at < len(frame); {
		if state == ansi.NormalState && isPrintableASCII(frame[at]) {
			// A run of ASCII text measures the same under every method, all
			// but its last character: marks after that one — a keycap's VS16
			// and U+20E3 — make a cluster of it, which the decoder would read
			// as a character and a cluster of no width of their own.
			end := at + 1
			for end < len(frame) && isPrintableASCII(frame[end]) {
				end++
			}
			if end == len(frame) || frame[end] < utf8.RuneSelf {
				at = end
				continue
			}
			at = end - 1
		}
		seq, width, n, next := ansi.GraphemeWidth.DecodeSequenceInString(frame[at:], state, nil)
		if isControl(seq) {
			state, at = next, at+n
			continue
		}
		cluster := seq
		if seq[0] < utf8.RuneSelf {
			// The ASCII character before a non-ASCII one, taken whole, the
			// way the emulator takes a cluster, at the width it gives it.
			cluster, width = ansi.FirstGraphemeCluster(frame[at:], ansi.GraphemeWidth)
		}
		state = ansi.NormalState
		if ansi.StringWidthWc(cluster) == width {
			at += len(cluster)
			continue
		}
		b.WriteString(frame[kept:at])
		r, _ := utf8.DecodeRuneInString(cluster)
		base := string(r)
		if w := ansi.StringWidthWc(base); w > 0 && w <= width && w == ansi.StringWidth(base) {
			b.WriteString(base)
			width -= w
		}
		b.WriteString(strings.Repeat(" ", width))
		at += len(cluster)
		kept = at
	}
	if kept == 0 {
		return frame
	}
	b.WriteString(frame[kept:])
	return b.String()
}

// isPrintableASCII reports whether a byte is an ASCII character that takes a
// cell.
func isPrintableASCII(c byte) bool { return c >= 0x20 && c < 0x7f }

// isControl reports whether a decoded sequence is an escape sequence or a
// control character rather than text: those occupy no cell under any method,
// and are passed through as they are.
func isControl(seq string) bool {
	// A raw byte from 0x80 to 0x9f is an 8-bit C1 control: no UTF-8 starts
	// with one.
	if c := seq[0]; c < 0x20 || c == 0x7f || (c >= 0x80 && c < 0xa0) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(seq)
	return r >= 0x80 && r < 0xa0
}

// isASCII is the fast path: ASCII measures the same under every method.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
