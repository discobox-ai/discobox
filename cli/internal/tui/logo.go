package tui

import (
	_ "embed"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// logo.chars is the discobox mark, already styled: it was captured from a
// terminal, so it carries its own colors and inverse-video runs.
//
//go:embed logo.chars
var logoArt string

// logo is the mark as drawn, split into rows with its own width measured in
// display cells rather than bytes.
type logo struct {
	rows  []string
	width int
}

// logoGutter is the space either side of the mark: one gutter between it and
// the box, and one between it and the list. Both, so the mark sits centered in
// the column it reserves rather than flush against whichever side got none.
const logoGutter = 3

// minWidthForLogo is the terminal width below which the mark is dropped. The
// mark costs the list about thirty columns, and narrower than this the list
// needs them more: decoration is the first thing a narrow terminal loses.
const minWidthForLogo = 100

func newLogo(color bool) logo {
	// The mark is drawn entirely in color: it is shading, not line art, and
	// stripped of its colors it is a smear of block characters. So a
	// colorless terminal does not get a monochrome version of it, it gets
	// none, and the list takes the columns back.
	if !color {
		return logo{}
	}

	// The capture includes the cursor hide and show it was bracketed by.
	// Leaving them in would take the terminal's cursor with it.
	art := strings.NewReplacer("\x1b[?25l", "", "\x1b[?25h", "").Replace(logoArt)

	l := logo{}
	for _, row := range strings.Split(strings.Trim(art, "\n"), "\n") {
		// The capture is padded with a blank row top and bottom. The top one
		// would push the mark below the list's title bar, so it goes; the
		// bottom one is the gap above the composer, so it stays.
		if len(l.rows) == 0 && strings.TrimSpace(ansi.Strip(row)) == "" {
			continue
		}
		l.rows = append(l.rows, row)
		l.width = max(l.width, lipgloss.Width(row))
	}
	return l
}

func (l logo) height() int { return len(l.rows) }

// column is the width the mark reserves beside the list: the art plus a gutter
// on each side. The art's own rows keep their alignment to each other — it is a
// picture, not a stack of centered lines — so it is the block that is centered,
// not the rows within it.
func (l logo) column() int {
	if l.height() == 0 {
		return 0
	}
	return l.width + 2*logoGutter
}

// view renders the mark as a block the width of its whole column, so it can be
// joined against the list without either side having to know the other's
// geometry.
//
// It is drawn from the top of the height it is given and padded below. The mark
// is a mark: it belongs at the head of the thing it marks, beside the first rows
// of the list rather than floating halfway down a column of them.
//
// Use viewCentered where the mark is the taller of the two, which is the opening
// window, and centering is what stops it reading as a caption on the mark.
func (l logo) view(height int) string {
	rows := l.paddedRows(height)
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// viewCentered draws the mark in the middle of the height it is given.
func (l logo) viewCentered(height int) string {
	rows := make([]string, 0, max(height, l.height()))
	blank := strings.Repeat(" ", l.column())
	// The remainder of an odd split goes below, so the mark sits a row high
	// rather than a row low, which is where the eye expects something centered
	// against text.
	for range max((height-l.height())/2, 0) {
		rows = append(rows, blank)
	}
	rows = append(rows, l.paddedRows(max(height-len(rows), l.height()))...)
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// paddedRows is the mark's own rows, indented into its column and padded down
// to height.
func (l logo) paddedRows(height int) []string {
	blank := strings.Repeat(" ", l.column())
	indent := strings.Repeat(" ", logoGutter)
	rows := make([]string, 0, max(height, l.height()))
	for _, row := range l.rows {
		rows = append(rows, padANSI(indent+row, l.column()))
	}
	for len(rows) < height {
		rows = append(rows, blank)
	}
	return rows
}
