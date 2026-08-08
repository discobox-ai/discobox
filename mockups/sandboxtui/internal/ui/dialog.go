package ui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
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
	kind    dialogKind
	title   string
	body    string
	command string // the "would run" preview, shown in a message dialog
	items   []action
	cursor  int
	offset  int
	input   textinput.Model
	err     bool

	// action receives the result: the chosen action's key, the entered text,
	// or "yes" for a confirmed question. It is not called on cancel.
	action func(result string) tea.Cmd
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

func messageDialog(title, body, command string) *dialog {
	return &dialog{kind: dlgMessage, title: title, body: body, command: command}
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
	ti.Width = 44
	ti.Prompt = "› "
	return &dialog{kind: dlgInput, title: title, body: body, input: ti, action: act}
}

func textDialog(title, body string) *dialog {
	return &dialog{kind: dlgText, title: title, body: body}
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
func (d *dialog) update(msg tea.KeyMsg) (tea.Cmd, bool) {
	if msg.String() == "esc" {
		return nil, true
	}

	switch d.kind {
	case dlgMessage, dlgText:
		switch msg.String() {
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
		switch strings.ToLower(msg.String()) {
		case "y", "enter":
			if d.action != nil {
				return d.action("yes"), true
			}
			return nil, true
		case "n", "q":
			return nil, true
		}
		return nil, false

	case dlgActions:
		switch msg.String() {
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
				if it.key == msg.String() && it.enabled && d.action != nil {
					return d.action(it.key), true
				}
			}
		}
		return nil, false

	case dlgInput:
		if msg.String() == "enter" {
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
		if d.command != "" {
			b.WriteString("\n")
			b.WriteString(st.dimText.Render("would run"))
			b.WriteString("\n")
			for _, l := range wrap(d.command, inner) {
				b.WriteString(st.command.Render(l))
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
		b.WriteString(st.dimText.Render("Esc closes"))
	case dlgActions:
		b.WriteString("\n")
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
			row := bar + " " + key + "  " + label.Render(pad(it.label, 14)) + " " +
				st.dimText.Render(truncate(detail, max(inner-20, 8)))
			b.WriteString(padANSI(row, inner))
			b.WriteString("\n")
		}
		b.WriteString(st.dimText.Render("Enter runs the highlighted action · Esc cancels"))
	case dlgInput:
		b.WriteString("\n")
		b.WriteString(d.input.View())
		b.WriteString("\n\n")
		b.WriteString(st.dimText.Render("Enter accepts · Esc cancels"))
	}

	return st.dialog.Width(boxWidth).Render(b.String())
}

// wrap word wraps while honouring existing newlines, so text already laid out
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
		if runewidth.StringWidth(para) <= width {
			out = append(out, para)
			continue
		}
		line := ""
		for _, word := range strings.Fields(para) {
			switch {
			case line == "":
				line = word
			case runewidth.StringWidth(line)+1+runewidth.StringWidth(word) <= width:
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
