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

// harnessField identifies one row of the harness form. The visible set depends
// on the mode (create vs. edit) and, for create, the chosen source.
type harnessField int

const (
	hfSource harnessField = iota
	hfName
	hfImage
	hfSubmit
)

// harnessFormScreen creates or edits a coding agent (harness config).
//
// Create starts from a "Source" dropdown listing the built-in harness
// definitions plus a "Custom image" entry: picking a definition needs nothing
// more, while a custom agent takes an image. Submitting a create is "Run
// configure" — it creates the config and then runs the agent's configure flow.
// Edit mode exposes only the name, matching what the API allows updating.
// Choosing the project default is done from the agents list, not here.
type harnessFormScreen struct {
	ctx    context.Context
	ds     DataSource
	keys   keyMap
	styles styles

	width  int
	height int

	editID   string // non-empty in edit mode
	editName string // original name shown while definitions load in edit mode

	loading bool
	loadErr string

	definitions []HarnessDefinition
	sourceIdx   int // index into definitions; == len(definitions) means custom

	name  textinput.Model
	image textinput.Model

	fields []harnessField // visible fields, recomputed each key/render
	focus  int            // index into fields

	open       bool // the source dropdown is expanded
	listCursor int

	submitting bool
	errText    string
}

func newHarnessFormScreen(ctx context.Context, ds DataSource, keys keyMap, st styles, edit *HarnessConfig) *harnessFormScreen {
	name := textinput.New()
	name.Placeholder = "name"
	name.Prompt = ""
	name.SetVirtualCursor(true)
	image := textinput.New()
	image.Placeholder = "registry/image:tag"
	image.Prompt = ""
	image.SetVirtualCursor(true)

	s := &harnessFormScreen{
		ctx:     ctx,
		ds:      ds,
		keys:    keys,
		styles:  st,
		loading: true,
		name:    name,
		image:   image,
	}
	if edit != nil {
		s.editID = edit.ID
		s.editName = harnessDisplayName(*edit)
		s.name.SetValue(edit.Name)
		s.image.SetValue(edit.Image)
	}
	s.rebuildFields()
	return s
}

func (s *harnessFormScreen) isEdit() bool { return s.editID != "" }

func (s *harnessFormScreen) isCustom() bool {
	if s.isEdit() {
		return false // edit keeps the existing source; image/slug stay read-only
	}
	return s.sourceIdx >= len(s.definitions)
}

func (s *harnessFormScreen) Init() tea.Cmd {
	return s.loadCmd()
}

func (s *harnessFormScreen) loadCmd() tea.Cmd {
	ds, ctx := s.ds, s.ctx
	return func() tea.Msg {
		defs, err := ds.ListHarnessDefinitions(ctx)
		if err != nil {
			return harnessFormDataMsg{err: err}
		}
		return harnessFormDataMsg{definitions: defs}
	}
}

func (s *harnessFormScreen) Update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case resizeMsg:
		s.width, s.height = msg.width, msg.height
		s.setInputWidths()
		return s, nil

	case harnessFormDataMsg:
		s.applyFormData(msg)
		return s, nil

	case errMsg:
		// A failed save; stay on the form so the user can retry.
		s.submitting = false
		s.errText = msg.err.Error()
		return s, nil

	case tea.KeyPressMsg:
		return s.handleKey(msg)
	}
	// Forward everything else (notably the cursor blink tick) to the focused input.
	return s, s.updateFocusedInput(msg)
}

func (s *harnessFormScreen) applyFormData(msg harnessFormDataMsg) {
	s.loading = false
	if msg.err != nil {
		s.loadErr = msg.err.Error()
		return
	}
	s.definitions = msg.definitions
	// Create mode defaults to a custom image only when there are no definitions;
	// otherwise it starts on the first definition.
	if !s.isEdit() {
		s.sourceIdx = 0
	}
	s.rebuildFields()
	s.setInputWidths()
}

// rebuildFields recomputes the visible field set and clamps focus into it.
func (s *harnessFormScreen) rebuildFields() {
	switch {
	case s.isEdit():
		s.fields = []harnessField{hfName, hfSubmit}
	case s.isCustom():
		s.fields = []harnessField{hfSource, hfImage, hfSubmit}
	default:
		s.fields = []harnessField{hfSource, hfSubmit}
	}
	if s.focus >= len(s.fields) {
		s.focus = len(s.fields) - 1
	}
	if s.focus < 0 {
		s.focus = 0
	}
	s.syncInputFocus()
}

// syncInputFocus focuses the textinput matching the focused field and blurs the
// rest, so only the active field shows a cursor.
func (s *harnessFormScreen) syncInputFocus() {
	s.name.Blur()
	s.image.Blur()
	switch s.currentField() {
	case hfName:
		s.name.Focus()
	case hfImage:
		s.image.Focus()
	}
}

func (s *harnessFormScreen) currentField() harnessField {
	if s.focus < 0 || s.focus >= len(s.fields) {
		return hfSubmit
	}
	return s.fields[s.focus]
}

func (s *harnessFormScreen) handleKey(msg tea.KeyPressMsg) (screen, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return s, tea.Quit
	}
	if s.submitting || s.loading {
		if key.Matches(msg, s.keys.Back) && !s.submitting {
			return s, backToHarnessesCmd()
		}
		return s, nil
	}
	if s.open {
		return s.handleOpenKey(msg)
	}
	// Esc leaves the form.
	if key.Matches(msg, s.keys.Back) {
		return s, backToHarnessesCmd()
	}
	switch s.currentField() {
	case hfSource:
		return s.handleSourceKey(msg)
	case hfSubmit:
		return s.handleSubmitKey(msg)
	default: // text fields
		return s.handleTextKey(msg)
	}
}

// handleTextKey routes keys while a text field has focus. Only tab/arrows move
// between fields; every other key is text, so vim aliases must not fire.
func (s *harnessFormScreen) handleTextKey(msg tea.KeyPressMsg) (screen, tea.Cmd) {
	switch msg.String() {
	case "enter":
		return s, s.submit()
	case "tab", "down":
		return s, s.focusField(s.focus + 1)
	case "shift+tab", "up":
		return s, s.focusField(s.focus - 1)
	default:
		return s, s.updateFocusedInput(msg)
	}
}

func (s *harnessFormScreen) handleSourceKey(msg tea.KeyPressMsg) (screen, tea.Cmd) {
	switch {
	case key.Matches(msg, s.keys.Enter):
		s.open = true
		s.listCursor = s.sourceIdx
		return s, nil
	case key.Matches(msg, s.keys.Tab), key.Matches(msg, s.keys.Down):
		return s, s.focusField(s.focus + 1)
	case key.Matches(msg, s.keys.ShiftTab), key.Matches(msg, s.keys.Up):
		return s, s.focusField(s.focus - 1)
	}
	switch msg.String() {
	case "left", "h":
		s.cycleSource(-1)
	case "right", "l":
		s.cycleSource(1)
	}
	return s, nil
}

func (s *harnessFormScreen) handleSubmitKey(msg tea.KeyPressMsg) (screen, tea.Cmd) {
	switch {
	case key.Matches(msg, s.keys.Enter):
		return s, s.submit()
	case key.Matches(msg, s.keys.Tab), key.Matches(msg, s.keys.Down):
		return s, s.focusField(s.focus + 1)
	case key.Matches(msg, s.keys.ShiftTab), key.Matches(msg, s.keys.Up):
		return s, s.focusField(s.focus - 1)
	}
	return s, nil
}

func (s *harnessFormScreen) handleOpenKey(msg tea.KeyPressMsg) (screen, tea.Cmd) {
	switch {
	case key.Matches(msg, s.keys.Up):
		if s.listCursor > 0 {
			s.listCursor--
		}
	case key.Matches(msg, s.keys.Down):
		if s.listCursor < s.sourceCount()-1 {
			s.listCursor++
		}
	case key.Matches(msg, s.keys.Enter):
		s.sourceIdx = s.listCursor
		s.open = false
		s.rebuildFields()
		s.setInputWidths()
	case key.Matches(msg, s.keys.Back):
		s.open = false
	}
	return s, nil
}

// focusField moves focus to field i (wrapping) and re-syncs input focus.
func (s *harnessFormScreen) focusField(i int) tea.Cmd {
	s.open = false
	n := len(s.fields)
	if n == 0 {
		return nil
	}
	s.focus = (i + n) % n
	s.syncInputFocus()
	return nil
}

func (s *harnessFormScreen) updateFocusedInput(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	switch s.currentField() {
	case hfName:
		s.name, cmd = s.name.Update(msg)
	case hfImage:
		s.image, cmd = s.image.Update(msg)
	}
	return cmd
}

func (s *harnessFormScreen) sourceCount() int { return len(s.definitions) + 1 }

func (s *harnessFormScreen) cycleSource(delta int) {
	n := s.sourceCount()
	s.sourceIdx = (s.sourceIdx + delta + n) % n
	s.rebuildFields()
	s.setInputWidths()
}

func (s *harnessFormScreen) submit() tea.Cmd {
	req := SaveHarnessRequest{ID: s.editID}
	if s.isEdit() {
		req.Name = strings.TrimSpace(s.name.Value())
	} else if s.isCustom() {
		req.Image = strings.TrimSpace(s.image.Value())
		if req.Image == "" {
			s.errText = "a custom coding agent needs an image"
			return nil
		}
	} else if def, ok := s.selectedDefinition(); ok {
		req.DefinitionID = def.ID
	}
	created := !s.isEdit()
	s.submitting = true
	s.errText = ""
	ds, ctx := s.ds, s.ctx
	return func() tea.Msg {
		cfg, err := ds.SaveHarness(ctx, req)
		if err != nil {
			return errMsg{context: "save agent", err: err}
		}
		return harnessSavedMsg{config: cfg, created: created}
	}
}

func (s *harnessFormScreen) selectedDefinition() (HarnessDefinition, bool) {
	if s.sourceIdx < 0 || s.sourceIdx >= len(s.definitions) {
		return HarnessDefinition{}, false
	}
	return s.definitions[s.sourceIdx], true
}

func (s *harnessFormScreen) title() string {
	if s.isEdit() {
		return "edit coding agent"
	}
	return "new coding agent"
}

func (s *harnessFormScreen) helpBindings() []key.Binding {
	return []key.Binding{s.keys.Up, s.keys.Down, s.keys.Enter, s.keys.Back}
}

func (s *harnessFormScreen) fullHelpBindings() [][]key.Binding {
	return [][]key.Binding{s.helpBindings()}
}

func (s *harnessFormScreen) cursor(int, int) *tea.Cursor { return nil }

const (
	harnessLabelWidth = 8
	// harnessInputWidth is the text-field width inside the form dialog. It is
	// fixed so the dialog keeps a stable, modest size rather than stretching to
	// the full terminal width.
	harnessInputWidth = 30
)

func (s *harnessFormScreen) setInputWidths() {
	w := harnessInputWidth
	if s.width > 0 {
		if limit := s.width - harnessLabelWidth - 12; limit < w {
			w = limit
		}
	}
	if w < 10 {
		w = 10
	}
	s.name.SetWidth(w)
	s.image.SetWidth(w)
}

func (s *harnessFormScreen) View(width, height int) string {
	s.width, s.height = width, height
	if s.loading {
		return s.center(width, height, s.styles.status.Render("loading definitions…"))
	}

	var b strings.Builder
	b.WriteString(s.styles.paneTitle.Render(strings.ToUpper(s.title()[:1]) + s.title()[1:]))
	b.WriteString("\n\n")
	for _, field := range s.fields {
		b.WriteString(s.renderField(field))
		b.WriteString("\n")
		if field == hfSource && s.open {
			b.WriteString(s.dropdown())
		}
	}
	b.WriteString("\n")
	b.WriteString(s.statusLine())
	return s.center(width, height, dialogBox(s.styles.harnessDialog, width, b.String()))
}

func (s *harnessFormScreen) renderField(field harnessField) string {
	switch field {
	case hfSource:
		return s.fieldRow("Source", s.selectorValue(s.sourceText()))
	case hfName:
		return s.fieldRow("Name", s.inputValue(hfName, s.name.View()))
	case hfImage:
		return s.fieldRow("Image", s.inputValue(hfImage, s.image.View()))
	case hfSubmit:
		return s.fieldRow("", s.submitValue())
	}
	return ""
}

func (s *harnessFormScreen) fieldRow(label, value string) string {
	return "  " + s.styles.formLabel.Render(padRight(label, harnessLabelWidth)) + "  " + value
}

// inputValue frames a text field, highlighting the label side when focused. The
// textinput itself renders the cursor, so we only mark focus with a caret.
func (s *harnessFormScreen) inputValue(field harnessField, view string) string {
	marker := "  "
	if s.currentField() == field {
		marker = s.styles.formActive.Render("› ")
	}
	return marker + view
}

func (s *harnessFormScreen) selectorValue(text string) string {
	caret := "▾"
	if s.open {
		caret = "▴"
	}
	box := fmt.Sprintf("[ %s %s ]", text, caret)
	if s.currentField() == hfSource {
		return s.styles.formActive.Render(box)
	}
	return s.styles.formValue.Render(box)
}

func (s *harnessFormScreen) submitValue() string {
	label := "[ Run configure ]"
	if s.isEdit() {
		label = "[ Save ]"
	}
	if s.currentField() == hfSubmit {
		return s.styles.formActive.Render(label)
	}
	return s.styles.formValue.Render(label)
}

func (s *harnessFormScreen) dropdown() string {
	indent := strings.Repeat(" ", 2+harnessLabelWidth+2)
	var b strings.Builder
	for i, opt := range s.sourceLabels() {
		b.WriteString(indent)
		if i == s.listCursor {
			b.WriteString(s.styles.dropCursor.Render("› " + opt))
		} else {
			b.WriteString(s.styles.dropItem.Render("  " + opt))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (s *harnessFormScreen) sourceText() string {
	if s.isCustom() {
		return "Custom image"
	}
	if def, ok := s.selectedDefinition(); ok {
		return def.Name
	}
	return "Custom image"
}

func (s *harnessFormScreen) sourceLabels() []string {
	labels := make([]string, 0, s.sourceCount())
	for _, def := range s.definitions {
		label := def.Name
		if desc := strings.TrimSpace(def.Description); desc != "" {
			label = fmt.Sprintf("%s — %s", def.Name, desc)
		}
		labels = append(labels, label)
	}
	return append(labels, "Custom image")
}

func (s *harnessFormScreen) statusLine() string {
	switch {
	case s.submitting:
		return s.styles.status.Render("saving coding agent…")
	case s.errText != "":
		return s.styles.statusError.Render(s.errText)
	case s.loadErr != "":
		return s.styles.statusError.Render(s.loadErr)
	case s.open:
		return s.styles.formHint.Render("↑/↓ choose · enter select · esc close")
	case s.currentField() == hfSource:
		return s.styles.formHint.Render("enter open · ←/→ change · tab/↑↓ move · esc cancel")
	default:
		return s.styles.formHint.Render("enter save · tab/↑↓ move · esc cancel")
	}
}

func (s *harnessFormScreen) center(width, height int, content string) string {
	if width <= 0 || height <= 0 {
		return content
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}

func backToHarnessesCmd() tea.Cmd {
	return func() tea.Msg { return harnessFormBackMsg{} }
}
