package tui

import (
	"os"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// The palette is discobox-review's, which is git-gui's: lightsalmon,
// lightgreen and gold over a terminal's own background. Nothing here paints a
// full screen background — the panes are told apart by their title bars alone.
//
// They are held as 256-color indices rather than as lipgloss colors because
// highlight() has to write the background escape itself; see the comment there.
// Every index here is 16 or above, where the 256-color cube is fixed. Indices
// below that are whatever the user's theme has redefined them to, so nothing
// that has to be a particular color can be named with one — see colMark.
const (
	colSalmon = "216" // the diffstat's minus side
	colGreen  = "120"
	colGold   = "220"
	colBlack  = "232"
	colGrey   = "245"
	colDim    = "240"
	// Three bands, because a row can be under the cursor, selected, or both,
	// and "both" has to be its own color or the cursor would hide what a
	// command is about to act on.
	colHighlightBG = "237" // under the cursor
	colSelectedBG  = "24"  // a command will act on it
	colBothBG      = "31"  // under the cursor and selected
	colInactive    = "236"
	// The band behind the credential banner: a dark red the text keeps its own
	// colors over, rather than the reverse video the whole bar used to be
	// drawn in. Reversed red puts the terminal's background color on a red
	// field, which is a slab at a glance and a struggle to read at a sentence.
	colAlertBG = "52"
	// The band behind the ready-to-apply banner, the same idea one color round
	// the wheel: a dark green, because the bar it paints is an offer rather
	// than a person waiting. Red for "something is blocked on you" and green
	// for "there is something here to take" is the one distinction the eye
	// makes before it reads either bar.
	colReadyBG = "22"
	// The mark's own purple, which is what the box round the window is drawn
	// in: the window is framed in the color it is branded in rather than in a
	// third accent.
	//
	// It is a hex rather than an index because it has to be that specific
	// color. This was "13" — bright magenta — which lives in the range every
	// terminal theme redefines, so the frame came out Solarized's violet or
	// Gruvbox's purple beside a mark that is drawn in this exact value, and the
	// two never matched. lipgloss downsamples the hex to whatever the terminal
	// can show, which lands on the nearest real color rather than an arbitrary
	// one. Keep it in step with the lit side of the mark in
	// scripts/logo-cells.mjs.
	colMark = "#f45cff"
	colWarn = "214"
	colErr  = "196"
	colOK   = "83"
	colInfo = "111"
)

// detectColor reports whether the terminal will show color at all. NO_COLOR is
// honored here rather than checked for directly: colorprofile reads it, per
// no-color.org, along with everything else that decides a profile.
//
// The profile is a value on the styles rather than a process-wide setting, so a
// test can render both the colored and the plain frame in one run.
//
// The profiles are ordered by how much they can show, and the three at the
// bottom — unknown, not a terminal, and a terminal without color — all mean the
// same thing here, so the test is against the last of them rather than for
// equality with any one.
func detectColor() bool {
	return colorprofile.Detect(os.Stdout, os.Environ()) > colorprofile.ASCII
}

type styles struct {
	// color is the one thing every other decision in the package reads: which
	// glyphs are drawn, whether the mark is drawn at all, and whether a row can
	// carry a background.
	color bool

	headerBar   lipgloss.Style
	headerLabel lipgloss.Style

	titleList lipgloss.Style
	titleDim  lipgloss.Style

	// The cursor is a chevron in the left column and the row it is on takes a
	// background, as discobox-review's file lists do. Painting a background
	// across a row that carries its own colors needs highlight(), not a style.
	cursorName lipgloss.Style

	dimText lipgloss.Style
	rule    lipgloss.Style
	ruleOn  lipgloss.Style

	// frame is the border round the whole window, in the mark's own purple. It
	// is a foreground color rather than a bordered style because the box is
	// drawn by hand; see Model.box.
	frame    lipgloss.Style
	statusOK lipgloss.Style
	statusWA lipgloss.Style
	// initializing is the line under the window while the server sets itself up.
	initializing lipgloss.Style
	statusER     lipgloss.Style
	// The workspace's attention bands: a whole-width bar rather than a colored
	// word, because it has to survive being looked past. It is three styles
	// over one painted band — the mark that catches the eye, the subject, and
	// the key that acts — so the bar reads as a sentence with something to do
	// rather than as a colored slab. The text and the hint are shared by both
	// bands; only the mark and the field behind it say which one this is.
	attentionMark lipgloss.Style
	readyMark     lipgloss.Style
	attentionText lipgloss.Style
	attentionHint lipgloss.Style
	info          lipgloss.Style

	stateRun  lipgloss.Style
	stateBusy lipgloss.Style
	stateOff  lipgloss.Style
	stateErr  lipgloss.Style

	name    lipgloss.Style
	add     lipgloss.Style
	del     lipgloss.Style
	chip    lipgloss.Style
	chipOn  lipgloss.Style
	command lipgloss.Style

	dialog      lipgloss.Style
	dialogTitle lipgloss.Style
	key         lipgloss.Style
	// hover is a control with the pointer on it. It is the accent the keys
	// and the cursor already use, so a live control is picked out in the one
	// color the window picks anything out in — and bold as well, so a terminal
	// with no color still shows it.
	//
	// Not underlined, which is the more obvious spelling of "you can press
	// this": lipgloss renders an underlined run one escape per character,
	// which is ten times the bytes for a hint that changes on every mouse
	// move. Not reverse video either — that is how a selection is drawn, and
	// two things that mean different things must not look the same.
	hover lipgloss.Style
}

func newStyles(color bool) *styles {
	// Without color every style is the identity, so nothing has to check the
	// profile before rendering: what a plain terminal gets is the text.
	paint := func(spec string) lipgloss.Style {
		if !color {
			return lipgloss.NewStyle()
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Color(spec))
	}

	s := &styles{color: color}
	s.headerBar = lipgloss.NewStyle().Bold(true)
	s.headerLabel = paint(colGrey)

	// The list's title bar is the same gold as the cursor and every key hint,
	// so the window has one accent color rather than two competing ones.
	s.titleList = lipgloss.NewStyle().Bold(true).Padding(0, 1)
	s.titleDim = lipgloss.NewStyle().Padding(0, 1)
	if color {
		s.titleList = s.titleList.Foreground(lipgloss.Color(colBlack)).Background(lipgloss.Color(colGold))
		s.titleDim = s.titleDim.Foreground(lipgloss.Color(colGrey)).Background(lipgloss.Color(colInactive))
	}

	s.cursorName = lipgloss.NewStyle().Bold(true)

	s.dimText = paint(colDim)
	s.rule = paint(colInactive)
	s.ruleOn = paint(colDim)

	s.frame = paint(colMark)
	s.statusOK = paint(colOK)
	s.statusWA = paint(colWarn)
	// The same amber the status line uses, because it is the same kind of
	// statement: something is happening that you are not waiting on.
	s.initializing = paint(colWarn)
	s.statusER = paint(colErr)
	s.attentionMark = paint(colWarn).Bold(true)
	s.readyMark = paint(colOK).Bold(true)
	s.attentionText = lipgloss.NewStyle().Bold(true)
	s.attentionHint = paint(colGrey)
	s.info = paint(colInfo)

	s.stateRun = paint(colOK)
	s.stateBusy = paint(colGold)
	s.stateOff = paint(colDim)
	s.stateErr = paint(colErr)

	s.name = lipgloss.NewStyle()
	s.add = paint(colGreen)
	s.del = paint(colSalmon)
	s.chip = paint(colDim)
	s.chipOn = paint(colGold)
	s.command = paint(colGreen)

	s.dialog = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 2)
	if color {
		s.dialog = s.dialog.BorderForeground(lipgloss.Color(colGold))
	}
	s.dialogTitle = paint(colGold).Bold(true)
	s.key = paint(colGold)
	s.hover = paint(colGold).Bold(true)
	return s
}
