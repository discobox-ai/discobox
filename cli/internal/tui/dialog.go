package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type dialogKind int

const (
	dlgMessage dialogKind = iota
	dlgConfirm
	dlgActions
	dlgInput
	dlgText
	// dlgStatus is a dialog nobody answers: it reports what is happening and
	// takes itself down when that finishes. See statusDialog.
	dlgStatus
)

// dialog is the single modal layer. Everything that is not the list or the
// prompt — the action menu, a confirmation, the help — is one of these.
type dialog struct {
	kind  dialogKind
	title string
	body  string
	items []action
	// footer is the line under a menu, which says what choosing one of its
	// rows does. Empty takes the action menu's wording.
	footer string
	cursor int
	offset int
	input  textinput.Model
	err    bool

	// defaultNo makes Enter mean no rather than yes, for a question whose
	// costly answer is yes. Only dlgConfirm reads it.
	defaultNo bool

	// altKey is a second thing a menu row can be asked for, on a key of its
	// own beside the one that runs it: the tools picker's "edit this tool's
	// files". Empty means the menu has only the one action per row. alt is
	// handed the highlighted row's key, since that is what identifies a row to
	// whoever built the menu.
	altKey string
	alt    func(key string) tea.Cmd

	// action receives the result: the chosen action's key, the entered text,
	// or "yes" for a confirmed question. It is not called on cancel.
	action func(result string) tea.Cmd

	// onCancel runs when the dialog is dismissed without answering. Most
	// dialogs have nothing to do there, but a question whose two answers are
	// both answers — carry the uncommitted changes, or do not — has to hear
	// "no" as well as "yes".
	onCancel func() tea.Cmd
}

// action is one row of the action menu, and also the definition of the letter
// that runs it directly from the list.
type action struct {
	key     string
	label   string
	detail  string
	enabled bool
	why     string // why it is not available, when it is not
}

func errorDialog(title, body string) *dialog {
	return &dialog{kind: dlgMessage, title: title, body: body, err: true}
}

func confirmDialog(title, body string, act func(string) tea.Cmd) *dialog {
	return &dialog{kind: dlgConfirm, title: title, body: body, action: act}
}

func actionsDialog(title, body string, items []action, act func(string) tea.Cmd) *dialog {
	d := &dialog{kind: dlgActions, title: title, body: body, items: items, action: act}
	d.cursor = d.nextEnabled(0, 1)
	return d
}

func inputDialog(title, body, placeholder, value string, act func(string) tea.Cmd) *dialog {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.SetValue(value)
	ti.Focus()
	ti.CharLimit = 200
	ti.SetWidth(44)
	ti.Prompt = "› "
	return &dialog{kind: dlgInput, title: title, body: body, input: ti, action: act}
}

// statusDialog reports an operation the user started and now waits on.
//
// It has no answer to give, so it has no buttons and Enter does not dismiss it:
// the thing it describes is what ends it. Escape still cancels, because a wait
// nobody can leave is a trap — and canceling a wait for a discobox that is
// coming up anyway costs the view of it, not the discobox.
func statusDialog(title, body string) *dialog {
	return &dialog{kind: dlgStatus, title: title, body: body}
}

func textDialog(title, body string) *dialog {
	return &dialog{kind: dlgText, title: title, body: body}
}

func (d *dialog) cancel() tea.Cmd {
	if d.onCancel == nil {
		return nil
	}
	return d.onCancel()
}

func (d *dialog) nextEnabled(from, delta int) int {
	if len(d.items) == 0 {
		return 0
	}
	i := from
	for range d.items {
		if i >= 0 && i < len(d.items) && d.items[i].enabled {
			return i
		}
		i += delta
		if i < 0 {
			i = len(d.items) - 1
		}
		if i >= len(d.items) {
			i = 0
		}
	}
	return from
}

// update handles a key press, returning a command and whether to close.
func (d *dialog) update(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if keyName(msg) == "esc" {
		return d.cancel(), true
	}

	switch d.kind {
	case dlgStatus:
		// Nothing to answer. Keys fall through to the window underneath rather
		// than being swallowed by a dialog that has no use for them.
		return nil, false
	case dlgMessage, dlgText:
		switch keyName(msg) {
		case "enter", " ", "q":
			return nil, true
		case "up", "k":
			if d.offset > 0 {
				d.offset--
			}
		case "down", "j":
			d.offset++
		}
		return nil, false

	case dlgConfirm:
		switch strings.ToLower(keyName(msg)) {
		case "enter":
			if d.defaultNo {
				return d.cancel(), true
			}
			if d.action != nil {
				return d.action("yes"), true
			}
			return nil, true
		case "y":
			if d.action != nil {
				return d.action("yes"), true
			}
			return nil, true
		case "n", "q":
			return d.cancel(), true
		}
		return nil, false

	case dlgActions:
		// The second action comes before the row keys, because a menu that
		// offers one has reserved that key from every row of itself.
		if d.altKey != "" && keyName(msg) == d.altKey && d.alt != nil {
			if d.cursor < 0 || d.cursor >= len(d.items) {
				return nil, false
			}
			return d.alt(d.items[d.cursor].key), true
		}
		switch keyName(msg) {
		case "up", "k":
			d.cursor = d.nextEnabled(max(d.cursor-1, 0), -1)
		case "down", "j":
			d.cursor = d.nextEnabled(min(d.cursor+1, len(d.items)-1), 1)
		case "enter":
			if d.cursor < len(d.items) && d.items[d.cursor].enabled && d.action != nil {
				return d.action(d.items[d.cursor].key), true
			}
			return nil, false
		case "q":
			return nil, true
		default:
			for _, it := range d.items {
				if it.key == keyName(msg) && it.enabled && d.action != nil {
					return d.action(it.key), true
				}
			}
		}
		return nil, false

	case dlgInput:
		if keyName(msg) == "enter" {
			if d.action != nil {
				return d.action(strings.TrimSpace(d.input.Value())), true
			}
			return nil, true
		}
		var cmd tea.Cmd
		d.input, cmd = d.input.Update(msg)
		return cmd, false
	}
	return nil, false
}

// How much of the window a dialog takes.
//
// A dialog is the only thing on screen while it is up, so the old fixed 90
// columns left most of a wide terminal empty around a card that was scrolling.
// It takes most of the window instead, and all of a window small enough that a
// margin costs more than the frame gives.
//
// Height is an allowance rather than a size: a dialog grows into it and stops
// at its content, so the config card fills the screen and "Disable Codex?"
// stays the size of the question.
const (
	dialogFillPercent = 90
	// Below these the dialog takes the whole window rather than most of it.
	dialogFullWidthBelow  = 80
	dialogFullHeightBelow = 24
	// dialogMinWidth is the narrowest worth drawing: the action rows put a key,
	// a label column and a reason on one line. A window narrower than this gets
	// all of it, which is the most there is to give.
	dialogMinWidth = 48
)

// dialogWidth and dialogHeight are the box's outside extent, border included.
func dialogWidth(window int) int  { return dialogExtent(window, dialogFullWidthBelow, dialogMinWidth) }
func dialogHeight(window int) int { return dialogExtent(window, dialogFullHeightBelow, 0) }

func dialogExtent(window, fullBelow, minimum int) int {
	if window <= 0 {
		return minimum
	}
	extent := window
	if window >= fullBelow {
		extent = window * dialogFillPercent / 100
	}
	// Never wider than the window: on a terminal narrower than the minimum, the
	// whole of it is all there is.
	return min(max(extent, minimum), window)
}

// dialogChrome is what the box costs around its content: the border and the
// padding either side. See styles.dialog.
const (
	dialogChromeWidth  = 2 + 2*2
	dialogChromeHeight = 2 + 2*1
)

func (d *dialog) view(st *styles, width, height int) string {
	boxWidth := dialogWidth(width)
	inner := max(boxWidth-dialogChromeWidth, 16)

	var b strings.Builder
	titleStyle := st.dialogTitle
	if d.err {
		titleStyle = st.statusER.Bold(true)
	}
	b.WriteString(titleStyle.Render(truncate(d.title, inner)))
	b.WriteString("\n\n")

	// What the body may take: the box's allowance, less its chrome, the title
	// and the blank under it, and the three rows the scroll hint and footer
	// need under it.
	maxBody := max(dialogHeight(height)-dialogChromeHeight-2-3, 3)
	if d.body != "" {
		lines := wrap(d.body, inner)
		// Wrapping is not enough on its own: a line the wrapper could not break
		// — the help text's key columns are one long run of spaces and words —
		// comes back wider than the box, and lipgloss wraps it again into a row
		// the height was not budgeted for. One row over is a frame one row
		// taller than the terminal.
		for i, line := range lines {
			lines[i] = truncate(line, inner)
		}
		if d.kind == dlgText || d.kind == dlgMessage {
			d.offset = min(max(d.offset, 0), max(len(lines)-maxBody, 0))
			end := min(d.offset+maxBody, len(lines))
			b.WriteString(strings.Join(lines[d.offset:end], "\n"))
			if len(lines) > maxBody {
				b.WriteString("\n")
				b.WriteString(st.dimText.Render("  … ↑/↓ to scroll"))
			}
		} else {
			b.WriteString(strings.Join(lines, "\n"))
		}
		b.WriteString("\n")
	}

	switch d.kind {
	case dlgConfirm:
		b.WriteString("\n")
		yes, no := "yes", "no"
		if d.defaultNo {
			no += " (Enter)"
		} else {
			yes += " (Enter)"
		}
		b.WriteString(st.key.Render("y"))
		b.WriteString(" ")
		b.WriteString(yes)
		b.WriteString("   ")
		b.WriteString(st.key.Render("n"))
		b.WriteString(" ")
		b.WriteString(no)
	case dlgStatus:
		b.WriteString("\n")
		b.WriteString(st.dimText.Render("Esc stops watching"))
	case dlgMessage, dlgText:
		b.WriteString("\n")
		b.WriteString(st.dimText.Render("Esc closes"))
	case dlgActions:
		b.WriteString("\n")
		// The label column fits the longest label rather than a fixed width:
		// action names are a word, but the folder dropdown's are paths, and a
		// path cut off at fourteen cells is not a path you can choose between.
		labelW := 14
		for _, it := range d.items {
			labelW = max(labelW, lipgloss.Width(it.label))
		}
		labelW = min(labelW, max(inner-24, 14))
		for i, it := range d.items {
			bar, label := " ", st.dimText
			if i == d.cursor && it.enabled {
				bar, label = st.key.Render("❯"), st.cursorName
			}
			key, detail := st.key.Render(it.key), it.detail
			if !it.enabled {
				key = st.dimText.Render(it.key)
				if it.why != "" {
					detail = it.why
				}
			}
			row := bar + " " + key + "  " + label.Render(pad(it.label, labelW)) + " " +
				st.dimText.Render(truncate(detail, max(inner-labelW-6, 8)))
			b.WriteString(padANSI(row, inner))
			b.WriteString("\n")
		}
		footer := d.footer
		if footer == "" {
			footer = "Enter runs the highlighted action · Esc cancels"
		}
		b.WriteString(st.dimText.Render(truncate(footer, inner)))
	case dlgInput:
		b.WriteString("\n")
		b.WriteString(d.input.View())
		b.WriteString("\n\n")
		b.WriteString(st.dimText.Render("Enter accepts · Esc cancels"))
	}

	return st.dialog.Width(boxWidth).Render(b.String())
}
