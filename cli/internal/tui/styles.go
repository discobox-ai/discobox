package tui

import (
	"image/color"

	"charm.land/bubbles/v2/table"
	"charm.land/lipgloss/v2"
)

// The palette uses the terminal's own ANSI colors (0-15) rather than fixed hex
// values so the UI honors the user's terminal theme in both light and dark
// mode, the way k9s does. Semantic phase colors map onto the standard ANSI
// slots: green for healthy, yellow for in-progress, red for failure, grey for
// dormant.
var (
	colorAccent = lipgloss.Color("6")  // cyan
	colorTitle  = lipgloss.Color("13") // bright magenta
	colorMuted  = lipgloss.Color("8")  // bright black / grey
	colorGood   = lipgloss.Color("2")  // green
	colorWarn   = lipgloss.Color("3")  // yellow
	colorBad    = lipgloss.Color("1")  // red
	colorMark   = lipgloss.Color("11") // bright yellow
	colorText   = lipgloss.Color("15") // bright white
)

type styles struct {
	header        lipgloss.Style
	headerKey     lipgloss.Style
	headerValue   lipgloss.Style
	tab           lipgloss.Style
	tabActive     lipgloss.Style
	tabFocused    lipgloss.Style
	status        lipgloss.Style
	statusError   lipgloss.Style
	table         table.Styles
	paneBorder    lipgloss.Style
	paneTitle     lipgloss.Style
	dialog        lipgloss.Style
	dialogKey     lipgloss.Style
	harnessDialog lipgloss.Style
	newActive     lipgloss.Style
	newInactive   lipgloss.Style
	visualBadge   lipgloss.Style
	formLabel     lipgloss.Style
	formValue     lipgloss.Style
	formActive    lipgloss.Style
	formHint      lipgloss.Style
	dropItem      lipgloss.Style
	dropCursor    lipgloss.Style
}

func defaultStyles() styles {
	tbl := table.DefaultStyles()
	tbl.Header = tbl.Header.
		Foreground(colorAccent).
		Bold(true).
		BorderBottom(true).
		BorderForeground(colorMuted)
	tbl.Selected = tbl.Selected.
		Foreground(colorText).
		Background(colorAccent).
		Bold(true)
	tbl.Cell = tbl.Cell.Foreground(colorText)

	return styles{
		header: lipgloss.NewStyle().
			Foreground(colorTitle).
			Bold(true),
		headerKey: lipgloss.NewStyle().
			Foreground(colorMuted),
		headerValue: lipgloss.NewStyle().
			Foreground(colorText),
		// Tab bar: inactive tabs are muted, the active tab reads in the accent
		// color, and when the bar itself holds focus the active tab reverses to a
		// filled highlight so it is unmistakably where h/l motion applies.
		tab: lipgloss.NewStyle().
			Foreground(colorMuted),
		tabActive: lipgloss.NewStyle().
			Foreground(colorAccent).
			Bold(true),
		tabFocused: lipgloss.NewStyle().
			Foreground(colorText).
			Background(colorAccent).
			Bold(true),
		status: lipgloss.NewStyle().
			Foreground(colorMuted),
		statusError: lipgloss.NewStyle().
			Foreground(colorBad).
			Bold(true),
		table: tbl,
		paneBorder: lipgloss.NewStyle().
			Foreground(colorMuted),
		paneTitle: lipgloss.NewStyle().
			Foreground(colorAccent).
			Bold(true),
		dialog: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorWarn).
			Padding(1, 3),
		dialogKey: lipgloss.NewStyle().
			Foreground(colorWarn).
			Bold(true),
		harnessDialog: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorAccent).
			Padding(1, 3),
		newActive: lipgloss.NewStyle().
			Foreground(colorText).
			Background(colorAccent).
			Bold(true),
		newInactive: lipgloss.NewStyle().
			Foreground(colorGood),
		visualBadge: lipgloss.NewStyle().
			Foreground(colorMark).
			Bold(true),
		formLabel: lipgloss.NewStyle().
			Foreground(colorMuted),
		formValue: lipgloss.NewStyle().
			Foreground(colorText),
		formActive: lipgloss.NewStyle().
			Foreground(colorText).
			Background(colorAccent).
			Bold(true),
		formHint: lipgloss.NewStyle().
			Foreground(colorMuted),
		dropItem: lipgloss.NewStyle().
			Foreground(colorText),
		dropCursor: lipgloss.NewStyle().
			Foreground(colorText).
			Background(colorAccent).
			Bold(true),
	}
}

// stateColor returns the semantic color for a sandbox display state.
func stateColor(state string) color.Color {
	switch state {
	case "running", "ready":
		return colorGood
	case "failed", "error":
		return colorBad
	case "stopped", "deleted", "terminated":
		return colorMuted
	default:
		// starting, stopping, deleting, and anything unknown.
		return colorWarn
	}
}
