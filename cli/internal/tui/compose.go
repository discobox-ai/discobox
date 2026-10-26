package tui

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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
	done      bool
	width     int
	height    int
}

func newPromptComposer(ctx context.Context) *promptComposer {
	text := textarea.New()
	text.ShowLineNumbers = false
	text.CharLimit = 0
	text.DynamicHeight = true
	text.MinHeight = 2
	text.MaxHeight = 20
	// MaxHeight bounds the viewport, not how much prompt text can be entered.
	text.MaxContentHeight = math.MaxInt
	text.MaxWidth = 0
	text.KeyMap = promptKeyMap()
	text.Placeholder = "Write your prompt…"
	m := &promptComposer{ctx: ctx, text: text, width: 80, height: 24}
	m.layout()
	return m
}

func (m *promptComposer) Init() tea.Cmd { return m.text.Focus() }

func (m *promptComposer) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	defer m.layout()
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
	case tea.KeyPressMsg:
		if m.discard {
			switch keyName(msg) {
			case "y", "Y":
				m.done = true
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
			m.done = true
			return m, tea.Quit
		case "ctrl+c":
			m.done = true
			return m, tea.Quit
		case "ctrl+d":
			m.submitted = m.text.Value()
			m.done = true
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
	if m.done {
		return tea.NewView("")
	}
	return tea.NewView(ansi.Hardwrap("Write a prompt", m.width, true) + "\n\n" + m.text.View() + "\n" + m.footer())
}

// The footer wraps explicitly so the inline renderer can count every row it
// owns. Its actual height also leaves room for every shortcut and error.
func (m *promptComposer) footer() string {
	var b strings.Builder
	if m.discard {
		b.WriteString("Discard this prompt? y discard · n/Esc keep editing")
	} else {
		b.WriteString("Enter newline · Ctrl-D submit · Esc cancel · Alt-E editor")
	}
	if m.status != "" {
		b.WriteString("\n" + m.status)
	}
	return ansi.Hardwrap(b.String(), m.width, true)
}

func (m *promptComposer) layout() {
	headingRows := 2 + (ansi.StringWidth("Write a prompt")-1)/m.width
	footerRows := strings.Count(m.footer(), "\n") + 1
	m.text.MaxHeight = max(1, m.height-headingRows-footerRows-1)
	m.text.SetWidth(max(1, m.width-2))
}
