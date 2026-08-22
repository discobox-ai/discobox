package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// pad truncates s to w display cells, padding with spaces when it is shorter.
func pad(s string, w int) string {
	return padANSI(truncate(s, w), w)
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

// hyperlink makes text clickable, by the OSC 8 escape terminals have agreed on
// for it. The sequences occupy no cells, so everything that measures or
// truncates this row goes on measuring the text.
//
// A terminal that does not know OSC 8 shows the text and drops the rest, which
// is the whole reason the label reads as what it is — `8082->8080`, not "open"
// — rather than being a word that only means something where the link works.
func hyperlink(url, text string) string {
	return ansi.SetHyperlink(url) + text + ansi.ResetHyperlink()
}

// truncate shortens text to w display cells without padding.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// highlight paints a background across a row that already carries foreground
// color of its own.
//
// A style cannot do this: every styled span in the row ends in a reset, and a
// reset clears the background with it, so the color would stop at the first
// one. Re-asserting the background after each reset is what makes difftui's
// full-row selection work over colored content.
//
// The palette is 256-color indices throughout, so the sequence is built
// directly. A terminal that cannot color gets nothing back, and the cursor
// shows as its chevron alone.
//
// Both spellings of the reset are re-asserted after: lipgloss writes the short
// one, and text that arrived from anywhere else may carry the long one. Missing
// either leaves the background stopping partway across the row.
func highlight(st *styles, s string, bg string) string {
	if !st.color {
		return s
	}
	seq := "\x1b[48;5;" + bg + "m"
	replacer := strings.NewReplacer(ansiReset, ansiReset+seq, ansiResetShort, ansiResetShort+seq)
	return seq + replacer.Replace(s) + ansiReset
}

const (
	ansiReset      = "\x1b[0m"
	ansiResetShort = "\x1b[m"
)

// trimLeft drops n display cells from the front of plain text.
func trimLeft(s string, n int) string {
	for _, r := range s {
		if n <= 0 {
			break
		}
		n -= ansi.StringWidth(string(r))
		s = s[len(string(r)):]
	}
	return s
}

// usage is what the sandbox is costing: its share of the cpu, the memory and
// the disk it was given. A sandbox that is not up, or one nothing has measured
// yet, has none, and says so rather than showing three zeroes.
func usage(st *styles, s Sandbox) string {
	if !s.up() || !s.Usage.Known {
		return st.dimText.Render(pad("·    ·    ·", usageWidth))
	}
	// The color is the share in every case; only the disk shows something
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
	return padANSI(cell(s.Usage.CPUPercent, fmt.Sprintf("%d%%", s.Usage.CPUPercent), 4)+" "+
		cell(s.Usage.MemoryPercent, fmt.Sprintf("%d%%", s.Usage.MemoryPercent), 4)+" "+
		cell(s.Usage.DiskPercent, humanBytes(s.Usage.DiskBytes), 8), usageWidth)
}

const usageWidth = 18

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

// since is how long ago something was, in one unit and two characters where it
// can manage it: the column is there to rank rows by recency, not to time them.
func since(t time.Time, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// createdText is the column: the sandbox's age with "ago" on it. Creation is
// the one timestamp a user's action put there — nothing yet records real
// access — so the row says how old the discobox is rather than pretending to
// know when it was last touched.
func createdText(s Sandbox, now time.Time) string {
	age := since(s.Created, now)
	if age == "" {
		return ""
	}
	return age + " ago"
}

// renderTitle draws a pane title bar with an optional right aligned suffix.
func renderTitle(style lipgloss.Style, left, right string, w int) string {
	inner := max(w-2, 1) // the styles pad one cell on each side
	text := left
	if right != "" && lipgloss.Width(left)+lipgloss.Width(right)+2 <= inner {
		gap := inner - lipgloss.Width(left) - lipgloss.Width(right)
		text = left + strings.Repeat(" ", gap) + right
	}
	return style.Width(w).Render(truncate(text, inner))
}

// spread lays a left and a right fragment out on one row of the given width.
func spread(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return truncate(left, w)
	}
	return left + strings.Repeat(" ", gap) + right
}

// spreadPin lays a left and a right fragment out on one row like spread, but
// with the right pinned there: a row too narrow for both cuts the left back to
// make room rather than dropping the right. For the caller whose right end
// carries the fact and whose left carries a list of keys with a losable tail.
func spreadPin(left, right string, w int) string {
	room := w - lipgloss.Width(right)
	if room < 1 {
		return truncate(right, w)
	}
	return pad(left, room) + right
}

// centerRoom is how wide a middle fragment may be on a spreadCenter row: the
// row less the fragments on either side, less a cell of air against each of
// them so the middle never touches its neighbors.
//
// A caller that composes its middle out of parts asks for the room first and
// drops parts whole to fit it, rather than handing spreadCenter something it
// has to cut mid-word.
func centerRoom(left, right string, w int) int {
	return w - lipgloss.Width(left) - lipgloss.Width(right) - 2
}

// dropToFit joins fields with sep into a row of at most room columns, dropping
// them whole from the right until they fit rather than cutting one mid-word: a
// narrow window should lose a field, not show half of one.
//
// The first field always survives. It is the one the rest qualify, so a row
// that cannot hold it has nothing worth saying anyway, and the caller's own
// truncation is what deals with that.
func dropToFit(fields []string, sep string, room int) string {
	for len(fields) > 1 {
		if out := strings.Join(fields, sep); lipgloss.Width(out) <= room {
			return out
		}
		fields = fields[:len(fields)-1]
	}
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// spreadCenter lays a left, a middle and a right fragment out on one row, with
// the middle centered in the row itself rather than in the gap between the other
// two — so it does not shift as they change length.
//
// A middle with no room for it is shortened, not dropped. It names what is on
// screen, and a name that silently disappears at some widths is worse than a
// shortened one; that is exactly how the opening hint went missing before it
// moved to a line of its own.
func spreadCenter(left, middle, right string, w int) string {
	if middle == "" {
		return spread(left, right, w)
	}
	leftW, rightW := lipgloss.Width(left), lipgloss.Width(right)
	room := centerRoom(left, right, w)
	if room < 4 {
		return spread(left, right, w)
	}
	middle = truncate(middle, room)
	midW := lipgloss.Width(middle)

	start := max((w-midW)/2, leftW+1)
	if start+midW > w-rightW-1 {
		start = w - rightW - 1 - midW
	}
	row := left + strings.Repeat(" ", start-leftW) + middle
	return row + strings.Repeat(" ", max(w-lipgloss.Width(row)-rightW, 0)) + right
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// shellQuote quotes an argument the way the command preview needs it: only when
// the argument would otherwise be split or interpreted.
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

// wrap word wraps while honoring existing newlines, so text already laid out
// in columns — the help — keeps its alignment.
func wrap(s string, width int) []string {
	if width < 4 {
		width = 4
	}
	var out []string
	for _, para := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if para == "" {
			out = append(out, "")
			continue
		}
		if lipgloss.Width(para) <= width {
			out = append(out, para)
			continue
		}
		line := ""
		for _, word := range strings.Fields(para) {
			switch {
			case line == "":
				line = word
			case lipgloss.Width(line)+1+lipgloss.Width(word) <= width:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
