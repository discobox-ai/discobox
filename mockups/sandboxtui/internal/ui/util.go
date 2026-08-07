package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
)

// pad truncates s to w display cells, padding with spaces when it is shorter.
func pad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return runewidth.FillRight(runewidth.Truncate(s, w, "…"), w)
}

// padANSI pads a string that already carries styling to w display cells.
func padANSI(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return ansi.Truncate(s, w, "…")
}

// truncate shortens plain text to w display cells without padding.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return runewidth.Truncate(s, w, "…")
}

// truncateANSI shortens already styled text, measuring display cells.
func truncateANSI(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// highlight paints a background across a row that already carries foreground
// colour of its own.
//
// A style cannot do this: every styled span in the row ends in a reset, and a
// reset clears the background with it, so the colour would stop at the first
// one. Re-asserting the background after each reset is what makes difftui's
// full-row selection work over coloured content.
func highlight(s string, bg lipgloss.Color) string {
	seq := backgroundSeq(bg)
	if seq == "" {
		return s
	}
	return seq + strings.ReplaceAll(s, ansiReset, ansiReset+seq) + ansiReset
}

const ansiReset = "\x1b[0m"

// colorEnabled reports whether the terminal will show colour at all — either
// because it cannot, or because NO_COLOR said not to.
func colorEnabled() bool { return lipgloss.ColorProfile() != termenv.Ascii }

// backgroundSeq is the escape that sets bg as the background. The palette is
// 256-colour indices throughout, so the sequence can be built directly; a
// terminal that cannot colour gets nothing, and the cursor shows as its
// chevron alone.
func backgroundSeq(bg lipgloss.Color) string {
	if !colorEnabled() {
		return ""
	}
	return "\x1b[48;5;" + string(bg) + "m"
}

// trimLeft drops n display cells from the front of plain text.
func trimLeft(s string, n int) string {
	for _, r := range s {
		if n <= 0 {
			break
		}
		n -= runewidth.RuneWidth(r)
		s = s[len(string(r)):]
	}
	return s
}

// usage is what the sandbox is costing: its share of the cpu, the memory and
// the disk it was given, as three percentages. A sandbox that is not up has
// none, and says so rather than showing three zeroes.
func usage(st *styles, s sandbox) string {
	if !s.up() {
		return st.dimText.Render(pad("·    ·    ·", 18))
	}
	// The colour is the share in every case; only the disk shows something
	// other than the share as its number.
	cell := func(share int, text string, w int) string {
		text = pad(text, w)
		switch {
		case share >= 90:
			return st.statusER.Render(text)
		case share >= 75:
			return st.statusWA.Render(text)
		default:
			return st.dimText.Render(text)
		}
	}
	return padANSI(cell(s.cpu, fmt.Sprintf("%d%%", s.cpu), 4)+" "+
		cell(s.mem, fmt.Sprintf("%d%%", s.mem), 4)+" "+
		cell(s.diskShare, humanBytes(s.disk), 8), 18)
}

// humanBytes writes a byte count the way df -h does, in binary units, with one
// decimal place below 10 so "1.2 GiB" and "15 GiB" both fit the same column.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, exp := float64(n), 0
	for value >= unit && exp < 4 {
		value /= unit
		exp++
	}
	suffix := [...]string{"B", "KiB", "MiB", "GiB", "TiB"}[exp]
	if value < 10 {
		return fmt.Sprintf("%.1f %s", value, suffix)
	}
	return fmt.Sprintf("%.0f %s", value, suffix)
}

// renderTitle draws a pane title bar with an optional right aligned suffix.
func renderTitle(style lipgloss.Style, left, right string, w int) string {
	inner := max(w-2, 1) // the styles pad one cell on each side
	text := left
	if right != "" && runewidth.StringWidth(left)+runewidth.StringWidth(right)+2 <= inner {
		gap := inner - runewidth.StringWidth(left) - runewidth.StringWidth(right)
		text = left + strings.Repeat(" ", gap) + right
	}
	return style.Width(w).Render(truncate(text, inner))
}

// spread lays a left and a right fragment out on one row of the given width.
func spread(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return truncateANSI(left, w)
	}
	return left + strings.Repeat(" ", gap) + right
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// shellQuote quotes an argument the way the "would run" preview needs it: only
// when the argument would otherwise be split or interpreted.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return strings.ContainsRune(" \t\n\"'\\$`*?![]{}()<>|&;#~", r)
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
