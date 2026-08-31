package tui

import (
	_ "embed"
	"encoding/json"
	"strings"

	"charm.land/lipgloss/v2"
)

// logo.json is the discobox mark as cells: runs of text carrying explicit
// colors, generated from the terminal capture beside it by
// scripts/logo-cells.mjs.
//
// It is cell data rather than a replayed capture because the capture was not
// safe to replay. It painted with 16-color indices, which every terminal theme
// redefines, so the mark came out whatever purple the user's theme held; and it
// built its solid areas from inverse-video runs, which paint the glyph in the
// terminal's own background, so on a light theme the mark came out speckled.
// Explicit colors fix both, and let lipgloss downsample them to whatever the
// terminal can actually show rather than guessing at capture time.
//
//go:embed logo.json
var logoCells []byte

// logoRun is a piece of a row and the colors it is drawn in. An absent
// foreground means the terminal's own ground: an inverse cell draws its glyph
// in the background color, carving a notch out of the cell, and those notches
// are what give the mark its shape.
type logoRun struct {
	Text string `json:"t"`
	FG   string `json:"f"`
	BG   string `json:"b"`
}

// logoDoc is the mark's on-disk shape: rows of runs, and the width every row
// pads out to.
type logoDoc struct {
	Width int         `json:"width"`
	Rows  [][]logoRun `json:"rows"`
}

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

	var doc logoDoc
	if err := json.Unmarshal(logoCells, &doc); err != nil {
		// The data is embedded and generated, so a parse failure is a build
		// problem, not a runtime one. Draw no mark rather than panic in a TUI.
		return logo{}
	}

	l := logo{}
	for _, row := range doc.Rows {
		rendered := renderLogoRow(row, doc.Width)
		// The mark is padded with a blank row top and bottom. The top one
		// would push it below the list's title bar, so it goes; the bottom one
		// is the gap above the composer, so it stays.
		if len(l.rows) == 0 && strings.TrimSpace(rendered) == "" {
			continue
		}
		l.rows = append(l.rows, rendered)
	}
	if len(l.rows) > 0 {
		l.width = doc.Width
	}
	return l
}

// renderLogoRow draws one row of cells, padded out to the mark's full width so
// every row occupies the same block whatever its runs trimmed to.
func renderLogoRow(row []logoRun, width int) string {
	var b strings.Builder
	drawn := 0
	for _, run := range row {
		style := lipgloss.NewStyle()
		switch {
		case run.FG == "" && run.BG != "":
			// The glyph belongs in the terminal's own background, and no
			// foreground can name that. Reverse video is how a terminal says
			// it: the color goes on as a foreground and the swap puts it
			// behind the glyph, leaving the glyph itself in the ground.
			style = style.Reverse(true).Foreground(lipgloss.Color(run.BG))
		default:
			if run.FG != "" {
				style = style.Foreground(lipgloss.Color(run.FG))
			}
			if run.BG != "" {
				style = style.Background(lipgloss.Color(run.BG))
			}
		}
		b.WriteString(style.Render(run.Text))
		drawn += lipgloss.Width(run.Text)
	}
	if drawn < width {
		b.WriteString(strings.Repeat(" ", width-drawn))
	}
	return b.String()
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
