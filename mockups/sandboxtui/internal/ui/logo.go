package ui

import (
	_ "embed"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// logo.chars is the disco mark, already styled: it was captured from a
// terminal, so it carries its own colours and inverse-video runs.
//
//go:embed logo.chars
var logoArt string

// logo is the mark as drawn, split into rows with its own width measured in
// display cells rather than bytes.
type logo struct {
	rows  []string
	width int
}

// gutter is the space between the mark and the sandbox list.
const logoGutter = 3

// minWidthForLogo is the terminal width below which the mark is dropped. The
// mark costs the list about thirty columns, and narrower than this the list
// needs them more: decoration is the first thing a narrow terminal loses.
const minWidthForLogo = 100

func newLogo() logo {
	// The capture includes the cursor hide and show it was bracketed by.
	// Leaving them in would take the terminal's cursor with it.
	// The mark is drawn entirely in colour: it is shading, not line art, and
	// stripped of its colours it is a smear of block characters. So a
	// colourless terminal does not get a monochrome version of it, it gets
	// none, and the list takes the columns back.
	if !colorEnabled() {
		return logo{}
	}

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

// view renders the mark as a block of its own width plus the gutter, so it can
// be joined against the list without either side having to know the other's
// geometry.
func (l logo) view() string {
	rows := make([]string, 0, len(l.rows))
	for _, row := range l.rows {
		rows = append(rows, padANSI(row, l.width+logoGutter))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}
