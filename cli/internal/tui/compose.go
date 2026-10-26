package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
)

// ComposePrompt writes a run's prompt before handing it to the normal creation
// flow. It needs no server or draft state, and works for detached and raw runs.
// Leaving without submitting returns an empty string.
func ComposePrompt(ctx context.Context, in io.Reader, out io.Writer) (string, error) {
	m := newPromptComposer(ctx)
	_, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out)).Run()
	return m.submitted, err
}

type promptComposer struct {
	ctx       context.Context
	text      textarea.Model
	submitted string
	status    string
	discard   bool
}

func newPromptComposer(ctx context.Context) *promptComposer {
	text := textarea.New()
	text.ShowLineNumbers = false
	text.CharLimit = 0
	text.MaxHeight = 0
	text.MaxWidth = 0
	text.KeyMap = promptKeyMap()
	text.Placeholder = "Write your prompt…"
	text.SetWidth(76)
	text.SetHeight(12)
	return &promptComposer{ctx: ctx, text: text}
}

func (m *promptComposer) Init() tea.Cmd { return m.text.Focus() }

func (m *promptComposer) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.text.SetWidth(max(1, msg.Width-2))
		m.text.SetHeight(max(1, msg.Height-5))
	case tea.KeyPressMsg:
		if m.discard {
			switch keyName(msg) {
			case "y", "Y":
				return m, tea.Quit
			case "n", "N", "esc", "ctrl+c":
				m.discard = false
			}
			return m, nil
		}
		switch keyName(msg) {
		case "esc":
			if m.text.Value() != "" {
				m.discard = true
				return m, nil
			}
			return m, tea.Quit
		case "ctrl+c":
			return m, tea.Quit
		case "ctrl+d":
			m.submitted = m.text.Value()
			return m, tea.Quit
		case "alt+e", "f2":
			return m, editPrompt(m.ctx, m.text.Value())
		case "enter", "shift+enter", "ctrl+enter", "alt+enter", "ctrl+j":
			m.text.InsertString("\n")
			return m, nil
		}
	case tea.PasteMsg:
		if m.discard {
			return m, nil
		}
	case editorDoneMsg:
		defer os.Remove(msg.path)
		if msg.err != nil {
			m.status = fmt.Sprintf("editor exited: %v", msg.err)
			return m, nil
		}
		text, err := os.ReadFile(msg.path)
		if err != nil {
			m.status = fmt.Sprintf("cannot read the prompt back: %v", err)
			return m, nil
		}
		m.text.SetValue(string(text))
		m.text.CursorEnd()
		m.status = ""
		return m, nil
	case statusMsg:
		m.status = msg.text
		return m, nil
	}
	var cmd tea.Cmd
	m.text, cmd = m.text.Update(msg)
	return m, cmd
}

func (m *promptComposer) View() tea.View {
	var b strings.Builder
	b.WriteString("Write a prompt\n\n")
	b.WriteString(m.text.View())
	if m.discard {
		b.WriteString("\nDiscard this prompt? y discard · n/Esc keep editing")
	} else {
		b.WriteString("\nEnter newline · Ctrl-D submit · Esc cancel · Alt-E editor")
	}
	if m.status != "" {
		b.WriteString("\n" + m.status)
	}
	v := tea.NewView(b.String())
	v.AltScreen = true
	return v
}
