package tui

import (
	"fmt"
	"strconv"
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

// fillRows grows a block to rows lines, adding empty ones at the bottom. It is
// how a screen whose content is shorter than the window keeps its full height
// without spreading the gap through the middle of what it draws.
func fillRows(block string, rows int) string {
	lines := strings.Split(block, "\n")
	for len(lines) < rows {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
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
// one. Re-asserting the background after each reset is what makes
// discobox-review's full-row selection work over colored content.
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

// usage is what the discobox is costing: its share of the host's cpu, the
// memory it is holding, and the disk it has taken.
//
// The first two empty out when it stops and the third does not. Stopping frees
// cpu and memory; it frees no disk at all, and a stopped discobox is often
// exactly the one whose disk is worth seeing.
//
// Only the cpu is a share. Memory and disk are the amounts themselves, because
// what a row is read for is what this discobox costs beside the one under it,
// and a percentage of the whole machine answers a question about the machine
// instead. The share is still what colors them, so a discobox filling the box
// is noticed without the number having to be read.
//
// A cell nothing has measured is a dot, never a zero: `0%` reads as idle and
// `0 B` as holding nothing, and both are claims about the discobox where a dot
// is a claim about what we know. The two halves are measured on different
// schedules — the counters every report, the disk on the agent's own slower
// sweep — so a discobox can have one and not the other.
func usage(st *styles, s Sandbox) string {
	// The color is the share in every case; only the cpu shows the share as
	// its number.
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
	// One layout for measured and unmeasured alike, so a dot sits in the cell
	// its figure would have, under the label naming it. Two layouts is how the
	// dots came to sit under the wrong columns when the cells were resized.
	cpu, memory, disk := "·", "·", "·"
	// CPU and memory are what a discobox is using, which a stopped one is not:
	// it holds neither, and there is no sample of it to report.
	if s.up() && s.Usage.Known {
		cpu = fmt.Sprintf("%d%%", s.Usage.CPUPercent)
		memory = humanBytes(s.Usage.MemoryBytes)
	}
	// Disk is what it is holding, which it holds whether it runs or not — a
	// stopped discobox has given nothing back. Gating this on running was
	// wrong: it hid exactly the figure a stopped discobox still has, and the
	// agent walks a stopped one's trees precisely so it can be shown.
	if s.Usage.DiskKnown {
		disk = humanBytes(s.Usage.DiskBytes)
	}
	return padANSI(cell(s.Usage.CPUPercent, cpu, 4)+" "+
		cell(s.Usage.MemoryPercent, memory, 8)+" "+
		cell(s.Usage.DiskPercent, disk, 8), usageWidth)
}

// usageHeader labels the three usage cells, in exactly the widths usage draws
// them in, so the labels sit over their own columns.
func usageHeader(st *styles) string {
	return st.dimText.Render(padANSI(pad("cpu", 4)+" "+pad("mem", 8)+" "+pad("disk", 8), usageWidth))
}

// usageWidth fits "100%" and two byte figures with a space between each.
const usageWidth = 22

// trimFloat writes a CPU count the way a person would say it: whole where it is
// whole, one decimal where it is not, so "24" and "4.2" sit beside each other
// without a trailing ".0" on the one that does not need it.
func trimFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
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

// humanBytesPair writes a used-of-total pair sharing one unit — "9.0/32 GiB"
// rather than "9.0 GiB/32 GiB". Both halves are scaled by the total, so the
// smaller number is read against the larger without the unit being said twice
// in a band that has no room to say anything twice.
func humanBytesPair(used, total int64) string {
	const unit = 1024
	if total < unit {
		return fmt.Sprintf("%d/%d B", used, total)
	}
	div, exp := float64(1), 0
	for float64(total)/div >= unit && exp < 4 {
		div *= unit
		exp++
	}
	suffix := [...]string{"B", "KiB", "MiB", "GiB", "TiB"}[exp]
	// The used half keeps a decimal wherever it is small enough to need one,
	// which is exactly when the difference between 0.4 and 4 matters most.
	format := "%.0f"
	if float64(used)/div < 10 {
		format = "%.1f"
	}
	return fmt.Sprintf(format+"/%.0f %s", float64(used)/div, float64(total)/div, suffix)
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
	return strings.Join(fitFields(fields, sep, room), sep)
}

// fitFields is dropToFit's answer before it is joined: which fields survived,
// for the caller that has to know where each of them landed as well as what
// the row says. See Model.statusLine, which marks every offer it draws.
func fitFields(fields []string, sep string, room int) []string {
	for len(fields) > 1 {
		if lipgloss.Width(strings.Join(fields, sep)) <= room {
			return fields
		}
		fields = fields[:len(fields)-1]
	}
	return fields
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

// machineText says how much of the machine Discobox has and how much of it is
// in use — never how much a pool has. A pool is how the system is built: one
// machine's worth of capacity that discoboxes are scheduled into. The person
// reading this window has exactly one and has never heard of it, and what they
// want to know before starting another discobox is how much room is left.
//
// Used covers everything Discobox runs, the discoboxes and the machinery beside
// them both — the shared builder above all, which on a machine mid-build is
// most of it. Which half is busy is what `discobox admin pool resources`
// answers; the question here is only whether there is room.
//
// It gives up rather than truncating. A half-drawn figure is worse than none:
// "cpu 4.2/2" is a wrong number, where an absent readout is merely an absent
// one. Parts drop from the right until what is left fits, so a narrow window
// keeps the cpu and loses the disk.
func machineText(st *styles, r Resources, avail int) string {
	if !r.Known || avail <= 0 {
		return ""
	}
	// Colored on the same thresholds as the row's usage column, so a full
	// machine reads the same wherever it is drawn.
	figure := func(text string, share float64) string {
		switch {
		case share >= 0.9:
			return st.statusER.Render(text)
		case share >= 0.75:
			return st.statusWA.Render(text)
		default:
			return st.dimText.Render(text)
		}
	}
	var parts []string
	if r.CPUCapacity > 0 {
		parts = append(parts, figure(
			fmt.Sprintf("cpu %s/%s", trimFloat(r.CPUVCPUs), trimFloat(r.CPUCapacity)),
			r.CPUVCPUs/r.CPUCapacity))
	}
	if r.MemoryCapacity > 0 {
		parts = append(parts, figure(
			"mem "+humanBytesPair(r.MemoryBytes, r.MemoryCapacity),
			float64(r.MemoryBytes)/float64(r.MemoryCapacity)))
	}
	// Free leads, and what is taken is broken out behind it. Free is the
	// figure that answers whether the next discobox will fit; the split says
	// where the rest went and, more usefully, how much of it is the cache and
	// the builder rather than the discoboxes — which is the half that can be
	// reclaimed without deleting anything somebody made.
	if r.DiskKnown {
		disk := st.dimText.Render("disk " + humanBytes(r.DiskFreeBytes) + " free")
		if r.DiskDataBytes > 0 || r.DiskCacheBytes > 0 {
			disk += st.dimText.Render(fmt.Sprintf(" (%s data, %s cache)",
				humanBytes(r.DiskDataBytes), humanBytes(r.DiskCacheBytes)))
		}
		parts = append(parts, disk)
	}

	// The breakdown is the first thing to give way, before any whole figure
	// goes: a window too narrow for it still has room to say how much is left,
	// which is the part somebody is reading this for.
	if text := strings.Join(parts, st.dimText.Render(" · ")); lipgloss.Width(text) > avail && r.DiskKnown {
		parts[len(parts)-1] = st.dimText.Render("disk " + humanBytes(r.DiskFreeBytes) + " free")
	}
	for len(parts) > 0 {
		text := strings.Join(parts, st.dimText.Render(" · "))
		if lipgloss.Width(text) <= avail {
			return text
		}
		parts = parts[:len(parts)-1]
	}
	return ""
}
