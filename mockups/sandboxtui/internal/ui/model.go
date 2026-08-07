// Package ui is a mockup — nothing here talks to a server. It exists to try
// out one idea: the disco launcher opens with the cursor already in a prompt,
// and the sandboxes you already have are one press of Up away.
package ui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type focusArea int

const (
	// The prompt is where the cursor starts. Nothing has to be pressed to
	// begin describing a new sandbox: typing is the default mode.
	focusPrompt focusArea = iota
	focusList
)

// Model is the launcher window.
type Model struct {
	st *styles

	width, height int
	ready         bool

	list   *sandboxList
	prompt textarea.Model
	opts   *optionSet
	logo   logo

	focus       focusArea
	optionsOpen bool
	dialog      *dialog

	project  string
	status   string
	statusEr bool
	quit     bool

	// resize counts drags and statusGen counts messages, so a timer can tell
	// whether it is the last one out.
	resize    int
	statusGen int
}

// Option configures the window at construction. There is one, and it stands
// in for the CLI's persistent -p: the project is a property of the session,
// not something the window changes.
type Option func(*Model)

// Project names the project this session is in. Anything but the default one
// is worth saying in the header, and worth carrying into every command the
// window would run.
func Project(name string) Option {
	return func(m *Model) {
		if name = strings.TrimSpace(name); name != "" {
			m.project = name
		}
	}
}

func New(options ...Option) Model {
	// Before any style or the mark is built: both read the profile it sets.
	applyColorPreference()

	ta := textarea.New()
	// The label above the field already says what an empty prompt does, and
	// the placeholder is gone the moment you type.
	ta.Placeholder = "What should the new sandbox do?"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	// Claude Code's composer: one chevron on the first line, continuation
	// lines aligned under the text rather than under the chevron.
	ta.SetPromptFunc(2, func(line int) string {
		if line == 0 {
			return "❯ "
		}
		return "  "
	})
	ta.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(colGold)
	ta.BlurredStyle.Prompt = lipgloss.NewStyle().Foreground(colDim)
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.Focus()

	m := Model{
		st:      newStyles(),
		list:    newSandboxList(fakeSandboxes()),
		prompt:  ta,
		logo:    newLogo(),
		focus:   focusPrompt,
		project: defaultProject,
	}
	for _, option := range options {
		option(&m)
	}
	m.opts = newOptions(m.project)
	return m
}

func (m Model) Init() tea.Cmd { return textarea.Blink }

// ---------------------------------------------------------------------------
// update

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// A width change is the one thing an inline window cannot redraw its
		// way out of. The terminal reflows the frame already on screen —
		// every line too long for the new width becomes two — while Bubble
		// Tea's renderer still moves the cursor up by the line count it wrote
		// before the reflow. It lands mid-frame and paints a second header
		// under the first, and every further event stacks another.
		//
		// Erasing the screen puts the cursor back at a known origin, but
		// clearing on every event is a race the clear cannot win: a drag
		// emits size messages far faster than a command round-trips, and the
		// renderer keeps flushing with the stale count in between. So the
		// clear waits for the drag to stop, and lands once.
		//
		// Height alone reflows nothing, and the first size message is the
		// window opening: clearing there would wipe the scrollback that being
		// inline exists to preserve.
		reflowed := m.ready && m.width != msg.Width
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		m.layout()
		if !reflowed {
			return m, nil
		}
		m.resize++
		settled := m.resize
		return m, tea.Tick(resizeSettles, func(time.Time) tea.Msg {
			return resizeSettledMsg{generation: settled}
		})

	case resizeSettledMsg:
		// Only the last drag's timer clears; the ones behind it are stale.
		if msg.generation != m.resize {
			return m, nil
		}
		return m, tea.ClearScreen

	case statusMsg:
		// A message is an answer to the last key, so it goes when the next
		// one is pressed — and if none is, it still goes: a line that stays
		// green all afternoon stops meaning "just happened".
		m.status, m.statusEr = msg.text, msg.err
		m.statusGen++
		expires := m.statusGen
		return m, tea.Tick(statusHolds, func(time.Time) tea.Msg {
			return statusExpiredMsg{generation: expires}
		})

	case statusExpiredMsg:
		if msg.generation == m.statusGen {
			m.status, m.statusEr = "", false
		}
		return m, nil

	case runActionMsg:
		model, cmd := m.actOn(msg.key, msg.targets)
		return model, cmd

	case showCommandMsg:
		m.dialog = messageDialog(msg.title, msg.body, msg.command)
		return m, nil

	case editorDoneMsg:
		return m.promptEdited(msg), nil

	case tea.KeyMsg:
		model, cmd := m.updateKey(msg)
		// The composer's height is a function of its contents, so geometry is
		// recomputed after every key rather than only on a resize.
		if next, ok := model.(Model); ok {
			next.layout()
			return next, cmd
		}
		return model, cmd
	}

	var cmd tea.Cmd
	m.prompt, cmd = m.prompt.Update(msg)
	return m, cmd
}

type statusMsg struct {
	text string
	err  bool
}

// resizeSettles is how long the window waits for a drag to stop before it
// erases and redraws. Long enough that a drag lands one clear rather than
// dozens, short enough not to leave the mess sitting there.
const resizeSettles = 120 * time.Millisecond

type resizeSettledMsg struct{ generation int }

// statusHolds is how long a message sits there when nothing else is pressed.
const statusHolds = 4 * time.Second

type statusExpiredMsg struct{ generation int }

func status(format string, args ...any) tea.Cmd {
	return func() tea.Msg { return statusMsg{text: fmt.Sprintf(format, args...)} }
}

func (m Model) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Whatever was reported was about the previous key. Clearing here rather
	// than in each handler means a message cannot outlive its moment, and the
	// handlers that report something simply report it again.
	m.status, m.statusEr = "", false
	m.statusGen++

	if msg.String() == "ctrl+c" {
		m.quit = true
		return m, tea.Quit
	}

	if m.dialog != nil {
		cmd, closed := m.dialog.update(msg)
		if closed {
			m.dialog = nil
		}
		return m, cmd
	}
	if m.optionsOpen {
		return m.updateOptions(msg)
	}
	if msg.String() == "f1" {
		m.dialog = textDialog("Keys", helpText())
		return m, nil
	}

	if m.focus == focusList {
		return m.updateList(msg)
	}
	return m.updatePrompt(msg)
}

// promptEdited takes back what the editor saved. The buffer is replaced
// wholesale, including with nothing: an editor left empty is how you throw a
// prompt away.
func (m Model) promptEdited(msg editorDoneMsg) Model {
	defer os.Remove(msg.path)

	if msg.err != nil {
		m.status, m.statusEr = fmt.Sprintf("editor exited: %v", msg.err), true
		return m
	}
	edited, err := os.ReadFile(msg.path)
	if err != nil {
		m.status, m.statusEr = fmt.Sprintf("cannot read the prompt back: %v", err), true
		return m
	}
	m.prompt.SetValue(strings.TrimRight(string(edited), "\n"))
	m.promptEnd()
	m.layout()
	m.status, m.statusEr = "", false
	return m
}

// updatePrompt handles the default mode. Every key is text, except the ones
// that leave: Up walks out of the top of the field, Enter launches, and Tab
// opens the options.
func (m Model) updatePrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up":
		// Up is a line motion first, and only walks out of the field from the
		// row it cannot move off. On that row it goes to the start of the
		// line, and only from there — already at the start — does it leave
		// for the list. Editing never costs you your place by accident, and a
		// multi-line prompt still moves a line at a time.
		if !m.onFirstPromptRow() {
			break
		}
		if m.prompt.LineInfo().ColumnOffset > 0 {
			m.prompt.CursorStart()
			return m, nil
		}
		if len(m.list.rows()) == 0 {
			return m, status("no sandboxes to pick from")
		}
		m.focus = focusList
		m.prompt.Blur()
		return m, nil

	case "down":
		// The mirror, off the last row: to the end of the line, then into the
		// list — at the top of it, because arriving from below at the row
		// nearest the prompt would put the cursor somewhere the eye was not.
		if !m.onLastPromptRow() {
			break
		}
		if !m.atPromptEnd() {
			m.prompt.CursorEnd()
			return m, nil
		}
		if len(m.list.rows()) == 0 {
			return m, status("no sandboxes to pick from")
		}
		m.focus = focusList
		m.list.moveTo(0)
		m.prompt.Blur()
		return m, nil

	case "tab":
		// Tab is the one key that moves between the two halves of the window,
		// in both directions. The options are a layer over it, on Shift-Tab.
		if len(m.list.rows()) == 0 {
			return m, status("no sandboxes to pick from")
		}
		m.focus = focusList
		m.prompt.Blur()
		return m, nil

	case "shift+tab", "ctrl+o":
		m.optionsOpen = true
		return m, nil

	case "alt+e", "f2":
		// Alt-E, and F2 for the terminals where Option is not Meta.
		return m, editPrompt(m.prompt.Value())

	case "enter":
		cmd := m.run()
		return m, cmd

	case "alt+enter", "ctrl+enter", "ctrl+j":
		// A terminal that can tell Ctrl-Enter apart sends ctrl+enter; most
		// send it as ctrl+j, which is the same byte as Ctrl-J.
		m.prompt.InsertString("\n")
		return m, nil

	case "ctrl+d":
		// EOF on an empty line quits, the way a shell does. With text in the
		// buffer it is the shell's other meaning, delete forward, which the
		// textarea already implements — so it is passed through.
		if strings.TrimSpace(m.prompt.Value()) == "" {
			m.quit = true
			return m, tea.Quit
		}

	case "esc":
		return m, nil
	}

	var cmd tea.Cmd
	m.prompt, cmd = m.prompt.Update(msg)
	return m, cmd
}

// onFirstPromptRow and onLastPromptRow ask about displayed rows, not lines: a
// line long enough to wrap occupies several, and Up has to walk through them
// the way it walks through separate lines.
func (m *Model) onFirstPromptRow() bool {
	return m.prompt.Line() == 0 && m.prompt.LineInfo().RowOffset == 0
}

func (m *Model) onLastPromptRow() bool {
	info := m.prompt.LineInfo()
	return m.prompt.Line() == m.prompt.LineCount()-1 && info.RowOffset == info.Height-1
}

func (m *Model) atPromptStart() bool {
	return m.onFirstPromptRow() && m.prompt.LineInfo().ColumnOffset == 0
}

func (m *Model) promptHome() {
	for m.prompt.Line() > 0 {
		m.prompt.CursorUp()
	}
	m.prompt.CursorStart()
}

// atPromptEnd reports whether the cursor is past the last character of the
// buffer. The textarea's own CharWidth counts a trailing cell the cursor can
// sit on, so the buffer's last line is measured instead.
func (m *Model) atPromptEnd() bool {
	lines := strings.Split(m.prompt.Value(), "\n")
	last := len(lines) - 1
	return m.prompt.Line() >= last &&
		m.prompt.LineInfo().ColumnOffset >= lipgloss.Width(lines[last])
}

func (m *Model) promptEnd() {
	for m.prompt.Line() < m.prompt.LineCount()-1 {
		m.prompt.CursorDown()
	}
	m.prompt.CursorEnd()
}

// updateList handles the sandbox pane. A letter is a command here, the way it
// is in difftui's file lists, because there is no text to type.
func (m Model) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "down", "j":
		// Visual mode holds you in the list: falling out of the bottom of a
		// range you are still drawing would throw the range away.
		if m.list.cursor >= len(m.list.rows())-1 && !m.list.visual {
			return m.backToPrompt(), nil
		}
		m.list.move(1)
	case "up", "k":
		// Both ends of the list lead back to the prompt: it is the place you
		// always end up, so neither edge is a dead stop.
		if m.list.cursor == 0 && !m.list.visual {
			return m.backToPrompt(), nil
		}
		m.list.move(-1)
	case "left", "h":
		// A long name is ellipsized; the row under the cursor can be walked
		// sideways to read the rest of it.
		if !m.list.scrollName(-4) {
			return m, nil
		}
	case "right", "l":
		if !m.list.scrollName(4) {
			return m, status("nothing more to the right")
		}
	case "pgdown":
		m.list.move(m.list.height)
	case "pgup":
		m.list.move(-m.list.height)
	case "home", "g":
		m.list.moveTo(0)
	case "end", "G":
		m.list.moveTo(len(m.list.rows()) - 1)

	case "esc", "tab":
		if m.list.visual {
			m.list.visual = false
			return m, status("visual select cancelled")
		}
		return m.backToPrompt(), nil
	case "shift+tab", "ctrl+o":
		m.optionsOpen = true
	case "V":
		m.list.toggleVisual()
		if !m.list.visual {
			return m, status("visual select cancelled")
		}
		return m, nil
	case " ":
		if m.list.visual {
			return m, status("selected %s", plural(m.list.commitVisual(), "sandbox", "sandboxes"))
		}
		m.list.toggleSelect()
		m.list.move(1)
	case "c":
		m.list.clearSelection()
		m.list.visual = false
		return m, status("selection cleared")
	case "f":
		m.list.onlyHere = !m.list.onlyHere
		m.list.clamp()
		if m.list.onlyHere {
			return m, status("showing sandboxes started from %s", currentDir)
		}
		return m, status("showing every sandbox in the project")
	case "A":
		m.list.showArchived = !m.list.showArchived
		m.list.clamp()
		m.layout()
		if m.list.showArchived {
			return m, status("showing archived sandboxes")
		}
		return m, status("hiding archived sandboxes")
	case "r":
		return m, status("refreshed (mock — the data never changes)")
	case "?":
		m.dialog = textDialog("Keys", helpText())
	case "enter":
		return m.act("a")
	case ".":
		// The menu cannot run the action itself: a dialog closes over the
		// model by value. It emits the choice as a message instead, and the
		// update loop runs it against the live model.
		targets := m.list.targets()
		m.dialog = actionsDialog(actionTitle(targets), "", m.actions(targets), chooseAction(targets))
	default:
		for _, a := range m.actions(m.list.targets()) {
			if a.key == msg.String() {
				return m.act(a.key)
			}
		}
	}
	return m, nil
}

func (m Model) backToPrompt() Model {
	m.focus = focusPrompt
	m.prompt.Focus()
	return m
}

// updateOptions handles the run options panel.
func (m Model) updateOptions(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	opt := m.opts.current()
	switch msg.String() {
	case "esc", "tab", "shift+tab", "ctrl+o":
		m.optionsOpen = false
		return m, nil
	case "up", "k":
		m.opts.move(-1)
	case "down", "j":
		m.opts.move(1)
	case "left", "h":
		opt.cycle(-1)
	case "right", "l", " ":
		opt.cycle(1)
	case "enter":
		switch opt.kind {
		case optText:
			m.dialog = inputDialog(opt.label, opt.hint, "", opt.value, func(v string) tea.Cmd {
				opt.value = v
				return nil
			})
		case optMulti:
			m.dialog = inputDialog("Add "+opt.label, opt.hint, "KEY=VALUE", "", func(v string) tea.Cmd {
				if v != "" {
					opt.items = append(opt.items, v)
				}
				return nil
			})
		default:
			opt.cycle(1)
		}
	case "backspace", "-":
		if opt.kind == optMulti && len(opt.items) > 0 {
			opt.items = opt.items[:len(opt.items)-1]
		}
	case "ctrl+r":
		m.optionsOpen = false
		return m, m.run()
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// actions

// actions is the base level CLI, filtered against what the current targets can
// actually do. Unavailable actions stay on the menu with the reason, rather
// than disappearing and leaving you wondering where upgrade went.
func (m Model) actions(targets []sandbox) []action {
	one := len(targets) == 1
	anyDiff, anyUpgrade, anyRunning, anyStopped := false, false, false, false
	anyArchived, allArchived := false, len(targets) > 0
	for _, s := range targets {
		anyDiff = anyDiff || s.hasDiff()
		anyUpgrade = anyUpgrade || s.upgrade
		anyRunning = anyRunning || s.state == stateRunning
		anyStopped = anyStopped || s.state == stateStopped
		anyArchived = anyArchived || s.state == stateArchived
		allArchived = allArchived && s.state == stateArchived
	}
	attachable := one && targets[0].attachable()
	return []action{
		{key: "a", label: "attach", detail: "join the harness terminal", enabled: attachable,
			why: attachWhy(one, targets)},
		{key: "s", label: "shell", detail: "open a shell in the sandbox", enabled: attachable,
			why: attachWhy(one, targets)},
		{key: "d", label: "diff", detail: "show what changed in the sandbox", enabled: anyDiff,
			why: "nothing has changed yet"},
		{key: "y", label: "apply", detail: "bring the changes back to " + currentDir, enabled: anyDiff,
			why: "nothing to apply"},
		{key: "i", label: "status", detail: "changed files, one per line", enabled: len(targets) > 0},
		{key: "u", label: "upgrade", detail: "re-pin to the current harness image", enabled: anyUpgrade,
			why: "already on the current image"},
		{key: "t", label: "stop", detail: "power the sandbox off, keeping its disk", enabled: anyRunning,
			why: "not running"},
		{key: "T", label: "start", detail: "power a stopped sandbox back on", enabled: anyStopped,
			why: "already on, or archived"},
		{key: "x", label: "archive", detail: "put it away, disk and all, reversibly", enabled: !allArchived,
			why: "already archived"},
		{key: "U", label: "unarchive", detail: "bring an archived sandbox back", enabled: anyArchived,
			why: "not archived"},
		{key: "P", label: "purge", detail: "destroy it and its data, for good", enabled: allArchived,
			why: "archive it first — purge only takes archived sandboxes"},
	}
}

func attachWhy(one bool, targets []sandbox) string {
	if !one {
		return "takes exactly one sandbox"
	}
	if len(targets) == 1 && !targets[0].attachable() {
		return "the sandbox is " + string(targets[0].state)
	}
	return ""
}

func actionTitle(targets []sandbox) string {
	if len(targets) == 1 {
		return targets[0].name
	}
	return plural(len(targets), "sandbox", "sandboxes") + " selected"
}

// chooseAction is the action menu's callback: it turns the chosen key into a
// message, which the update loop runs against the live model.
func chooseAction(targets []sandbox) func(string) tea.Cmd {
	return func(key string) tea.Cmd {
		return func() tea.Msg { return runActionMsg{key: key, targets: targets} }
	}
}

type runActionMsg struct {
	key     string
	targets []sandbox
}

func (m Model) act(key string) (tea.Model, tea.Cmd) {
	// A command acts on the visual range and ends the mode, the way staging a
	// visual selection does in difftui.
	targets := m.list.targets()
	m.list.visual = false
	model, cmd := m.actOn(key, targets)
	return model, cmd
}

// actOn is where a real launcher would hand off to the CLI. The mockup shows
// the command it would run instead, which doubles as documentation of what
// each key does.
func (m Model) actOn(key string, targets []sandbox) (Model, tea.Cmd) {
	if len(targets) == 0 {
		return m, status("nothing selected")
	}
	var chosen *action
	for _, a := range m.actions(targets) {
		if a.key == key {
			chosen = &a
			break
		}
	}
	if chosen == nil {
		return m, nil
	}
	if !chosen.enabled {
		m.dialog = errorDialog("Cannot "+chosen.label, chosen.why)
		return m, nil
	}

	ids := make([]string, 0, len(targets))
	for _, s := range targets {
		ids = append(ids, s.id)
	}
	idList := strings.Join(ids, " ")

	var command string
	switch key {
	case "a":
		command = "disco attach " + idList
	case "s":
		command = "disco shell " + idList
	case "d":
		command = "disco diff " + idList
	case "y":
		command = "disco apply " + idList
	case "i":
		command = "disco status " + idList
	case "u":
		command = "disco sandbox upgrade " + idList
	case "t":
		command = "disco sandbox stop " + idList
	case "T":
		command = "disco sandbox start " + idList
	case "x":
		command = "disco delete " + idList
	case "U":
		command = "disco sandbox unarchive " + idList
	case "P":
		command = "disco sandbox purge " + idList
	}

	body := actionTitle(targets)
	if key == "P" {
		// Archiving is reversible and needs no ceremony. Purging is not.
		question := fmt.Sprintf("Purge %s? The disk and everything on it goes, and unarchive cannot bring it back.",
			actionTitle(targets))
		cmd := command
		m.dialog = confirmDialog("Purge", question, func(string) tea.Cmd {
			return func() tea.Msg { return showCommandMsg{title: "Purge", body: body, command: cmd} }
		})
		return m, nil
	}
	m.dialog = messageDialog(strings.ToUpper(chosen.label[:1])+chosen.label[1:], body, command)
	return m, nil
}

type showCommandMsg struct {
	title, body, command string
}

// run is Enter in the prompt: the whole point of the window.
//
// An empty prompt is not an error. It means the other thing you come here for:
// a sandbox of your own to work in, with no harness given anything to do.
func (m *Model) run() tea.Cmd {
	prompt := strings.TrimSpace(m.prompt.Value())
	body := prompt
	if prompt == "" {
		body = "an empty sandbox from " + currentDir + " @ " + currentBranch
	}
	m.dialog = messageDialog("Run", body, m.opts.command(prompt))
	return nil
}

// ---------------------------------------------------------------------------
// layout and view

func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	// The composer grows with what is typed, one line at a time, the way
	// Claude Code's does — and the list gives up a row for each one it takes.
	promptH := min(max(m.prompt.LineCount(), 1), 8)
	// header, list title, the blanks above and below the rows, the composer's
	// label, its two rules, the mode line and the status line.
	room := max(m.height-promptH-9, 1)

	// The mark sits to the left of the list, and gives the columns back on a
	// terminal narrow enough that the list needs them more.
	listW := m.width
	if m.showLogo() {
		listW = m.width - m.logo.width - logoGutter
	}

	// The window is inline, so it takes the rows it needs and no more: the
	// list is as tall as its contents until it runs out of terminal, and only
	// then does it scroll. It never gives up so many rows that the mark beside
	// it is left hanging in space.
	rows := min(len(m.list.rows()), room)
	if m.showLogo() {
		// The title and the two blanks stand beside the mark too.
		rows = min(max(rows, m.logo.height()-3), room)
	}
	m.list.width, m.list.height = listW, rows
	m.list.clamp()
	m.prompt.SetWidth(max(m.width-2, 10))
	m.prompt.SetHeight(promptH)
}

func (m Model) View() string {
	if m.quit {
		return ""
	}
	if !m.ready {
		return "Loading…"
	}

	// Inline, so a modal cannot be centred over anything: it is drawn in
	// place of the window, and closing it puts the window back.
	if m.dialog != nil {
		return m.dialog.view(m.st, m.width)
	}
	if m.optionsOpen {
		return m.opts.view(m.st, m.width, m.prompt.Value())
	}

	// The header spans the window; under it the mark stands beside the list,
	// and the composer spans both again.
	body := m.list.view(m.st, m.focus == focusList)
	if m.showLogo() {
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.logo.view(), body)
	}

	return lipgloss.JoinVertical(lipgloss.Left,
		m.viewHeader(m.width),
		body,
		m.viewPrompt(),
	)
}

// showLogo reports whether there is width to spare for the mark. Below the
// threshold the list takes the whole row: decoration is the first thing a
// narrow terminal should lose.
func (m Model) showLogo() bool {
	return m.logo.height() > 0 && m.width >= minWidthForLogo
}

func (m Model) viewHeader(width int) string {
	left := m.st.headerLabel.Render("disco  ")
	// The project is named only when it is not the one you are almost always
	// in — a header that says "default" every time teaches you to skip it.
	if m.project != defaultProject {
		left += m.st.headerBar.Render(m.project) + m.st.headerLabel.Render("  ")
	}
	left += m.st.headerLabel.Render(currentDir + " @ " + currentBranch)
	right := m.st.dimText.Render("F1 help  ·  Ctrl-C quit")
	return spread(left, right, width)
}

// viewPrompt draws the composer the way Claude Code draws its own: a rule
// above and below the text, a chevron in front of it, and one dim line under
// the rule for the mode you are in. The rule brightens when the prompt has
// focus, which is the only thing that has to be visible from across the room.
func (m Model) viewPrompt() string {
	ruleStyle := m.st.ruleOn
	if m.focus != focusPrompt {
		ruleStyle = m.st.rule
	}
	rule := ruleStyle.Render(strings.Repeat("─", max(m.width, 0)))
	mode := padANSI("  "+m.opts.chips(m.st), m.width)
	// What the field does sits against the field; the keys are keys, and
	// belong on the status line with everything else transient.
	return lipgloss.JoinVertical(lipgloss.Left,
		m.viewLabel(), rule, m.prompt.View(), rule, mode, m.viewStatus())
}

// viewLabel is the line above the composer, and says what pressing Enter in it
// does. It does not change with focus: it is a label on the field, not a hint
// about the moment.
func (m Model) viewLabel() string {
	label := "Enter runs the prompt in a new sandbox, or just creates one when it is empty"
	if m.focus != focusPrompt {
		return padANSI("  "+m.st.dimText.Render(label), m.width)
	}
	return padANSI("  "+m.st.chip.Render(label), m.width)
}

// viewStatus is the bottom line: the keys, or what just happened. A message
// displaces the keys until the next one is pressed.
func (m Model) viewStatus() string {
	left := "  " + m.st.dimText.Render(m.hints())
	switch {
	case m.statusEr:
		left = "  " + m.st.statusER.Render("✗ "+m.status)
	case m.status != "":
		left = "  " + m.st.statusOK.Render(m.status)
	}
	right := ""
	if n := m.list.selectionCount(); n > 0 {
		right = m.st.statusWA.Render(plural(n, "selected", "selected")) + "  "
	}
	return spread(left, right, m.width)
}

func (m Model) hints() string {
	switch m.focus {
	case focusList:
		if m.list.visual {
			lo, hi := m.list.visualRange()
			return fmt.Sprintf("VISUAL  %s · ↑/↓ extend · Space selects · a letter acts on the range · V or Esc cancel",
				plural(hi-lo+1, "sandbox", "sandboxes"))
		}
		// Only the actions the sandboxes under the cursor can actually take:
		// a key list that offers purge on a running sandbox is a key list you
		// stop reading.
		var parts []string
		for _, a := range m.actions(m.list.targets()) {
			if a.enabled {
				parts = append(parts, a.key+" "+a.label)
			}
		}
		parts = append(parts, "Space select", "V range")
		if m.list.showArchived {
			parts = append(parts, "A hide archived")
		} else if m.list.archivedCount() > 0 {
			parts = append(parts, "A show archived")
		}
		parts = append(parts, "Tab prompt")
		keys := strings.Join(parts, " · ")
		if m.list.nameFull > m.list.nameWidth {
			// Only worth saying on a row that has more name than column.
			keys = "←→ read the rest of the name · " + keys
		}
		return keys
	default:
		return "Tab or ↑↓ select sandbox · Shift-Tab options · Alt-E editor · Ctrl-Enter newline · Ctrl-D quit"
	}
}

func helpText() string {
	return strings.Join([]string{
		"The window opens in the prompt, because starting something new is",
		"what you are usually here for. Everything else is one key away.",
		"",
		"───────────────────────────────────────────────────────────────",
		"In the prompt",
		"",
		"    Enter          run the prompt in a new sandbox, or with an",
		"                   empty prompt just create one and attach to it",
		"    Ctrl-Enter     newline (Alt-Enter too, and Ctrl-J, which is",
		"                   what most terminals send for Ctrl-Enter)",
		"    Ctrl-D         quit, when the prompt is empty",
		"    ↑ ↓            move a line at a time, wrapped rows included.",
		"                   From the row they cannot move off they go to the",
		"                   start or the end of it, and from there into the",
		"                   sandbox list: ↑ where you last were, ↓ at the top",
		"    Alt-E or F2    write the prompt in $EDITOR",
		"    Tab            the sandbox list, and Tab again to come back",
		"    Shift-Tab      run options",
		"",
		"───────────────────────────────────────────────────────────────",
		"In the sandbox list",
		"",
		"    ↑ ↓ / k j      move            Space   select, for acting on",
		"    g / G          first / last            several at once",
		"    ← →            read the rest of a name too long for its column",
		"    c              clear the selection",
		"    V              draw a range: ↑ ↓ extend it, Space selects the",
		"                   whole of it, a command acts on it, V or Esc",
		"                   cancels",
		"",
		"  Three backgrounds, and no column spent on any of them: the row",
		"  under the cursor, which also wears the chevron; the rows a",
		"  command would act on; and, in its own colour, a row that is",
		"  both. A range being drawn counts as selected.",
		"    ↓ past the end returns to the prompt, and so do Tab and Esc.",
		"",
		"  A row reads: state · name · harness · the folder and commit it",
		"  was spawned from · when it was last used · what it is using of",
		"  its cpu, memory and disk · what it has changed.",
		"",
		"      ● running    ◐ starting    ○ stopped    ▪ archived",
		"      ✗ error — the row shows the error under the cursor",
		"",
		"  Half of what those carry is their colour, so without it the",
		"  glyph gives way to the state spelled out in a column.",
		"      ↑ an upgrade is available, to the current harness image",
		"",
		"  A starred commit — main@a3f9c21* — means the sandbox was cut",
		"  from a snapshot of uncommitted work on top of it, and a folder",
		"  in blue is one other than this one.",
		"",
		"    Enter  attach          s  shell",
		"    d      diff            y  apply back to this directory",
		"    i      status          u  upgrade to the current image",
		"    t      stop            T  start",
		"    x      archive         U  unarchive",
		"    P      purge           .  every action, as a menu",
		"",
		"  The keys along the bottom are only the ones the sandboxes under",
		"  the cursor can take: upgrade appears when one is available,",
		"  unarchive when something is archived, purge only for archived",
		"  sandboxes. Archiving is reversible and asks nothing; purge",
		"  destroys the disk and asks first.",
		"",
		"    A      show or hide archived sandboxes",
		"    f      only sandboxes started from this directory",
		"    r      refresh",
		"",
		"───────────────────────────────────────────────────────────────",
		"Run options (Shift-Tab)",
		"",
		"    ↑ ↓            move            ← →   change the value",
		"    Enter          edit a text option, or add an env / secret",
		"    Backspace      drop the last env / secret",
		"    Ctrl-R         run with these options",
		"    Esc            back to the prompt",
		"",
		"  The strip above the prompt always shows what is set, so the",
		"  panel never has to be open to know what Enter will do.",
		"",
		"───────────────────────────────────────────────────────────────",
		"This is a mockup. Nothing is created, attached to or deleted:",
		"every action shows the command it would have run.",
		"",
		"  Press Esc to close.",
	}, "\n")
}
