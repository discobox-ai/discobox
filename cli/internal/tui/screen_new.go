package tui

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Form field indices. The prompt is last so it holds focus by default and its
// enter submits.
const (
	fieldHarness = iota
	fieldPath
	fieldPrompt
	fieldCount
)

// newSessionScreen is the create-a-sandbox form: a harness dropdown, a path
// dropdown, and a prompt field. The prompt is focused by default; pressing enter
// in it creates the sandbox. An empty prompt is valid.
type newSessionScreen struct {
	ctx    context.Context
	ds     DataSource
	keys   keyMap
	styles styles

	width  int
	height int

	loading bool
	loadErr string

	harnesses  []Harness
	paths      []string
	harnessIdx int
	pathIdx    int

	prompt textinput.Model
	focus  int

	open       bool // the focused dropdown is expanded
	listCursor int

	submitting bool
	errText    string
}

func newNewSessionScreen(ctx context.Context, ds DataSource, keys keyMap, st styles) *newSessionScreen {
	ti := textinput.New()
	ti.Placeholder = "describe the task (optional)"
	ti.Prompt = "> "
	ti.SetVirtualCursor(true)
	return &newSessionScreen{
		ctx:     ctx,
		ds:      ds,
		keys:    keys,
		styles:  st,
		loading: true,
		prompt:  ti,
		focus:   fieldPrompt,
	}
}

func (s *newSessionScreen) Init() tea.Cmd {
	return tea.Batch(s.loadCmd(), s.prompt.Focus())
}

// loadCmd fetches the harness and path options off the UI goroutine.
func (s *newSessionScreen) loadCmd() tea.Cmd {
	ds, ctx := s.ds, s.ctx
	return func() tea.Msg {
		harnesses, err := ds.ListHarnesses(ctx)
		if err != nil {
			return newFormDataMsg{err: err}
		}
		paths, err := ds.PathOptions(ctx)
		if err != nil {
			return newFormDataMsg{err: err}
		}
		return newFormDataMsg{harnesses: harnesses, paths: paths}
	}
}

func (s *newSessionScreen) Update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case resizeMsg:
		s.width, s.height = msg.width, msg.height
		s.prompt.SetWidth(max(msg.width-fieldLabelWidth-6, 10))
		return s, nil

	case newFormDataMsg:
		s.applyFormData(msg)
		return s, nil

	case errMsg:
		// A failed create; stay on the form so the user can retry.
		s.submitting = false
		s.errText = msg.err.Error()
		return s, nil

	case tea.KeyPressMsg:
		return s.handleKey(msg)
	}
	// Forward everything else (notably the cursor blink tick) to the prompt.
	var cmd tea.Cmd
	s.prompt, cmd = s.prompt.Update(msg)
	return s, cmd
}

func (s *newSessionScreen) applyFormData(msg newFormDataMsg) {
	s.loading = false
	if msg.err != nil {
		s.loadErr = msg.err.Error()
		return
	}
	s.harnesses = msg.harnesses
	for i, h := range s.harnesses {
		if h.Default {
			s.harnessIdx = i
			break
		}
	}
	// The default path (cwd) leads, followed by distinct existing sources.
	paths := []string{s.ds.DefaultPath()}
	for _, p := range msg.paths {
		if p != "" && p != paths[0] {
			paths = append(paths, p)
		}
	}
	s.paths = paths
}

func (s *newSessionScreen) handleKey(msg tea.KeyPressMsg) (screen, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return s, tea.Quit
	}
	if s.submitting {
		return s, nil
	}
	if s.open {
		return s.handleOpenKey(msg)
	}
	if key.Matches(msg, s.keys.Back) {
		return s, func() tea.Msg { return backMsg{} }
	}
	if s.focus == fieldPrompt {
		return s.handlePromptKey(msg)
	}
	return s.handleSelectorKey(msg)
}

// handlePromptKey routes keys while the text field has focus. Only arrows and
// tab move between fields; every other key (including j/k/h/l) is text, so the
// vim navigation aliases must not fire here.
func (s *newSessionScreen) handlePromptKey(msg tea.KeyPressMsg) (screen, tea.Cmd) {
	switch msg.String() {
	case "enter":
		return s, s.submit()
	case "tab", "down":
		return s, s.focusField(s.focus + 1)
	case "shift+tab", "up":
		return s, s.focusField(s.focus - 1)
	default:
		var cmd tea.Cmd
		s.prompt, cmd = s.prompt.Update(msg)
		return s, cmd
	}
}

// handleSelectorKey routes keys while a dropdown field has focus, where vim
// navigation is welcome since the field holds no text.
func (s *newSessionScreen) handleSelectorKey(msg tea.KeyPressMsg) (screen, tea.Cmd) {
	switch {
	case key.Matches(msg, s.keys.Enter):
		s.open = true
		s.listCursor = s.currentIndex()
		return s, nil
	case key.Matches(msg, s.keys.Tab), key.Matches(msg, s.keys.Down):
		return s, s.focusField(s.focus + 1)
	case key.Matches(msg, s.keys.ShiftTab), key.Matches(msg, s.keys.Up):
		return s, s.focusField(s.focus - 1)
	}
	switch msg.String() {
	case "left", "h":
		s.cycle(-1)
	case "right", "l":
		s.cycle(1)
	}
	return s, nil
}

func (s *newSessionScreen) handleOpenKey(msg tea.KeyPressMsg) (screen, tea.Cmd) {
	switch {
	case key.Matches(msg, s.keys.Up):
		if s.listCursor > 0 {
			s.listCursor--
		}
	case key.Matches(msg, s.keys.Down):
		if s.listCursor < s.optionCount()-1 {
			s.listCursor++
		}
	case key.Matches(msg, s.keys.Enter):
		s.setCurrentIndex(s.listCursor)
		s.open = false
	case key.Matches(msg, s.keys.Back):
		s.open = false
	}
	return s, nil
}

// focusField moves focus to field i (wrapping) and toggles the prompt's cursor.
func (s *newSessionScreen) focusField(i int) tea.Cmd {
	s.open = false
	s.focus = (i + fieldCount) % fieldCount
	if s.focus == fieldPrompt {
		return s.prompt.Focus()
	}
	s.prompt.Blur()
	return nil
}

func (s *newSessionScreen) submit() tea.Cmd {
	s.submitting = true
	s.errText = ""
	req := NewSessionRequest{Harness: s.selectedHarness(), Path: s.selectedPath(), Prompt: s.prompt.Value()}
	ds, ctx := s.ds, s.ctx
	return func() tea.Msg {
		sb, err := ds.CreateSession(ctx, req)
		if err != nil {
			return errMsg{context: "create", err: err}
		}
		return sessionCreatedMsg{sandbox: sb}
	}
}

// optionCount, currentIndex, and setCurrentIndex operate on whichever dropdown
// currently holds focus.
func (s *newSessionScreen) optionCount() int {
	switch s.focus {
	case fieldHarness:
		return len(s.harnesses)
	case fieldPath:
		return len(s.paths)
	}
	return 0
}

func (s *newSessionScreen) currentIndex() int {
	switch s.focus {
	case fieldHarness:
		return s.harnessIdx
	case fieldPath:
		return s.pathIdx
	}
	return 0
}

func (s *newSessionScreen) setCurrentIndex(i int) {
	if i < 0 || i >= s.optionCount() {
		return
	}
	switch s.focus {
	case fieldHarness:
		s.harnessIdx = i
	case fieldPath:
		s.pathIdx = i
	}
}

func (s *newSessionScreen) cycle(delta int) {
	n := s.optionCount()
	if n == 0 {
		return
	}
	s.setCurrentIndex((s.currentIndex() + delta + n) % n)
}

func (s *newSessionScreen) selectedHarness() string {
	if s.harnessIdx >= 0 && s.harnessIdx < len(s.harnesses) {
		return s.harnesses[s.harnessIdx].Name
	}
	return ""
}

func (s *newSessionScreen) selectedPath() string {
	if s.pathIdx >= 0 && s.pathIdx < len(s.paths) {
		return s.paths[s.pathIdx]
	}
	return ""
}

func (s *newSessionScreen) title() string { return "new session" }

func (s *newSessionScreen) helpBindings() []key.Binding {
	return []key.Binding{s.keys.Up, s.keys.Down, s.keys.Enter, s.keys.Back}
}

func (s *newSessionScreen) fullHelpBindings() [][]key.Binding {
	return [][]key.Binding{s.helpBindings()}
}

func (s *newSessionScreen) cursor(int, int) *tea.Cursor { return nil }

const fieldLabelWidth = 8

func (s *newSessionScreen) View(width, height int) string {
	s.width, s.height = width, height
	if s.loading {
		return s.center(width, height, s.styles.status.Render("loading options…"))
	}

	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(s.fieldRow("Harness", s.selectorValue(fieldHarness, s.harnessText())))
	b.WriteString("\n")
	if s.focus == fieldHarness && s.open {
		b.WriteString(s.dropdown(s.harnessLabels()))
	}
	b.WriteString(s.fieldRow("Path", s.selectorValue(fieldPath, s.pathText())))
	b.WriteString("\n")
	if s.focus == fieldPath && s.open {
		b.WriteString(s.dropdown(s.paths))
	}
	b.WriteString(s.fieldRow("Prompt", s.prompt.View()))
	b.WriteString("\n\n")
	b.WriteString(s.statusLine())
	return b.String()
}

func (s *newSessionScreen) fieldRow(label, value string) string {
	return "  " + s.styles.formLabel.Render(padRight(label, fieldLabelWidth)) + "  " + value
}

// selectorValue renders a dropdown field's collapsed box, highlighted when it
// holds focus and marked open/closed with a caret.
func (s *newSessionScreen) selectorValue(field int, text string) string {
	caret := "▾"
	if s.focus == field && s.open {
		caret = "▴"
	}
	box := fmt.Sprintf("[ %s %s ]", text, caret)
	if s.focus == field {
		return s.styles.formActive.Render(box)
	}
	return s.styles.formValue.Render(box)
}

func (s *newSessionScreen) dropdown(options []string) string {
	indent := strings.Repeat(" ", 2+fieldLabelWidth+2)
	var b strings.Builder
	for i, opt := range options {
		b.WriteString(indent)
		if i == s.listCursor {
			b.WriteString(s.styles.dropCursor.Render("› " + opt))
		} else {
			b.WriteString(s.styles.dropItem.Render("  " + opt))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func (s *newSessionScreen) statusLine() string {
	switch {
	case s.submitting:
		return s.styles.status.Render("creating sandbox…")
	case s.errText != "":
		return s.styles.statusError.Render(s.errText)
	case s.loadErr != "":
		return s.styles.statusError.Render(s.loadErr)
	case s.open:
		return s.styles.formHint.Render("↑/↓ choose · enter select · esc close")
	case s.focus == fieldPrompt:
		return s.styles.formHint.Render("enter create · esc cancel · tab/↑↓ move fields")
	default:
		return s.styles.formHint.Render("enter open · ←/→ change · tab/↑↓ move · esc cancel")
	}
}

func (s *newSessionScreen) harnessText() string {
	if s.harnessIdx >= 0 && s.harnessIdx < len(s.harnesses) {
		return s.harnesses[s.harnessIdx].Label
	}
	return "(project default)"
}

func (s *newSessionScreen) pathText() string {
	if p := s.selectedPath(); p != "" {
		return p
	}
	return "(current directory)"
}

func (s *newSessionScreen) harnessLabels() []string {
	labels := make([]string, len(s.harnesses))
	for i, h := range s.harnesses {
		labels[i] = h.Label
	}
	return labels
}

func (s *newSessionScreen) center(width, height int, content string) string {
	if width <= 0 || height <= 0 {
		return content
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}

// padRight pads s with spaces to width runes (never truncates).
func padRight(s string, width int) string {
	if n := width - len([]rune(s)); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}
