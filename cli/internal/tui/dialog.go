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
		case "y", "enter":
			if d.action != nil {
				return d.action("yes"), true
			}
			return nil, true
		case "n", "q":
			return d.cancel(), true
		}
		return nil, false

	case dlgActions:
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

func (d *dialog) view(st *styles, width int) string {
	boxWidth := min(max(width-4, 24), 90)
	inner := max(boxWidth-6, 16)

	var b strings.Builder
	titleStyle := st.dialogTitle
	if d.err {
		titleStyle = st.statusER.Bold(true)
	}
	b.WriteString(titleStyle.Render(truncate(d.title, inner)))
	b.WriteString("\n\n")

	const maxBody = 18
	if d.body != "" {
		lines := wrap(d.body, inner)
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
		b.WriteString(st.key.Render("y"))
		b.WriteString(" yes   ")
		b.WriteString(st.key.Render("n"))
		b.WriteString(" no")
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
