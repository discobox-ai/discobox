package ui

import (
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// applyColorPreference honours NO_COLOR, per no-color.org: the variable being
// present and non-empty is the whole signal, whatever it is set to. It forces
// the renderer down to plain ASCII, which is what every style in this package
// and the mark itself then read to decide what to emit.
//
// A terminal that cannot do colour at all is already handled: lipgloss detects
// that, and the profile it picks says so.
func applyColorPreference() {
	if os.Getenv("NO_COLOR") != "" {
		lipgloss.SetColorProfile(termenv.Ascii)
	}
}

// The palette is difftui's, which is git-gui's: lightsalmon, lightgreen and
// gold over a terminal's own background. Nothing here paints a full screen
// background — the panes are told apart by their title bars alone.
var (
	colSalmon = lipgloss.Color("216") // the diffstat's minus side
	colGreen  = lipgloss.Color("120")
	colGold   = lipgloss.Color("220")
	colBlack  = lipgloss.Color("232")
	colGrey   = lipgloss.Color("245")
	colDim    = lipgloss.Color("240")
	// Three bands, because a row can be under the cursor, selected, or both,
	// and "both" has to be its own colour or the cursor would hide what a
	// command is about to act on.
	colHighlightBG = lipgloss.Color("237") // under the cursor
	colSelectedBG  = lipgloss.Color("24")  // a command will act on it
	colBothBG      = lipgloss.Color("31")  // under the cursor and selected
	colInactive    = lipgloss.Color("236")
	colWarn        = lipgloss.Color("214")
	colErr         = lipgloss.Color("196")
	colOK          = lipgloss.Color("83")
	colInfo        = lipgloss.Color("111")
)

type styles struct {
	headerBar   lipgloss.Style
	headerLabel lipgloss.Style

	titleList lipgloss.Style
	titleDim  lipgloss.Style

	// The cursor is a chevron in the left column and the row it is on takes a
	// background, as difftui's file lists do. Painting a background across a
	// row that carries its own colours needs highlight(), not a style.
	cursorName lipgloss.Style

	dimText  lipgloss.Style
	sep      lipgloss.Style
	rule     lipgloss.Style
	ruleOn   lipgloss.Style
	statusOK lipgloss.Style
	statusWA lipgloss.Style
	statusER lipgloss.Style
	info     lipgloss.Style

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
}

func newStyles() *styles {
	s := &styles{}
	s.headerBar = lipgloss.NewStyle().Bold(true)
	s.headerLabel = lipgloss.NewStyle().Foreground(colGrey)

	// The list's title bar is the same gold as the cursor and every key hint,
	// so the window has one accent colour rather than two competing ones.
	s.titleList = lipgloss.NewStyle().Foreground(colBlack).Bold(true).
		Padding(0, 1).Background(colGold)
	s.titleDim = lipgloss.NewStyle().Foreground(colGrey).Background(colInactive).Padding(0, 1)

	s.cursorName = lipgloss.NewStyle().Bold(true)

	s.dimText = lipgloss.NewStyle().Foreground(colDim)
	s.sep = lipgloss.NewStyle().Foreground(colDim)
	s.rule = lipgloss.NewStyle().Foreground(colInactive)
	s.ruleOn = lipgloss.NewStyle().Foreground(colDim)
	s.statusOK = lipgloss.NewStyle().Foreground(colOK)
	s.statusWA = lipgloss.NewStyle().Foreground(colWarn)
	s.statusER = lipgloss.NewStyle().Foreground(colErr)
	s.info = lipgloss.NewStyle().Foreground(colInfo)

	s.stateRun = lipgloss.NewStyle().Foreground(colOK)
	s.stateBusy = lipgloss.NewStyle().Foreground(colGold)
	s.stateOff = lipgloss.NewStyle().Foreground(colDim)
	s.stateErr = lipgloss.NewStyle().Foreground(colErr)

	s.name = lipgloss.NewStyle()
	s.add = lipgloss.NewStyle().Foreground(colGreen)
	s.del = lipgloss.NewStyle().Foreground(colSalmon)
	s.chip = lipgloss.NewStyle().Foreground(colDim)
	s.chipOn = lipgloss.NewStyle().Foreground(colGold)
	s.command = lipgloss.NewStyle().Foreground(colGreen)

	s.dialog = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(colGold).Padding(1, 2)
	s.dialogTitle = lipgloss.NewStyle().Foreground(colGold).Bold(true)
	s.key = lipgloss.NewStyle().Foreground(colGold)
	return s
}
