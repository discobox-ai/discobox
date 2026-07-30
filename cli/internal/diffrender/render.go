package diffrender

import (
	"fmt"
	"image/color"
	"io"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Options configures rendering. The zero value renders plain, unpadded,
// unwrapped text, which is what a test or a non-terminal wants.
type Options struct {
	// Width is the terminal width. Zero means unknown: lines are neither
	// wrapped nor padded, so the background of a changed line stops at its text.
	Width int
	// Color enables styling. Off, the sign column carries the whole meaning,
	// which is why there is a sign column at all.
	Color bool
	// Dark selects the palette for a dark terminal background.
	Dark bool
	// TabWidth expands tabs; a tab painted with a background renders as an
	// unstyled gap in most terminals. Zero uses defaultTabWidth.
	TabWidth int
}

const defaultTabWidth = 4

// minGutterWidth keeps the line-number column from jittering between files of
// different lengths.
const minGutterWidth = 3

// Render writes files as a diff meant to be read: a heading per file, line
// numbers, changed lines on colored backgrounds, and the changed part of a
// modified line highlighted within it.
func Render(w io.Writer, files []File, opts Options) error {
	if opts.TabWidth <= 0 {
		opts.TabWidth = defaultTabWidth
	}
	theme := newTheme(opts)
	var out strings.Builder
	for i, file := range files {
		if i > 0 {
			out.WriteString("\n")
		}
		renderFile(&out, file, opts, theme)
	}
	_, err := io.WriteString(w, out.String())
	return err
}

func renderFile(out *strings.Builder, file File, opts Options, theme theme) {
	out.WriteString(fileHeading(file, opts, theme))
	out.WriteString("\n")
	if file.Binary {
		out.WriteString(theme.meta.Render("  binary file"))
		out.WriteString("\n")
		return
	}
	gutter := gutterWidth(file)
	for i, hunk := range file.Hunks {
		// A gap between hunks is information: the lines in between are
		// unchanged, and the reader should not think the file is contiguous.
		if i > 0 {
			out.WriteString(theme.meta.Render(strings.Repeat(" ", gutter) + " ⋯"))
			out.WriteString("\n")
		}
		renderHunk(out, hunk, file.Path, gutter, opts, theme)
	}
}

func fileHeading(file File, opts Options, theme theme) string {
	name := file.Path
	if file.Status == Renamed && file.OldPath != "" && file.OldPath != file.Path {
		name = file.OldPath + " → " + file.Path
	}
	if name == "" {
		name = file.OldPath
	}
	heading := theme.path.Render(name)
	var notes []string
	switch file.Status {
	case AddedFile:
		notes = append(notes, "new file")
	case DeletedFile:
		notes = append(notes, "deleted")
	case Renamed:
		notes = append(notes, "renamed")
	}
	if file.Mode != "" {
		notes = append(notes, "mode "+file.Mode)
	}
	if len(notes) > 0 {
		heading += theme.meta.Render("  (" + strings.Join(notes, ", ") + ")")
	}
	counts := fileCounts(file, theme)
	if counts == "" {
		return heading
	}
	// The counts sit at the right margin when the width is known, and just
	// after the name when it is not.
	gap := 2
	if opts.Width > 0 {
		if room := opts.Width - ansi.StringWidth(heading) - ansi.StringWidth(counts); room > gap {
			gap = room
		}
	}
	return heading + strings.Repeat(" ", gap) + counts
}

func fileCounts(file File, theme theme) string {
	var parts []string
	if file.Added > 0 {
		parts = append(parts, theme.addCount.Render("+"+strconv.Itoa(file.Added)))
	}
	if file.Removed > 0 {
		parts = append(parts, theme.removeCount.Render("-"+strconv.Itoa(file.Removed)))
	}
	return strings.Join(parts, " ")
}

// gutterWidth sizes the line-number column to the largest number the file will
// print, so the numbers stay right-aligned within one file.
func gutterWidth(file File) int {
	largest := 0
	for _, hunk := range file.Hunks {
		for _, line := range hunk.Lines {
			largest = max(largest, line.Old, line.New)
		}
	}
	return max(len(strconv.Itoa(largest)), minGutterWidth)
}

func renderHunk(out *strings.Builder, hunk Hunk, path string, gutter int, opts Options, theme theme) {
	// Tabs are expanded before anything measures, compares, or tokenises a
	// line, so emphasis offsets, syntax offsets, wrap points, and padding all
	// count the same columns.
	texts := make([]string, len(hunk.Lines))
	for i, line := range hunk.Lines {
		texts[i] = expandTabs(line.Text, opts.TabWidth)
	}
	emphasis := pairEmphasis(hunk.Lines, texts)
	var spans [][]span
	if theme.syntax != nil {
		spans = highlightHunk(lexerFor(path), hunk.Lines, texts)
	} else {
		spans = make([][]span, len(hunk.Lines))
	}
	for i, line := range hunk.Lines {
		renderLine(out, line, texts[i], spans[i], emphasis[i], gutter, opts, theme)
	}
}

// renderLine lays out one line as
//
//	[line number][ ][sign][ ][content, padded to the terminal width]
//
// with the background covering everything from the sign onwards, so changed
// lines read as solid bands rather than as ragged text. The line number stays
// outside the band: it is not part of the file.
func renderLine(out *strings.Builder, line Line, text string, spans []span, emph emphasis, gutter int, opts Options, theme theme) {
	number := line.New
	if line.Kind == Removed {
		number = line.Old
	}
	numberText := strings.Repeat(" ", gutter)
	if number > 0 {
		numberText = fmt.Sprintf("%*d", gutter, number)
	}

	sign := " "
	base, emphasized := theme.context, theme.context
	switch line.Kind {
	case Added:
		sign, base, emphasized = "+", theme.add, theme.addEmph
	case Removed:
		sign, base, emphasized = "-", theme.remove, theme.removeEmph
	}

	// The band spans everything after the line number and the space that
	// separates it, so it starts at the sign and runs to the right margin.
	width := 0
	if opts.Width > 0 {
		width = max(opts.Width-gutter-1, 1)
	}
	runs := styleRuns(text, spans, emph, base, emphasized, theme.syntax)
	for i, row := range wrapRuns(runs, width-1) {
		out.WriteString(theme.number.Render(numberText))
		out.WriteString(" ")
		// The sign repeats on a wrapped continuation: without it a long removed
		// line reads as context once it spills over.
		out.WriteString(base.Render(sign))
		content := 0
		for _, run := range row {
			out.WriteString(run.style.Render(string(run.runes)))
			content += ansi.StringWidth(string(run.runes))
		}
		// Only a changed line is padded out to the margin, to complete its band.
		// Padding an unchanged one would append trailing whitespace to every
		// line the reader copies out.
		if width > 0 && line.Kind != Context {
			out.WriteString(base.Render(strings.Repeat(" ", max(width-content-1, 0))))
		}
		if i == 0 && line.NoNewline {
			out.WriteString(theme.meta.Render(" ⏎̸"))
		}
		out.WriteString("\n")
		numberText = strings.Repeat(" ", gutter)
	}
}

// styledRun is a stretch of one line that renders with a single style.
type styledRun struct {
	runes []rune
	style lipgloss.Style
}

// styleRuns cuts a line into the runs the terminal actually needs, resolving
// the two independent channels at every position: the background says what the
// diff did to the line, the foreground says what the code is. They are combined
// rather than chosen between — that is the whole reason a highlighted diff
// reads better than either alone — and a run break happens wherever either one
// changes.
func styleRuns(text string, spans []span, emph emphasis, base, emphasized lipgloss.Style, palette syntaxPalette) []styledRun {
	runes := []rune(text)
	// Resolve each rune's foreground up front. Tokens the palette leaves alone
	// resolve to nil, the same as no token at all, so a run break happens only
	// where the rendered style really differs — not at every token boundary the
	// lexer happened to find.
	colors := make([]color.Color, len(runes))
	if palette != nil {
		for _, s := range spans {
			value, ok := palette.color(s.token)
			if !ok {
				continue
			}
			for at := max(s.start, 0); at < s.end && at < len(runes); at++ {
				colors[at] = value
			}
		}
	}
	type appearance struct {
		emphasized bool
		foreground color.Color
	}
	var runs []styledRun
	var previous appearance
	for at, r := range runes {
		current := appearance{emphasized: at >= emph.start && at < emph.end, foreground: colors[at]}
		if len(runs) == 0 || current != previous {
			style := base
			if current.emphasized {
				style = emphasized
			}
			if current.foreground != nil {
				style = style.Foreground(current.foreground)
			}
			runs = append(runs, styledRun{style: style})
			previous = current
		}
		runs[len(runs)-1].runes = append(runs[len(runs)-1].runes, r)
	}
	if len(runs) == 0 {
		runs = append(runs, styledRun{style: base})
	}
	return runs
}

// wrapRuns breaks a line's runs into rows of at most per runes, splitting a run
// that straddles the break.
//
// Rows are measured in runes rather than display width so that a wide rune
// makes a row narrower than the margin, never wider: nothing overflows, and the
// band's right edge stays where it belongs.
func wrapRuns(runs []styledRun, per int) [][]styledRun {
	if per <= 0 {
		return [][]styledRun{runs}
	}
	var rows [][]styledRun
	var row []styledRun
	used := 0
	for _, run := range runs {
		for len(run.runes) > 0 {
			take := min(per-used, len(run.runes))
			row = append(row, styledRun{runes: run.runes[:take], style: run.style})
			run.runes = run.runes[take:]
			used += take
			if used == per {
				rows, row, used = append(rows, row), nil, 0
			}
		}
	}
	if len(row) > 0 || len(rows) == 0 {
		rows = append(rows, row)
	}
	return rows
}

func expandTabs(text string, tabWidth int) string {
	if !strings.Contains(text, "\t") {
		return text
	}
	var out strings.Builder
	column := 0
	for _, r := range text {
		if r == '\t' {
			spaces := tabWidth - column%tabWidth
			out.WriteString(strings.Repeat(" ", spaces))
			column += spaces
			continue
		}
		out.WriteRune(r)
		column++
	}
	return out.String()
}

// emphasis is the rune range of a line that actually changed, when its
// counterpart on the other side makes that knowable.
type emphasis struct {
	start int
	end   int
}

// pairEmphasis finds, for each line, the span within it that differs from its
// counterpart.
//
// A run of removed lines immediately followed by an equally long run of added
// lines is an edit of those lines, in order; anything else is an insertion or a
// deletion, where there is no counterpart and nothing to highlight.
func pairEmphasis(lines []Line, texts []string) []emphasis {
	out := make([]emphasis, len(lines))
	for i := 0; i < len(lines); {
		removedStart := i
		for i < len(lines) && lines[i].Kind == Removed {
			i++
		}
		removed := i - removedStart
		addedStart := i
		for i < len(lines) && lines[i].Kind == Added {
			i++
		}
		added := i - addedStart
		if removed > 0 && removed == added {
			for offset := range removed {
				before, after := texts[removedStart+offset], texts[addedStart+offset]
				oldSpan, newSpan, ok := changedSpan(before, after)
				if !ok {
					continue
				}
				out[removedStart+offset], out[addedStart+offset] = oldSpan, newSpan
			}
		}
		if removed == 0 && added == 0 {
			i++
		}
	}
	return out
}

// maxEmphasisFraction is how much of a line may differ before highlighting the
// difference stops being useful: past it, nearly the whole line is emphasized,
// which says less than the line's background color already did.
const maxEmphasisFraction = 0.7

// changedSpan trims the common prefix and suffix of two versions of a line and
// reports what is left on each side.
func changedSpan(before, after string) (emphasis, emphasis, bool) {
	oldRunes, newRunes := []rune(before), []rune(after)
	if len(oldRunes) == 0 || len(newRunes) == 0 {
		return emphasis{}, emphasis{}, false
	}
	prefix := 0
	for prefix < len(oldRunes) && prefix < len(newRunes) && oldRunes[prefix] == newRunes[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldRunes)-prefix && suffix < len(newRunes)-prefix &&
		oldRunes[len(oldRunes)-1-suffix] == newRunes[len(newRunes)-1-suffix] {
		suffix++
	}
	oldSpan := emphasis{start: prefix, end: len(oldRunes) - suffix}
	newSpan := emphasis{start: prefix, end: len(newRunes) - suffix}
	oldChanged := float64(oldSpan.end-oldSpan.start) / float64(len(oldRunes))
	newChanged := float64(newSpan.end-newSpan.start) / float64(len(newRunes))
	if oldChanged > maxEmphasisFraction || newChanged > maxEmphasisFraction {
		return emphasis{}, emphasis{}, false
	}
	return oldSpan, newSpan, true
}

// theme is the styles a render uses, resolved once from the options so no
// rendering code has to ask whether color is on.
type theme struct {
	path        lipgloss.Style
	meta        lipgloss.Style
	number      lipgloss.Style
	context     lipgloss.Style
	add         lipgloss.Style
	addEmph     lipgloss.Style
	remove      lipgloss.Style
	removeEmph  lipgloss.Style
	addCount    lipgloss.Style
	removeCount lipgloss.Style
	// syntax is nil when the code itself is left uncolored, which is every
	// uncolored render and any file no lexer recognizes.
	syntax syntaxPalette
}

func newTheme(opts Options) theme {
	if !opts.Color {
		plain := lipgloss.NewStyle()
		return theme{
			path: plain, meta: plain, number: plain, context: plain,
			add: plain, addEmph: plain, remove: plain, removeEmph: plain,
			addCount: plain, removeCount: plain,
		}
	}
	// 256-color indexes rather than RGB: they are what the rest of the CLI uses,
	// and colorprofile downsamples them for a 16-color terminal.
	addBg, addEmphBg := lipgloss.Color("22"), lipgloss.Color("28")
	removeBg, removeEmphBg := lipgloss.Color("52"), lipgloss.Color("88")
	if !opts.Dark {
		addBg, addEmphBg = lipgloss.Color("194"), lipgloss.Color("157")
		removeBg, removeEmphBg = lipgloss.Color("224"), lipgloss.Color("217")
	}
	return theme{
		syntax:      newSyntaxPalette(opts.Dark),
		path:        lipgloss.NewStyle().Bold(true),
		meta:        lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		number:      lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		context:     lipgloss.NewStyle(),
		add:         lipgloss.NewStyle().Background(addBg),
		addEmph:     lipgloss.NewStyle().Background(addEmphBg),
		remove:      lipgloss.NewStyle().Background(removeBg),
		removeEmph:  lipgloss.NewStyle().Background(removeEmphBg),
		addCount:    lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		removeCount: lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
	}
}
