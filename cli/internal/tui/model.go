// Package tui is the `disco tui` launcher: one window that opens with the
// cursor already in a prompt for a new sandbox, and the sandboxes you already
// have one press of Tab away.
//
// It runs on the alternate screen: it takes the whole terminal while it is up,
// and leaves what was on screen before exactly as it was when it exits. Actions
// that own the terminal — the command behind apply — suspend the window, which
// drops back to the primary screen and hands them the real streams; attach and
// a shell are drawn in the window itself.
package tui

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/obot-platform/discobox/termpane"
	"github.com/obot-platform/discobox/termpane/selection"
)

type focusArea int

const (
	// The prompt is where the cursor starts. Nothing has to be pressed to
	// begin describing a new sandbox: typing is the default mode.
	focusPrompt focusArea = iota
	focusList
	// The folder filter, in the header. It sits above the list on screen, so
	// Up off the top of the list is what reaches it.
	focusFolder
	// A sandbox's terminal, drawn in place of everything else. While it has
	// focus every key belongs to the sandbox except the detach prefix.
	focusPane
)

// Model is the launcher window.
type Model struct {
	ctx context.Context
	ds  DataSource
	st  *styles

	width, height int
	ready         bool

	list   *sandboxList
	prompt textarea.Model
	opts   *optionSet
	logo   logo

	// harnesses is the project's harnesses: the screen that manages them, and
	// the listing the run options' harness choices are built from. It is read
	// whether or not the screen is up. See harnesses.go.
	harnesses *harnessList

	// The workspace screen: the discobox's terminals on the left, the primary
	// among them, and its shells as tabs on the right, both following the
	// server. See workspace.go.
	terminals column
	shells    column
	// onShells is which of the two columns has the keys.
	onShells bool
	// maximized is whether one column has the whole window instead of the two
	// sharing it. Which column that is follows the focus — onShells — so there
	// is one visible box and it is always the one the keys go to. See
	// toggleMaximized.
	maximized bool
	// connecting is the exec ids with an attach in flight, so a poll that
	// still lists them does not open a second pane onto the same session.
	connecting map[string]bool
	// wsGen numbers workspaces. Detaching bumps it, and a poll tick or an
	// open still in flight from the one that was left is stale and dropped.
	wsGen int
	// forward holds the workspace's port forward while it is open: the local
	// ports standing in for what the discobox is serving, which the header
	// draws as arrows. See workspace.go.
	forward Forward
	// overlay is the command that has the screen over the workspace while it
	// runs.
	overlay *pane
	// paneBox is the discobox the workspace is showing. Every pane on it is
	// that one, and so is everything the leader's keys act on.
	paneBox    Sandbox
	nextPaneID int

	// mouseSeized is whether the leader plus m has taken the mouse from the
	// box: while set, nothing is forwarded and every event drives the panes'
	// own selection. Selection needs no seizing while nothing in the box has
	// asked for the mouse — it simply works — so this is for vim and htop and
	// their kind, when you would rather copy a stack trace than click on it.
	mouseSeized bool

	// mouseCapture is the pane a left-button gesture started in, which owns
	// every mouse event until the button is released: a drag that crosses a
	// border must not change hands mid-gesture. Zero when no gesture is open.
	mouseCapture int

	// The chrome's own selection — header, hints, borders — behind the
	// panes' rich one; see chrome.go. lastFrame is the frame as last
	// composed, which is what a chrome press selects from, and chromeShot is
	// what the selection read when it was made.
	chromeGrid    *frameGrid
	chromeSel     *selection.Model
	chromeShot    string
	chromeCapture bool
	lastFrame     string

	// tabSpans is where each visible tab label sits in the shell box's top
	// border, recorded as the strip is drawn (tabbedEdge) so a click on
	// [2 bash] can mean tab 2. Box-relative columns.
	tabSpans []tabSpan

	// zoomSpans is where each box's maximize control sits, recorded as the
	// boxes are drawn (zoomControl) so a click on [+] can mean that box.
	// Absolute screen columns.
	zoomSpans []zoomSpan

	// leaderKey is the pane's prefix; empty takes the default. See Model.leader.
	leaderKey string

	// expanded is whether the window has opened out from the prompt it starts
	// as into the full launcher. See compact.go.
	expanded bool

	// shimmer is the frame the opening glint is on, or zero when there is none.
	// See shimmer.go.
	shimmer int
	noise   *rand.Rand

	focus       focusArea
	optionsOpen bool
	// harnessesOpen is whether the harnesses screen has the window. Like the
	// options panel it stands in place of the launcher rather than inside it, and
	// every key belongs to it while it is up.
	harnessesOpen bool
	dialog        *dialog

	session Session
	status  string
	statusE bool
	quit    bool

	// busy is what the window is waiting on — creating a sandbox, running a
	// verb — shown in place of the key hints so a slow server does not read as
	// a dead window.
	busy string

	// exec hands a terminal-owning action to the runtime, which releases the
	// terminal around it. It is a field only so a test can run one without a
	// terminal to release; nothing in the window ever replaces it.
	exec func(tea.ExecCommand, tea.ExecCallback) tea.Cmd

	// copyOS writes the OS clipboard. A field for the same reason exec is:
	// a test copy must not clobber the developer's actual clipboard.
	copyOS func(string) error

	// statusGen counts messages, so a timer can tell whether it is the last one
	// out.
	statusGen int
}

// Option configures the window at construction.
type Option func(*Model)

// WithLeader sets the pane's prefix key, as the keys package normalizes it.
// Empty takes the default.
func WithLeader(key string) Option {
	return func(m *Model) { m.leaderKey = key }
}

// WithHarnesses opens the window on the harnesses screen, which is what
// `disco configure` is: the same window, opened on the screen that command is
// about. The window is opened out with it — the screen is the whole of it, and
// the opening prompt has no room for one.
func WithHarnesses() Option {
	return func(m *Model) { m.harnessesOpen, m.expanded = true, true }
}

func New(ctx context.Context, ds DataSource, options ...Option) *Model {
	color := detectColor()
	st := newStyles(color)

	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	// Claude Code's composer: one chevron on the first line, continuation
	// lines aligned under the text rather than under the chevron.
	ta.SetPromptFunc(2, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return "❯ "
		}
		return "  "
	})
	// A colorless terminal gets the zero styles, which are the identity: what
	// it is shown is the text. Anywhere else the composer carries the window's
	// own accent on its chevron, and no cursor-line background — the composer is
	// one field, not a list with a row picked out.
	composer := textarea.Styles{}
	if st.color {
		composer = textarea.DefaultDarkStyles()
	}
	composer.Focused.Prompt = st.chipOn
	composer.Blurred.Prompt = st.dimText
	composer.Focused.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(composer)
	ta.Focus()

	session := Session{DefaultProject: "default"}
	m := &Model{
		ctx:        ctx,
		ds:         ds,
		st:         st,
		list:       newSandboxList(session),
		harnesses:  newHarnessList(),
		prompt:     ta,
		logo:       newLogo(color),
		focus:      focusPrompt,
		session:    session,
		exec:       tea.Exec,
		copyOS:     func(text string) error { return osClipboard(ctx, text) },
		chromeGrid: &frameGrid{},
		noise:      newNoise(),
	}
	m.chromeSel = selection.New(m.chromeGrid)
	// The label above the field already says what an empty prompt does, and the
	// placeholder is gone the moment you type.
	m.prompt.Placeholder = m.placeholder()
	for _, option := range options {
		option(m)
	}
	m.opts = newOptions(session)
	return m
}

// Init loads what the window is drawn from. The session comes first because the
// header and the filter are read from it; the listing and the harnesses follow
// on their own, so a slow server delays the rows rather than the window. The
// harnesses are read here rather than when their screen is opened because the
// run options offer them as the harness to run.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.loadSession(), m.refresh(), m.loadHarnesses(), m.tick(), m.startShimmer())
}

// ---------------------------------------------------------------------------
// commands

// refreshEvery is how often the listing is re-read. A sandbox's state changes
// without anything here asking it to — it starts, it stops, an upgrade appears
// — so the window follows the server rather than waiting to be told.
const refreshEvery = 5 * time.Second

func (m *Model) tick() tea.Cmd {
	return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *Model) loadSession() tea.Cmd {
	return func() tea.Msg {
		session, err := m.ds.Session(m.ctx)
		return sessionLoadedMsg{session: session, err: err}
	}
}

func (m *Model) refresh() tea.Cmd {
	return func() tea.Msg {
		sandboxes, err := m.ds.List(m.ctx)
		return listLoadedMsg{sandboxes: sandboxes, err: err}
	}
}

// ---------------------------------------------------------------------------
// messages

type sessionLoadedMsg struct {
	session Session
	err     error
}

type listLoadedMsg struct {
	sandboxes []Sandbox
	err       error
}

type tickMsg struct{}

type statusMsg struct {
	text string
	err  bool
}

// dirtyCheckedMsg answers --include-dirty=auto: whether there is uncommitted
// work in the source, and so whether there is anything to ask about.
type dirtyCheckedMsg struct {
	req   RunRequest
	dirty bool
	err   error
}

// createdMsg reports the sandbox Enter asked for, or why there is none.
type createdMsg struct {
	sandbox Sandbox
	req     RunRequest
	err     error
}

// verbDoneMsg reports a lifecycle verb that ran against the API and returned.
type verbDoneMsg struct {
	verb Verb
	ids  []string
	errs []error
}

// interactDoneMsg reports that a terminal-owning action returned and the window
// has redrawn.
type interactDoneMsg struct {
	action Interaction
	err    error
}

// runActionMsg carries a choice out of the action menu. The menu cannot run the
// action itself: a dialog closes over the model by value, so it emits the
// choice and the update loop runs it against the live model.
type runActionMsg struct {
	key     string
	targets []Sandbox
}

// statusHolds is how long a message sits there when nothing else is pressed.
const statusHolds = 4 * time.Second

type statusExpiredMsg struct{ generation int }

func status(format string, args ...any) tea.Cmd {
	return func() tea.Msg { return statusMsg{text: fmt.Sprintf(format, args...)} }
}

// ---------------------------------------------------------------------------
// update

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		// Columns mean nothing across a resize; see the panes' own clearing.
		m.chromeSel.Clear()
		m.layout()
		return m, nil

	case sessionLoadedMsg:
		if msg.err != nil {
			return m, m.report(true, "cannot read the session: %v", msg.err)
		}
		m.session = msg.session
		m.list.session = msg.session
		// The window opens on the folder it was opened in, which is what
		// `disco ls` shows and what the header has always said. Everything
		// else is one press away in the dropdown.
		m.list.folder = msg.session.Directory
		m.opts = newOptions(msg.session)
		m.opts.setFolder(m.list.folder)
		// The two loads race, and either order has to end with the panel
		// offering the harnesses that are actually there.
		m.opts.setHarnesses(m.harnesses.all)
		return m, nil

	case listLoadedMsg:
		if msg.err != nil {
			return m, m.report(true, "cannot list discoboxes: %v", msg.err)
		}
		m.list.setAll(msg.sandboxes)
		m.layout()
		return m, nil

	case tickMsg:
		cmds := []tea.Cmd{m.refresh(), m.tick()}
		// Harnesses change when somebody changes them, which is here — so they are
		// re-read after every action rather than on a clock. The exception is
		// the screen itself, where a listing going stale under the cursor is
		// exactly what a stale listing costs.
		if m.harnessesOpen {
			cmds = append(cmds, m.loadHarnesses())
		}
		return m, tea.Batch(cmds...)

	case statusMsg:
		// A message is an answer to the last key, so it goes when the next
		// one is pressed — and if none is, it still goes: a line that stays
		// green all afternoon stops meaning "just happened".
		m.status, m.statusE = msg.text, msg.err
		m.statusGen++
		expires := m.statusGen
		return m, tea.Tick(statusHolds, func(time.Time) tea.Msg {
			return statusExpiredMsg{generation: expires}
		})

	case statusExpiredMsg:
		if msg.generation == m.statusGen {
			m.status, m.statusE = "", false
		}
		return m, nil

	case dirtyCheckedMsg:
		return m, m.dirtyChecked(msg)

	case createMsg:
		return m, m.create(msg.req)

	case createdMsg:
		return m, m.created(msg)

	case runVerbMsg:
		return m, m.runVerb(msg.verb, msg.ids)

	case verbDoneMsg:
		return m, m.verbDone(msg)

	case renameMsg:
		return m, m.rename(msg.id, msg.name)

	case renameDoneMsg:
		return m, m.renameDone(msg)

	case editorOpenedMsg:
		return m, m.editorOpened(msg)

	case interactDoneMsg:
		m.busy = ""
		cmds := []tea.Cmd{m.refresh()}
		if msg.err != nil {
			cmds = append(cmds, m.report(true, "%s: %v", msg.action, msg.err))
		}
		return m, tea.Batch(cmds...)

	case folderChosenMsg:
		return m, m.selectFolder(msg.folder)

	case paneOpenedMsg:
		return m, m.paneOpened(msg)

	case workspaceExecsMsg:
		return m, m.workspaceExecs(msg)

	case workspaceTermMsg:
		return m, m.workspaceTermOpened(msg)

	case workspaceTickMsg:
		if msg.gen != m.wsGen {
			return m, nil
		}
		return m, tea.Batch(m.listExecs(msg.gen), m.workspaceTick(msg.gen))

	case workspaceForwardMsg:
		return m, m.workspaceForward(msg)

	case workspaceForwardChangedMsg:
		// Nothing to apply: the header reads the forward itself. This is the
		// redraw, and re-arming the wait is what keeps it coming.
		if msg.gen != m.wsGen || m.forward == nil {
			return m, nil
		}
		return m, m.forwardEvents(msg.gen, m.forward)

	case paneMsg:
		// Addressed to the pane it came from, which may exist before the
		// screen does: a tab can connect while the primary is still opening.
		return m, m.updatePaneMsg(msg)

	case runActionMsg:
		return m, m.actOn(msg.key, msg.targets)

	case harnessesLoadedMsg:
		return m, m.harnessesLoaded(msg)

	case harnessSetupMsg:
		if msg.andDefault {
			return m, m.configureHarnessThen(msg.harness, &msg.harness, msg.resume)
		}
		return m, m.configureHarnessThen(msg.harness, nil, msg.resume)

	case harnessDefaultMsg:
		return m, m.chooseDefaultHarness(msg)

	case resumeRunMsg:
		return m, m.startRun(msg.req)

	case harnessVerbMsg:
		return m, m.runHarnessVerb(msg.verb, msg.harness, nil)

	case harnessFileMsg:
		return m, m.editHarnessFile(msg.harness, msg.path)

	case harnessDoneMsg:
		return m, m.harnessDone(msg)

	case harnessCardMsg:
		m.busy = ""
		if msg.err != nil {
			return m, m.report(true, "cannot read the harness: %v", msg.err)
		}
		m.dialog = textDialog(msg.title, msg.body)
		return m, nil

	case editorDoneMsg:
		m.promptEdited(msg)
		return m, nil

	case shimmerTickMsg:
		return m, m.advanceShimmer(msg)

	case tea.KeyPressMsg:
		cmd := m.updateKey(msg)
		// The composer's height is a function of its contents, so geometry is
		// recomputed after every key rather than only on a resize.
		m.layout()
		return m, cmd
	}

	// Anything the window itself does not handle goes to whatever is focused.
	// For a pane that is most of what it needs: the terminal's output arrives
	// as the pane library's own messages, which nothing here can name, and
	// holding any of them back stops the pane dead.
	if m.inPanes() {
		return m, m.updatePane(msg)
	}
	var cmd tea.Cmd
	m.prompt, cmd = m.prompt.Update(msg)
	return m, cmd
}

// report sets the status line. It is the one path a handler uses to say what
// happened, so a message can never outlive the key that produced it.
func (m *Model) report(isErr bool, format string, args ...any) tea.Cmd {
	return func() tea.Msg { return statusMsg{text: fmt.Sprintf(format, args...), err: isErr} }
}

// keyName is what a handler matches on: the key as the key list spells it.
//
// Bubble Tea reports the keystroke rather than the character, so a typed "V"
// arrives as "shift+v" and a space as "space". The key list promises letters —
// "V range", "Space select" — so the text the terminal actually sent wins
// wherever there is any, and only the keys with no text of their own (enter,
// tab, the arrows) and the modified ones fall back to the keystroke.
func keyName(msg tea.KeyPressMsg) string {
	if msg.Text != "" && msg.Mod&^tea.ModShift == 0 {
		return msg.Text
	}
	if name := msg.String(); name != "space" {
		return name
	}
	return " "
}

// unshiftBackspace folds Shift-Backspace onto Backspace.
//
// A terminal that speaks the Kitty keyboard protocol reports the two as
// different keys, and nothing binds the shifted one — not the handlers here,
// not the textarea's key map — so on those terminals a key that deletes a
// character everywhere else does nothing at all. Shift is not a modifier
// Backspace has a meaning for; it is the same key with a finger left on Shift.
// Ctrl and Alt are left alone, because Alt-Backspace is delete-word.
func unshiftBackspace(msg tea.KeyPressMsg) tea.KeyPressMsg {
	if msg.Code == tea.KeyBackspace {
		msg.Mod &^= tea.ModShift
	}
	return msg
}

func (m *Model) updateKey(msg tea.KeyPressMsg) tea.Cmd {
	msg = unshiftBackspace(msg)

	// The opening flourish is over the moment there is anything to do.
	m.stopShimmer()

	// Whatever was reported was about the previous key. Clearing here rather
	// than in each handler means a message cannot outlive its moment, and the
	// handlers that report something simply report it again.
	m.status, m.statusE = "", false
	m.statusGen++

	// Ctrl-C quits the window, but never from inside a pane: there it belongs
	// to whatever is running, and a key that sometimes interrupts an agent and
	// sometimes closes the window it is running in is a key nobody can press
	// with any confidence. See detachHint.
	if keyName(msg) == "ctrl+c" && (m.focus != focusPane || m.dialog != nil) {
		m.quit = true
		return tea.Quit
	}

	if m.dialog != nil {
		cmd, closed := m.dialog.update(msg)
		if closed {
			m.dialog = nil
		}
		return cmd
	}
	if m.optionsOpen {
		return m.updateOptions(msg)
	}
	if keyName(msg) == "f1" {
		m.dialog = textDialog("Keys", m.helpText())
		return nil
	}
	// The harnesses screen is on a function key because the prompt takes every
	// letter and the list has spent them on its own actions — the same reason
	// help is on F1 and the editor on F2. It is a toggle: the key that opened
	// it puts it away, wherever the screen was reached from.
	//
	// Not from a pane, where every key is the sandbox's: a window key that
	// reached past a terminal would be a key the program in it could never
	// receive. The leader's keys are the way to the window from there.
	if keyName(msg) == harnessesKey && !m.inPanes() {
		if m.harnessesOpen {
			m.closeHarnesses()
			return nil
		}
		return m.openHarnesses()
	}
	if m.harnessesOpen {
		return m.updateHarnesses(msg)
	}

	switch m.focus {
	case focusPane:
		return m.updatePane(msg)
	case focusList:
		return m.updateList(msg)
	case focusFolder:
		return m.updateFolder(msg)
	default:
		return m.updatePrompt(msg)
	}
}

// promptEdited takes back what the editor saved. The buffer is replaced
// wholesale, including with nothing: an editor left empty is how you throw a
// prompt away.
func (m *Model) promptEdited(msg editorDoneMsg) {
	defer os.Remove(msg.path)

	if msg.err != nil {
		m.status, m.statusE = fmt.Sprintf("editor exited: %v", msg.err), true
		return
	}
	edited, err := os.ReadFile(msg.path)
	if err != nil {
		m.status, m.statusE = fmt.Sprintf("cannot read the prompt back: %v", err), true
		return
	}
	m.prompt.SetValue(strings.TrimRight(string(edited), "\n"))
	m.promptEnd()
	m.layout()
	m.status, m.statusE = "", false
}

// updatePrompt handles the default mode. Every key is text, except the ones
// that leave: Up walks out of the top of the field, Enter launches, and Tab
// opens the options.
func (m *Model) updatePrompt(msg tea.KeyPressMsg) tea.Cmd {
	switch keyName(msg) {
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
			return nil
		}
		m.leavePrompt(landLast)
		return nil

	case "down":
		// Down goes to the end of the last line and stops there. The prompt is
		// the bottom of the window, so there is nothing below it to move to —
		// and a Down that jumped to the top of the list would be moving up the
		// screen, which is not what the key says.
		if !m.onLastPromptRow() {
			break
		}
		m.prompt.CursorEnd()
		return nil

	case "tab":
		// Tab is the one key that moves between the halves of the window, in
		// both directions. The options are a layer over it, on Shift-Tab.
		m.leavePrompt(landFirst)
		return nil

	case "shift+tab", "ctrl+o":
		m.optionsOpen = true
		return nil

	case "alt+e", "f2":
		// Alt-E, and F2 for the terminals where Option is not Meta.
		return editPrompt(m.ctx, m.prompt.Value())

	case "enter":
		return m.run()

	case "alt+enter", "ctrl+enter", "ctrl+j":
		// A terminal that can tell Ctrl-Enter apart sends ctrl+enter; most
		// send it as ctrl+j, which is the same byte as Ctrl-J.
		m.prompt.InsertString("\n")
		return nil

	case "ctrl+d":
		// EOF on an empty line quits, the way a shell does. With text in the
		// buffer it is the shell's other meaning, delete forward, which the
		// textarea already implements — so it is passed through.
		if strings.TrimSpace(m.prompt.Value()) == "" {
			m.quit = true
			return tea.Quit
		}

	case "esc":
		return nil
	}

	var cmd tea.Cmd
	m.prompt, cmd = m.prompt.Update(msg)
	return cmd
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

func (m *Model) promptEnd() {
	for m.prompt.Line() < m.prompt.LineCount()-1 {
		m.prompt.CursorDown()
	}
	m.prompt.CursorEnd()
}

// updateList handles the sandbox pane. A letter is a command here, the way it
// is in difftui's file lists, because there is no text to type.
func (m *Model) updateList(msg tea.KeyPressMsg) tea.Cmd {
	switch keyName(msg) {
	case "down", "j":
		// Visual mode holds you in the list: falling out of the bottom of a
		// range you are still drawing would throw the range away.
		if m.list.cursor >= len(m.list.rows())-1 && !m.list.visual {
			m.backToPrompt()
			return nil
		}
		m.list.move(1)
	case "up", "k":
		// Off the top is the folder filter, which is drawn above the list; off
		// the bottom is the prompt, which is drawn below it. Neither edge is a
		// dead stop, and both lead where the eye is already going.
		if m.list.cursor == 0 && !m.list.visual {
			m.focus = focusFolder
			return nil
		}
		m.list.move(-1)
	case "left", "h":
		// A long name is ellipsized; the row under the cursor can be walked
		// sideways to read the rest of it.
		m.list.scrollName(-4)
	case "right", "l":
		if !m.list.scrollName(4) {
			return status("nothing more to the right")
		}
	case "pgdown":
		m.list.move(m.list.height)
	case "pgup":
		m.list.move(-m.list.height)
	case "home", "g":
		m.list.moveTo(0)
	case "end", "G":
		m.list.moveTo(len(m.list.rows()) - 1)

	case "esc":
		if m.list.visual {
			m.list.visual = false
			return status("visual select canceled")
		}
		m.backToPrompt()
		return nil
	case "tab":
		if m.list.visual {
			m.list.visual = false
			return status("visual select canceled")
		}
		// Tab goes round the window in the order it is drawn, bottom to top:
		// the prompt, the discoboxes, the folder they are filtered to, and back
		// to the prompt. Esc is the way straight out.
		m.focus = focusFolder
		return nil
	case "shift+tab", "ctrl+o":
		m.optionsOpen = true
	case "V":
		m.list.toggleVisual()
		if !m.list.visual {
			return status("visual select canceled")
		}
		return nil
	case " ":
		if m.list.visual {
			return status("selected %s", plural(m.list.commitVisual(), "box", "boxes"))
		}
		m.list.toggleSelect()
		m.list.move(1)
	case "c":
		m.list.clearSelection()
		m.list.visual = false
		return status("selection cleared")
	case "A":
		m.list.showArchived = !m.list.showArchived
		m.list.clamp()
		m.layout()
		if m.list.showArchived {
			return status("showing archived discoboxes")
		}
		return status("hiding archived discoboxes")
	case "r":
		return tea.Batch(m.refresh(), status("refreshing"))
	case "?":
		m.dialog = textDialog("Keys", m.helpText())
	case "enter":
		return m.act("a")
	case ".":
		targets := m.list.targets()
		m.dialog = actionsDialog(actionTitle(targets), "", m.actions(targets), chooseAction(targets))
	default:
		for _, a := range m.actions(m.list.targets()) {
			if a.key == keyName(msg) {
				return m.act(a.key)
			}
		}
	}
	return nil
}

func (m *Model) backToPrompt() {
	m.focus = focusPrompt
	m.prompt.Focus()
}

// leavePrompt moves focus up out of the composer: to the sandboxes, or to the
// folder filter when there are none.
//
// An empty list is exactly when the filter is the thing you want — the folder
// you are standing in has nothing in it, and the sandboxes are somewhere else —
// so landing on it beats refusing to move and leaving no way to reach it.
func (m *Model) leavePrompt(landing listLanding) {
	// Reaching past the prompt is the ask for everything behind it — and the
	// first time, the list arrives with it. Up means "the row nearest the
	// prompt" only once there are rows on screen to be near; opening the window
	// out, there are none yet, so it lands at the top like everything else.
	if !m.expanded {
		landing = landFirst
	}
	m.expand()
	m.prompt.Blur()
	if len(m.list.rows()) == 0 {
		m.focus = focusFolder
		return
	}
	m.focus = focusList
	// Where you were beats where the key points: leaving the list to type
	// something and coming back should put the cursor back on the sandbox you
	// left it on, whichever key brings you back.
	if m.list.visited {
		return
	}
	if landing == landLast {
		m.list.moveTo(len(m.list.rows()) - 1)
		return
	}
	m.list.moveTo(0)
}

// listLanding is where the cursor goes the first time focus enters the list,
// before it has been anywhere of its own.
type listLanding int

const (
	// The top, for the keys that are not a direction into the list: Tab, and
	// Down off the end of the prompt, which has wrapped round to the top.
	landFirst listLanding = iota
	// The row nearest the prompt, for Up — which is a cursor moving up out of
	// the composer into the row directly above it.
	landLast
)

// updateOptions handles the run options panel.
func (m *Model) updateOptions(msg tea.KeyPressMsg) tea.Cmd {
	opt := m.opts.current()
	switch keyName(msg) {
	case "esc", "tab", "shift+tab", "ctrl+o":
		m.optionsOpen = false
		return nil
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
			m.dialog = inputDialog(opt.label, opt.hint, opt.placeholder, opt.value, func(v string) tea.Cmd {
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
		return m.run()
	}
	return nil
}

// ---------------------------------------------------------------------------
// actions

// actions is the base level CLI, filtered against what the current targets can
// actually do. Unavailable actions stay on the menu with the reason, rather
// than disappearing and leaving you wondering where upgrade went.
func (m *Model) actions(targets []Sandbox) []action {
	one := len(targets) == 1
	anyUpgrade, anyRunning, anyStopped := false, false, false
	anyArchived, allArchived := false, len(targets) > 0
	for _, s := range targets {
		anyUpgrade = anyUpgrade || s.Upgrade
		anyRunning = anyRunning || s.State == StateRunning
		anyStopped = anyStopped || s.State == StateStopped
		anyArchived = anyArchived || s.State == StateArchived
		allArchived = allArchived && s.State == StateArchived
	}
	attachable := one && targets[0].attachable()
	// Apply runs git in the sandbox, so an archived one — which has no
	// container to run it in — cannot take it however much it changed. A
	// diffstat that has not come back yet is not the same answer as "nothing
	// changed", so it stays available until it has.
	applyable, applyWhy := false, "nothing has changed yet"
	for _, s := range targets {
		if s.State == StateArchived {
			continue
		}
		if s.hasDiff() || !s.Diff.Known {
			applyable = true
		}
	}
	if !applyable && anyArchived {
		applyWhy = "an archived box has no working tree to look at"
	}
	// A row named by its terminal's title is not showing the configured name,
	// which is the one rename edits: accepting a rename there would change
	// nothing on screen.
	renameable, renameWhy := one, "takes exactly one box"
	if one && targets[0].NameIsTitle {
		renameable = false
		renameWhy = "the name shown is the terminal's title, which the harness sets — a rename would not change it"
	}
	return []action{
		{key: "a", label: "attach", detail: "join the harness terminal", enabled: attachable,
			why: attachWhy(one, targets)},
		{key: "s", label: "shell", detail: "open a shell in the box", enabled: attachable,
			why: attachWhy(one, targets)},
		{key: "y", label: "apply", detail: "bring the changes back to " + m.session.Directory, enabled: applyable,
			why: applyWhy},
		{key: vscodeKey, label: "vscode", detail: "open the box in VS Code, in a window of its own", enabled: attachable,
			why: attachWhy(one, targets)},
		{key: renameKey, label: "rename", detail: "type a new name for the box", enabled: renameable,
			why: renameWhy},
		{key: "u", label: "upgrade", detail: "re-pin to the current harness image", enabled: anyUpgrade,
			why: "already on the current image"},
		{key: "t", label: "stop", detail: "power the box off, keeping its disk", enabled: anyRunning,
			why: "not running"},
		{key: "T", label: "start", detail: "power a stopped box back on", enabled: anyStopped,
			why: "already on, or archived"},
		{key: "x", label: "archive", detail: "put it away, disk and all, reversibly", enabled: !allArchived,
			why: "already archived"},
		{key: "U", label: "unarchive", detail: "bring an archived box back", enabled: anyArchived,
			why: "not archived"},
		{key: "P", label: "purge", detail: "destroy it and its data, for good", enabled: allArchived,
			why: "archive it first — purge only takes archived boxes"},
	}
}

func attachWhy(one bool, targets []Sandbox) string {
	if !one {
		return "takes exactly one box"
	}
	if len(targets) == 1 && !targets[0].attachable() {
		return "the box is " + string(targets[0].State)
	}
	return ""
}

func actionTitle(targets []Sandbox) string {
	if len(targets) == 1 {
		return targets[0].Name
	}
	return plural(len(targets), "box", "boxes") + " selected"
}

// chooseAction is the action menu's callback: it turns the chosen key into a
// message, which the update loop runs against the live model.
func chooseAction(targets []Sandbox) func(string) tea.Cmd {
	return func(key string) tea.Cmd {
		return func() tea.Msg { return runActionMsg{key: key, targets: targets} }
	}
}

func (m *Model) act(key string) tea.Cmd {
	// A command acts on the visual range and ends the mode, the way staging a
	// visual selection does in difftui.
	targets := m.list.targets()
	m.list.visual = false
	return m.actOn(key, targets)
}

// actOn runs one action against the given sandboxes.
//
// The two kinds part here: a verb goes to the API and comes back, leaving the
// window up; an interaction owns the terminal, so the window suspends and hands
// over the real streams until it returns.
func (m *Model) actOn(key string, targets []Sandbox) tea.Cmd {
	if len(targets) == 0 {
		return status("nothing selected")
	}
	var chosen *action
	for _, a := range m.actions(targets) {
		if a.key == key {
			chosen = &a
			break
		}
	}
	if chosen == nil {
		return nil
	}
	if !chosen.enabled {
		m.dialog = errorDialog("Cannot "+chosen.label, chosen.why)
		return nil
	}

	ids := make([]string, 0, len(targets))
	for _, s := range targets {
		ids = append(ids, s.ID)
	}

	if key == renameKey {
		return m.askRename(targets[0])
	}
	if key == vscodeKey {
		return m.openEditor(targets[0])
	}

	if action, ok := interactions[key]; ok {
		// On the workspace screen there is one discobox: attach goes back to
		// its terminal, shell opens a fresh tab, and a command that runs and
		// finishes takes the screen over them until it does.
		if m.inPanes() {
			switch action {
			case InteractAttach:
				// Back to the primary, which is what attach means here: it is
				// the session the workspace is a view onto.
				m.focusOrdinal(0)
				return nil
			case InteractShell:
				return m.newShell()
			default:
				return m.openOverlay(action, targets[0])
			}
		}
		// From the list, a terminal is drawn in the window; a command that
		// wants the real terminal gets it, and the window steps aside.
		if action.paneable() {
			if len(targets) != 1 {
				return status("%s takes exactly one box", action)
			}
			return m.openFromList(action, targets[0])
		}
		return m.interact(action, ids)
	}

	verb, ok := verbs[key]
	if !ok {
		return nil
	}
	if verb == VerbPurge {
		// Archiving is reversible and needs no ceremony. Purging is not.
		question := fmt.Sprintf("Purge %s? The disk and everything on it goes, and unarchive cannot bring it back.",
			actionTitle(targets))
		m.dialog = confirmDialog("Purge", question, func(string) tea.Cmd {
			return func() tea.Msg { return runVerbMsg{verb: VerbPurge, ids: ids} }
		})
		return nil
	}
	return m.runVerb(verb, ids)
}

// runVerbMsg is a confirmed verb on its way back to the live model, for the
// same reason the action menu emits a message rather than running anything.
type runVerbMsg struct {
	verb Verb
	ids  []string
}

var verbs = map[string]Verb{
	"u": VerbUpgrade,
	"t": VerbStop,
	"T": VerbStart,
	"x": VerbArchive,
	"U": VerbUnarchive,
	"P": VerbPurge,
}

var interactions = map[string]Interaction{
	"a": InteractAttach,
	"s": InteractShell,
	"y": InteractApply,
}

// runVerb applies a lifecycle verb to every target, in one command: the window
// reports once, on the whole batch, rather than flickering a line per sandbox.
func (m *Model) runVerb(verb Verb, ids []string) tea.Cmd {
	m.busy = string(verb) + "…"
	return func() tea.Msg {
		errs := make([]error, len(ids))
		for i, id := range ids {
			errs[i] = m.ds.Do(m.ctx, verb, id)
		}
		return verbDoneMsg{verb: verb, ids: ids, errs: errs}
	}
}

func (m *Model) verbDone(msg verbDoneMsg) tea.Cmd {
	m.busy = ""
	// A verb that acted on anything is worth clearing the selection for: the
	// marks were put there to run this, and leaving them invites running it
	// twice.
	failed := 0
	var firstErr error
	for _, err := range msg.errs {
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	done := len(msg.ids) - failed
	if done > 0 {
		m.list.clearSelection()
	}
	switch {
	case failed == 0:
		return tea.Batch(m.refresh(), m.report(false, "%s: %s", msg.verb, plural(done, "box", "boxes")))
	case done == 0 && len(msg.ids) == 1:
		return tea.Batch(m.refresh(), m.report(true, "%s: %v", msg.verb, firstErr))
	default:
		return tea.Batch(m.refresh(), m.report(true, "%s: %d of %d failed: %v", msg.verb, failed, len(msg.ids), firstErr))
	}
}

// ---------------------------------------------------------------------------
// vscode

// vscodeKey is the letter VS Code answers to, in the list and behind the leader
// alike. It is bound on the workspace screen too, where it is the same sandbox
// opened a second way: the terminal stays where it is and the editor arrives
// beside it.
const vscodeKey = "v"

// editorOpenedMsg is what came of handing a sandbox to the editor.
type editorOpenedMsg struct {
	name string
	err  error
}

// openEditor hands one sandbox to VS Code and reports on the status line.
//
// Nothing is suspended for it. The editor is another program in another window,
// and the CLI's part is over as soon as it has been told which host to connect
// to — so this is a request that returns, like a verb, rather than something
// that owns the screen.
func (m *Model) openEditor(box Sandbox) tea.Cmd {
	m.busy = "vscode…"
	ctx, ds, id, name := m.ctx, m.ds, box.ID, box.Name
	return func() tea.Msg {
		return editorOpenedMsg{name: name, err: ds.OpenEditor(ctx, id)}
	}
}

func (m *Model) editorOpened(msg editorOpenedMsg) tea.Cmd {
	m.busy = ""
	if msg.err != nil {
		return m.report(true, "vscode: %v", msg.err)
	}
	return m.report(false, "opened %s in VS Code", msg.name)
}

// ---------------------------------------------------------------------------
// rename

// renameKey is the letter rename answers to in the list. It is deliberately not
// bound on the workspace screen: rename needs a name typed into a dialog, and
// the discobox on screen is one you are already looking at by name.
const renameKey = "e"

// renameMsg is an accepted name on its way back to the live model, the same way
// the action menu and a confirmation hand their answers back.
type renameMsg struct{ id, name string }

type renameDoneMsg struct {
	name string
	err  error
}

// askRename opens the input dialog on the name the discobox already has, so the
// common edit — a word added to a name that is nearly right — is a word typed
// rather than a name retyped.
func (m *Model) askRename(box Sandbox) tea.Cmd {
	m.dialog = inputDialog("Rename", "What should this discobox be called?", "name", box.Name,
		func(name string) tea.Cmd {
			if name == "" || name == box.Name {
				return status("name unchanged")
			}
			return func() tea.Msg { return renameMsg{id: box.ID, name: name} }
		})
	return nil
}

func (m *Model) rename(id, name string) tea.Cmd {
	m.busy = "rename…"
	return func() tea.Msg {
		return renameDoneMsg{name: name, err: m.ds.Rename(m.ctx, id, name)}
	}
}

func (m *Model) renameDone(msg renameDoneMsg) tea.Cmd {
	m.busy = ""
	if msg.err != nil {
		return m.report(true, "rename: %v", msg.err)
	}
	return tea.Batch(m.refresh(), m.report(false, "renamed to %s", msg.name))
}

// interact suspends the window and hands the terminal to the action for as long
// as it runs. tea.Exec drops out of the alternate screen and restores the
// terminal around it, so the command runs on the screen the window was started
// from and the window comes back over the top of it.
func (m *Model) interact(act Interaction, ids []string) tea.Cmd {
	m.busy = string(act) + "…"
	return m.exec(&interactExec{ctx: m.ctx, ds: m.ds, action: act, ids: ids}, func(err error) tea.Msg {
		return interactDoneMsg{action: act, err: err}
	})
}

// ---------------------------------------------------------------------------
// run

// run is Enter in the prompt: the whole point of the window.
//
// An empty prompt is not an error. It means the other thing you come here for:
// a sandbox of your own to work in, with no harness given anything to do.
func (m *Model) run() tea.Cmd {
	req := m.opts.request(m.prompt.Value())
	if cmd, stop := m.askForADefaultHarness(req); stop {
		return cmd
	}
	if cmd, stop := m.askToSetUpHarness(req); stop {
		return cmd
	}
	return m.startRun(req)
}

// startRun is the run itself, past the questions about which harness it lands
// on. A run those questions interrupted resumes here rather than at run(): they
// have already been answered, and asking again against a listing that has not
// caught up yet would ask the same one twice.
func (m *Model) startRun(req RunRequest) tea.Cmd {
	if req.IncludeDirty != "" {
		return m.create(req)
	}
	// --include-dirty=auto means ask, and there is only something to ask about
	// when the working tree has something in it.
	m.busy = "checking the working tree…"
	return func() tea.Msg {
		dirty, err := m.ds.Dirty(m.ctx, req.Source)
		return dirtyCheckedMsg{req: req, dirty: dirty, err: err}
	}
}

// askForADefaultHarness stops a run that has named no harness in a project with
// no default, and asks which harness should be the default. It reports whether
// the run was stopped.
//
// The server refuses this create outright (ADR 0048), so the alternative to
// asking is the same refusal a moment later with the answer left to the user to
// find. Asking here is also the only point that knows what the project has to
// offer.
//
// `shell` is not among the choices. It runs like any other harness and is
// chosen like any other, but a project whose default is a login shell has no
// coding harness by default, which is the state this is trying to leave.
func (m *Model) askForADefaultHarness(req RunRequest) (tea.Cmd, bool) {
	if req.Harness != "" || m.projectDefaultHarness() != nil {
		return nil, false
	}
	if !m.harnesses.loaded {
		// The listing has not landed yet, so there is nothing to ask about with
		// any confidence. The server refuses a create it cannot resolve, which
		// is the answer this would only be guessing at.
		return nil, false
	}
	candidates := m.defaultCandidates()
	if len(candidates) == 0 {
		return m.report(true, "this project has no harness to run; register one with `disco box harnesses create`"), true
	}

	items := make([]action, 0, len(candidates))
	for i, harness := range candidates {
		detail := "make it the project default"
		if harness.State != HarnessEnabled {
			detail = "set it up, then make it the default"
		}
		items = append(items, action{
			key: itoa(i + 1), label: harness.displayName(), detail: detail, enabled: true,
		})
	}
	chosen := candidates
	menu := actionsDialog("Which harness should this project run?",
		"Nothing is set as the project default, so a discobox has no harness to start.",
		items, func(key string) tea.Cmd {
			for i, harness := range chosen {
				if itoa(i+1) == key {
					return func() tea.Msg { return harnessDefaultMsg{harness: harness, resume: &req} }
				}
			}
			return nil
		})
	menu.footer = "Enter chooses · Esc cancels"
	m.dialog = menu
	return nil, true
}

// chooseDefaultHarness acts on that choice: a harness that already works
// becomes the default outright, and one that does not is set up first. Either
// way the run that asked the question runs when it is answered.
func (m *Model) chooseDefaultHarness(msg harnessDefaultMsg) tea.Cmd {
	if msg.harness.State == HarnessEnabled {
		return m.runHarnessVerb(HarnessSetDefault, msg.harness, msg.resume)
	}
	return m.configureHarnessThen(msg.harness, &msg.harness, msg.resume)
}

// projectDefaultHarness is the harness the project runs when nothing says
// otherwise, or nil when it has named none.
func (m *Model) projectDefaultHarness() *Harness {
	for _, harness := range m.harnesses.all {
		if harness.Default {
			return &harness
		}
	}
	return nil
}

// defaultCandidates are the harnesses worth offering as a project default: all
// of them except `shell`, in the order the listing reports.
func (m *Model) defaultCandidates() []Harness {
	var out []Harness
	for _, harness := range m.harnesses.all {
		if !harness.Shell {
			out = append(out, harness)
		}
	}
	return out
}

// askToSetUpHarness stops a run whose harness cannot run, and offers the way
// out. It reports whether the run was stopped.
//
// A harness that has never been through its setup has no credentials, and the
// server refuses the sandbox at create — so the alternative to asking here is
// an error a few seconds later saying the same thing with nothing to do about
// it. The offer is the same configure flow the harnesses screen runs.
func (m *Model) askToSetUpHarness(req RunRequest) (tea.Cmd, bool) {
	if req.Harness == "" {
		// Nothing chosen: whatever the project default resolves to at create is
		// the server's answer, and it is entitled to give it.
		return nil, false
	}
	harness, ok := m.harnessNamed(req.Harness)
	if !ok || harness.State == HarnessEnabled {
		return nil, false
	}
	name := harness.displayName()
	if !harness.Configurable {
		// Nothing to offer: it declares no setup, so it is not the setup that
		// is missing. Saying so beats a confirm whose yes does nothing.
		return m.report(true, "%s cannot be run and has no setup to run", name), true
	}
	m.dialog = confirmDialog("Set up "+name+"?",
		name+" has not been set up, so a discobox cannot be created on it. Run its setup now? It takes the terminal and asks its own questions.",
		func(string) tea.Cmd {
			return func() tea.Msg { return harnessSetupMsg{harness: harness, resume: &req} }
		})
	return nil, true
}

// harnessNamed finds the harness a run request names, by the same name the
// request carries it as — what `--harness` takes.
func (m *Model) harnessNamed(name string) (Harness, bool) {
	for _, harness := range m.harnesses.all {
		if harness.flagName() == name {
			return harness, true
		}
	}
	return Harness{}, false
}

func (m *Model) dirtyChecked(msg dirtyCheckedMsg) tea.Cmd {
	m.busy = ""
	if msg.err != nil {
		return m.report(true, "cannot read the working tree: %v", msg.err)
	}
	if !msg.dirty {
		return m.create(msg.req)
	}
	// Excluding leads, the way it does in `disco run`: the default answer is
	// the one that changes nothing about what the sandbox sees.
	req := msg.req
	m.dialog = confirmDialog("Uncommitted changes",
		m.session.Directory+" has uncommitted changes. Carry them into the discobox as a snapshot on top of the checked-out commit?",
		func(string) tea.Cmd {
			req.IncludeDirty = "true"
			return func() tea.Msg { return createMsg{req: req} }
		})
	// Answering no is answering, not canceling, so the sandbox is still
	// created — from the last commit.
	m.dialog.onCancel = func() tea.Cmd {
		req.IncludeDirty = "false"
		return func() tea.Msg { return createMsg{req: req} }
	}
	return nil
}

// createMsg carries a settled request back to the live model.
type createMsg struct{ req RunRequest }

func (m *Model) create(req RunRequest) tea.Cmd {
	m.busy = "creating the discobox…"
	return func() tea.Msg {
		sandbox, err := m.ds.Run(m.ctx, req)
		return createdMsg{sandbox: sandbox, req: req, err: err}
	}
}

func (m *Model) created(msg createdMsg) tea.Cmd {
	m.busy = ""
	if msg.err != nil {
		return m.report(true, "cannot create the discobox: %v", msg.err)
	}
	// The prompt has been spent. Clearing it is what makes the window usable
	// twice in a row without reaching for a delete key.
	m.prompt.SetValue("")
	m.layout()
	if msg.req.Detach {
		return tea.Batch(m.refresh(), m.report(false, "created %s", msg.sandbox.ID))
	}
	return tea.Batch(m.refresh(), m.openFromList(InteractAttach, msg.sandbox))
}

// ---------------------------------------------------------------------------
// layout and view

// inner is the width the window has to lay out in: everything sits inside the
// box, so the two border columns and the one cell of breathing room on each
// side of them come off the top before anything is measured.
func (m *Model) inner() int {
	return max(m.width-boxChrome, 1)
}

// bodyWidth is what a full-window list gets: everything inside the box, less
// the mark when there is width to spare for one beside it. The mark gives its
// columns back on a terminal narrow enough that the rows need them more.
func (m *Model) bodyWidth() int {
	if m.showLogo() {
		return m.inner() - m.logo.column()
	}
	return m.inner()
}

// boxPad is the breathing room between the border and the content. Content
// butted straight against a border reads as spilling out of it.
const boxPad = 1

// boxChrome is what the border costs a row: the two edges and the padding.
const boxChrome = 2 + 2*boxPad

// windowChrome is what the window costs in rows before any sandbox is in it.
// See layout, which is the only place it is used and where it is counted out.
const windowChrome = 11

func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	// The harnesses screen is a window of its own, and is measured whether or not
	// it is up: it is opened by a key that can be pressed on any frame, and a
	// list sized on the frame after that would open with no rows in it.
	m.harnesses.width, m.harnesses.height = m.bodyWidth(), max(m.height-harnessesChrome, 0)
	m.harnesses.clamp()
	if !m.expanded && !m.inPanes() {
		m.compactLayout()
		return
	}
	// The workspace takes the whole window: a terminal wants every row it can
	// get, and the list underneath is not what you are looking at. Every pane
	// is sized for the box it is drawn in, the hidden tabs included — flipping
	// to one must show a screen drawn at the size it is shown at.
	if m.inPanes() {
		for _, p := range m.panes() {
			p.term.SetSize(m.paneCells(m.paneWidthOf(p)))
		}
		if m.overlay != nil {
			// It has the screen, whatever is under it.
			m.overlay.term.SetSize(m.paneCells(m.width))
		}
		return
	}
	// The composer grows with what is typed, one line at a time, the way
	// Claude Code's does — and the list gives up a row for each one it takes.
	promptH := min(max(m.prompt.LineCount(), 1), 8)
	// What the window costs before a single sandbox is drawn: the box's two
	// edges, the header and the blank under it, the list title and the blank
	// below the rows, the composer's label, its own two rules, the mode line
	// and the status line. The floor is no rows at all rather than one: on a
	// terminal this short the composer is the whole point.
	room := max(m.height-promptH-windowChrome, 0)

	// The window fills the terminal, so the list takes every row the composer
	// and the chrome leave it and pads the rest. A list shorter than its pane
	// is a pane with space at the bottom, which is what a full-screen window
	// looks like — not a reason to shrink the frame.
	m.list.width, m.list.height = m.bodyWidth(), room
	m.list.clamp()
	m.prompt.SetWidth(max(m.inner()-2, 10))
	m.prompt.SetHeight(promptH)
}

func (m *Model) View() tea.View {
	if m.quit {
		return tea.NewView("")
	}
	if !m.ready {
		return tea.NewView("Loading…")
	}

	// A modal is drawn in place of the window rather than over it, and closing
	// it puts the window back. It carries its own border, so it needs none of
	// the frame below.
	if m.dialog != nil {
		return m.altView(m.center(m.dialog.view(m.st, m.width, m.height)))
	}
	if m.optionsOpen {
		return m.altView(m.center(m.opts.view(m.st, m.width, m.prompt.Value())))
	}

	// The header spans the window; under it the mark stands beside the list,
	// and the composer spans both again.
	body := m.list.view(m.st, m.focus == focusList)
	if m.showLogo() {
		// The mark is centered against whatever the list came out at, so it
		// follows the window rather than the window working around it.
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.logo.view(lipgloss.Height(body)), body)
	}

	// The header names the session; what is under it is a different kind of
	// thing, and butted together they read as one block.
	var content string
	switch {
	case !m.expanded && !m.inPanes():
		content = m.viewCompact()
	case m.inPanes():
		// A pane wears the border itself. Everything else — the header, what
		// the sandbox is called, the keys — sits outside it, the way a caption
		// sits outside the thing it captions.
		content = m.paintChrome(m.viewPaneWindow())
	case m.harnessesOpen:
		// The harnesses screen is the window while it is up, drawn in the same box
		// with the same header: it is another list of things you act on, not a
		// panel over the launcher.
		content = m.viewHarnesses()
	default:
		rows := []string{m.viewHeader(m.inner()), ""}
		rows = append(rows, strings.Split(body, "\n")...)
		rows = append(rows, strings.Split(m.viewPrompt(), "\n")...)
		content = m.box("", rows)
	}

	view := tea.NewView(content)
	// The whole terminal, once the window has opened out — but not before: the
	// opening prompt is inline, sitting under the command that started it.
	view.AltScreen = m.expanded || m.inPanes()
	view.MouseMode = m.paneMouseMode()
	view.WindowTitle = m.windowTitle()
	// The cursor belongs to whatever is drawing one. A pane places it where the
	// sandbox put it; everywhere else the composer's own virtual cursor does
	// the job and there is nothing to place.
	if cursor := m.paneCursor(); cursor != nil {
		view.Cursor = cursor
	}
	return view
}

// box draws the window's border around content already laid out at inner()
// cells wide.
//
// It is drawn by hand rather than with a bordered style because such a style
// re-wraps any line as wide as the box — and a terminal grid that gets
// re-wrapped shifts every row below the wrap, desyncing the hardware cursor
// from the screen the sandbox believes it is drawing on. Every row is fitted to
// the cell here instead, so the frame is fixed and the cursor offsets in
// paneCursor are exact.
func (m *Model) box(title string, rows []string) string {
	inner := m.inner()
	pad := strings.Repeat(" ", boxPad)
	side := m.st.frame.Render("│")
	edge := inner + 2*boxPad

	out := make([]string, 0, len(rows)+2)
	out = append(out, titledEdge(m.st, m.st.frame, title, "", edge))
	for _, row := range rows {
		out = append(out, side+pad+padANSI(row, inner)+pad+side)
	}
	out = append(out, m.st.frame.Render("╰"+strings.Repeat("─", edge)+"╯"))
	return strings.Join(out, "\n")
}

// titledEdge draws a box's top edge with a title laid into it, and an already
// rendered control — the maximize button, or nothing — laid into its right end.
//
// It goes on the border rather than above it because the border is a line the
// eye already follows, so a word set into it costs no row at all — and because
// it is a line with nothing else on it, which is the one place a title can be
// centered without being squeezed out by whatever is beside it.
//
// The brackets are what make it read as set into the line rather than as a gap
// in it: bare text with space either side leaves the border looking broken where
// the title sits. A title with no room for rule on both sides is dropped.
func titledEdge(st *styles, edge lipgloss.Style, title, control string, width int) string {
	// The control keeps a cell of rule between it and the corner, the way the
	// title keeps rule on both sides, and the title is centered in what it
	// leaves rather than in the whole edge — a title that slid under the button
	// as the box narrowed would read as one label.
	tail := edge.Render("╮")
	if control != "" {
		tail = control + edge.Render("─╮")
		width = max(width-lipgloss.Width(control)-1, 0)
	}
	rule := strings.Repeat("─", width)
	if title = strings.TrimSpace(title); title == "" {
		return edge.Render("╭"+rule) + tail
	}
	label := edge.Render("[") + st.headerBar.Render(" "+title+" ") + edge.Render("]")
	labelW := lipgloss.Width(label)
	if labelW > width-4 {
		return edge.Render("╭"+rule) + tail
	}
	left := (width - labelW) / 2
	return edge.Render("╭"+strings.Repeat("─", left)) + label +
		edge.Render(strings.Repeat("─", width-left-labelW)) + tail
}

// altView is a frame on the alternate screen, for the layers that stand in
// place of the window rather than inside it.
// center puts a modal surface in the middle of the terminal. A dialog is the
// only thing on screen while it is up, so the window it is drawn over is empty
// space, and hanging it off the top-left corner leaves all of that space on two
// sides of it.
func (m *Model) center(content string) string {
	if m.width <= 0 || m.height <= 0 {
		return content
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, content)
}

func (m *Model) altView(content string) tea.View {
	view := tea.NewView(content)
	view.AltScreen = true
	return view
}

// showLogo reports whether there is width to spare for the mark. Below the
// threshold the list takes the whole row: decoration is the first thing a
// narrow terminal should lose.
func (m *Model) showLogo() bool {
	return m.logo.height() > 0 && m.inner() >= minWidthForLogo
}

func (m *Model) viewHeader(width int) string {
	return spread(m.viewHeaderLeft(), m.viewHeaderRight(), width)
}

// viewHeaderLeft is where you are: the project when it is not the usual one,
// and the folder the window is working in.
func (m *Model) viewHeaderLeft() string {
	return m.viewHeaderBrand() + m.viewFolder()
}

// viewHeaderBrand is the program's own name, and the project it is pointed at
// when that is not the one you are almost always in — a header that says
// "default" every time teaches you to skip it.
//
// It is a piece of its own rather than the head of viewHeaderLeft because the
// workspace's banner gives it up separately from the folder beside it when the
// row runs out of room (viewPaneHeader).
func (m *Model) viewHeaderBrand() string {
	brand := m.st.headerLabel.Render("disco  ")
	if m.session.Project != "" && m.session.Project != m.session.DefaultProject {
		brand += m.st.headerBar.Render(m.session.Project) + m.st.headerLabel.Render("  ")
	}
	return brand
}

// viewHeaderRight is the keys that work where you are. A pane owns the
// keyboard, so the ones the header offers everywhere else are not among them;
// the one that is, is the way out.
func (m *Model) viewHeaderRight() string {
	if p := m.focusedPane(); p != nil {
		return m.st.dimText.Render(m.detachHint() + " detach  ·  " + m.leader() + " " + paneQuitKey + " quit")
	}
	// The harnesses screen is advertised here rather than on the status line
	// because it is reachable from every one of the window's own screens, and
	// the status line says what the screen you are on can do.
	return m.st.dimText.Render("F1 help  ·  F3 harnesses  ·  Ctrl-C quit")
}

// windowTitle is what the terminal running this window should call itself.
//
// The primary terminal's title goes here as well as into the header: the header
// says what is in the window, and the terminal's own title bar is how you find
// the window among the others you have open — a tab reading "go test ./..." is
// worth more than one reading the name of the program that launched it.
//
// It is the primary rather than whatever has focus because the title bar is
// read from outside the window, where the point is which discobox this is and
// what its agent is doing. A shell tab opened to look something up, or a report
// on the screen for as long as it takes to read, is a thing you are doing
// inside this window and already looking at; renaming the window after it would
// churn the tab you find it by.
//
// With no primary it is empty, which leaves the title as whatever started this
// window set it: a launcher with nothing running in it has nothing to say that
// the terminal's own title does not already.
func (m *Model) windowTitle() string {
	p := m.primary()
	if p == nil {
		return ""
	}
	if title := strings.TrimSpace(p.term.Title()); title != "" {
		return title
	}
	return displayName(p.sandbox)
}

// viewFolder draws the folder filter: a path with a caret after it, which is
// what says it can be opened. Focused it wears the arrows that say left and
// right change it, the same way the run options panel marks its own rows.
func (m *Model) viewFolder() string {
	label := m.folderLabel()
	if m.focus != focusFolder {
		return m.st.headerLabel.Render(label + " ▾")
	}
	return m.st.key.Render("‹ ") + m.st.cursorName.Render(label) + m.st.key.Render(" ›")
}

// viewPrompt draws the composer the way Claude Code draws its own: a rule
// above and below the text, a chevron in front of it, and one dim line under
// the rule for the mode you are in. The rule brightens when the prompt has
// focus, which is the only thing that has to be visible from across the room.
func (m *Model) viewPrompt() string {
	// What the field does sits against the field; the keys are keys, and
	// belong on the status line with everything else transient.
	return m.viewComposer(m.inner()) + "\n" + m.viewStatus()
}

// viewComposer is the field and everything that describes it, at the given
// width: the label above, a rule either side of the text, and the chip strip
// saying what Enter will do.
func (m *Model) viewComposer(width int) string {
	ruleStyle := m.st.ruleOn
	if m.focus != focusPrompt {
		ruleStyle = m.st.rule
	}
	// The composer's own rules stop short of its edges on both sides: run into
	// the border and they read as the box broken in half rather than as a
	// separator inside it.
	rule := "  " + ruleStyle.Render(strings.Repeat("─", max(width-4, 1)))
	mode := padANSI("  "+m.opts.chips(m.st), width)
	return lipgloss.JoinVertical(lipgloss.Left,
		m.viewLabel(width), padANSI(rule, width), m.prompt.View(), padANSI(rule, width), mode)
}

// viewLabel is the line above the composer, and says what pressing Enter in it
// does. It does not change with focus: it is a label on the field, not a hint
// about the moment.
func (m *Model) viewLabel(width int) string {
	label := "Enter runs the prompt in a new discobox, or just creates one when it is empty"
	if width < lipgloss.Width(label)+4 {
		// Too narrow to say it all; the field speaks for itself at this size.
		label = "Enter runs the prompt in a new discobox"
	}
	if m.focus != focusPrompt {
		return padANSI("  "+m.st.dimText.Render(label), width)
	}
	return padANSI("  "+m.st.chip.Render(label), width)
}

// viewStatus is the bottom line: the keys, or what just happened. A message
// displaces the keys until the next one is pressed.
func (m *Model) viewStatus() string {
	left := "  " + m.st.dimText.Render(m.hints())
	switch {
	case m.statusE:
		left = "  " + m.st.statusER.Render("✗ "+m.status)
	case m.status != "":
		left = "  " + m.st.statusOK.Render(m.status)
	case m.busy != "":
		left = "  " + m.st.statusWA.Render(m.busy)
	}
	right := ""
	if n := m.list.selectionCount(); n > 0 {
		right = m.st.statusWA.Render(plural(n, "selected", "selected")) + "  "
	}
	return spread(left, right, m.inner())
}

func (m *Model) hints() string {
	if m.harnessesOpen {
		return m.harnessHints()
	}
	switch m.focus {
	case focusPane:
		p := m.focusedPane()
		if p == nil {
			return ""
		}
		if p.exited {
			hints := "finished · q closes"
			if p.term.ScrollbackLen() > 0 {
				hints = "finished · ↑↓ pgup/pgdn scroll · q closes"
			}
			if p != m.overlay {
				hints += " · ←/→ pane"
			}
			return hints
		}
		// A workspace terminal is the discobox's own and you detach from the
		// whole workspace; the command over them is this CLI's, and you close
		// it alone.
		what, out := "the box", "detach"
		if p == m.overlay {
			what, out = string(p.action), "close"
		}
		hints := "every key goes to " + what + " · " + m.detachHint() + " " + out
		if m.overlay == nil {
			// Only the shell is offered here. Another terminal is the advanced
			// one of the two — a second harness session, next to a shell it
			// sounds exactly like — and a hints line that names both spends its
			// scarcest row teaching a distinction most people never need. It is
			// in the help, under the key that opens it.
			hints += " · " + m.leader() + " s shell"
			if len(m.panes()) > 1 {
				hints += " · " + m.leader() + " ←/→ pane"
				hints += " · " + m.leader() + " 0-9 jump"
			}
			// Only with two columns on screen is there anything to maximize
			// over: more terminals are more tabs in the one box.
			if m.shells.len() > 0 {
				zoom := " maximize"
				if m.maximized {
					zoom = " restore"
				}
				hints += " · " + m.leader() + " " + paneZoomKey + zoom
			}
		}
		// The seize toggle only matters while something in the box has the
		// mouse; the rest of the time selection simply works.
		if m.mouseSeized {
			hints += " · " + m.paneMouseHint() + " mouse back"
		} else if p.term.MouseMode() != termpane.MouseNone {
			hints += " · " + m.paneMouseHint() + " take mouse"
		}
		return hints
	case focusFolder:
		return "←→ change folder · Enter lists them all · ↓ boxes · Tab or Esc prompt"
	case focusList:
		if m.list.visual {
			lo, hi := m.list.visualRange()
			return fmt.Sprintf("VISUAL  %s · ↑/↓ extend · Space selects · a letter acts on the range · V or Esc cancel",
				plural(hi-lo+1, "box", "boxes"))
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
		parts = append(parts, "↑ or Tab folder", "Esc prompt")
		keys := strings.Join(parts, " · ")
		if m.list.nameFull > m.list.nameWidth {
			// Only worth saying on a row that has more name than column.
			keys = "←→ read the rest of the name · " + keys
		}
		return keys
	default:
		return "Tab or ↑ discoboxes · Shift-Tab options · Alt-E editor · Ctrl-Enter newline · Ctrl-D quit"
	}
}

func (m *Model) helpText() string {
	return strings.Join([]string{
		"The window opens in the prompt, because starting something new is",
		"what you are usually here for. Everything else is one key away.",
		"",
		"───────────────────────────────────────────────────────────────",
		"In the prompt",
		"",
		"    Enter          run the prompt in a new discobox, or with an",
		"                   empty prompt just create one and attach to it",
		"    Ctrl-Enter     newline (Alt-Enter too, and Ctrl-J, which is",
		"                   what most terminals send for Ctrl-Enter)",
		"    Ctrl-D         quit, when the prompt is empty",
		"    ↑ ↓            move a line at a time, wrapped rows included.",
		"                   From the row they cannot move off they go to the",
		"                   start or the end of it. ↑ then leaves for the",
		"                   discobox list — back to the one you left the cursor",
		"                   on, or the last row, since that is the one nearest",
		"                   the prompt. Opening the window out it lands at the",
		"                   top instead: there were no rows on screen to be",
		"                   near. ↓ stops there: the prompt is the bottom of",
		"                   the window",
		"    Alt-E or F2    write the prompt in $EDITOR",
		"    Tab            round the window: the prompt, the discoboxes, the",
		"                   folder they are filtered to, and back",
		"    Shift-Tab      run options",
		"    " + HarnessesKeyName + "             the harnesses, and back",
		"",
		"───────────────────────────────────────────────────────────────",
		"In the discobox list",
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
		"  command would act on; and, in its own color, a row that is",
		"  both. A range being drawn counts as selected.",
		"    ↓ past the end returns to the prompt, and so do Tab and Esc.",
		"",
		"  A row reads: state · name · harness · where its work sits in",
		"  git · how old it is · what it is using of its cpu,",
		"  memory and disk · what it has changed.",
		"",
		"      ● running    ◐ starting    ○ stopped    ▪ archived",
		"      ✗ error — the row shows the error under the cursor",
		"",
		"  Half of what those carry is their color, so without it the",
		"  glyph gives way to the state spelled out in a column.",
		"      ↑ an upgrade is available, to the current harness image",
		"",
		"  main@a3f9c21 is the discobox's branch and commit, as its own",
		"  agent last reported them — until it reports, the commit it was",
		"  spawned from. The mark on it is the state of the work, spelled",
		"  out in the column beside it, most losable first:",
		"      *  dirty      uncommitted changes, which only the discobox",
		"                    holds — archiving it now would lose them",
		"      ⇡  ahead      committed work that no apply has landed",
		"                    anywhere yet — ahead of every host, the way a",
		"                    branch is ahead of its upstream",
		"      ✓  applied    the head commit is the last one applied back,",
		"                    so nothing here would be lost",
		"         clean      unmarked: it sits as it was cut, with nothing",
		"                    to bring back",
		"         -          its agent has not reported yet — a stopped or",
		"                    just-created discobox says nothing either way",
		"",
		"  +N −N at the row's end is what it has changed — lines added",
		"  and deleted against the commit it was cut from, pulled",
		"  upstream work not counted.",
		"",
		"    Enter  attach          s  shell",
		"    v      open it in VS Code, in a window of its own",
		"    y      apply back to this directory",
		"    u      upgrade to the current image",
		"    e      rename          t  stop",
		"    T      start           x  archive",
		"    U      unarchive       P  purge",
		"    .      every action, as a menu",
		"",
		"  rename opens the name it already has, to be edited rather than",
		"  retyped: Enter accepts it, Esc leaves it alone. A box whose",
		"  harness has titled its terminal shows that title instead, and",
		"  cannot be renamed: the harness owns the name on screen.",
		"",
		"  attach and shell open the workspace, drawn in the window",
		"  itself. apply takes the real terminal, because the list can",
		"  act on several discoboxes at once and a pane shows one.",
		"",
		"  vscode takes neither. It edits the box in place over",
		"  Remote-SSH, so the editor is another program in another",
		"  window and this one carries on: the terminal and the editor",
		"  are two views of the same box, open at once.",
		"",
		"───────────────────────────────────────────────────────────────",
		"The workspace screen",
		"",
		"  It is one discobox as the server has it: its terminals on the",
		"  left, the primary among them, and its shells on the right, one",
		"  of each visible at a time. Attaching joins them all, and a",
		"  session started from anywhere — another window, another machine —",
		"  appears on its own, on the side the server's own record puts it:",
		"  a harness terminal is a terminal, everything else is a shell.",
		"  With no shells the terminals take the whole width.",
		"",
		"  Every pane wears the number it answers to, counted across the",
		"  screen from the primary, which is always 0 and always the",
		"  leftmost tab of the left box.",
		"",
		"  Every command in the list is here too, on the key it has there,",
		"  behind the leader, acting on the discobox on screen:",
		"",
		"    " + m.leader() + " a       back to the primary terminal",
		"    " + m.leader() + " " + paneTerminalKey + "       another terminal, beside the primary — a fresh",
		"                   session of the harness this discobox runs. It is",
		"                   " + paneTerminalKey + " because that is what screen and tmux create a",
		"                   window on; t is stop, which the list has it on",
		"    " + m.leader() + " s       a new shell, in a new tab",
		"    " + m.leader() + " " + vscodeKey + "       open it in VS Code, in a window of its own",
		"    " + m.leader() + " y       apply back to this directory",
		"    " + m.leader() + " x / U   archive / unarchive",
		"    " + m.leader() + " u       upgrade      " + m.leader() + " t / T   stop / start",
		"",
		"  apply runs in the screen itself, over the workspace, for as",
		"  long as it takes — and the terminals underneath are untouched:",
		"  still connected, still running, still where you left them when",
		"  the command exits. The rest run against the server and report",
		"  on the status line, and the workspace stays up while they do.",
		"",
		"  Every other key goes to the focused pane. Which ones do not",
		"  depends on what is in it:",
		"",
		"    " + m.leader() + " " + paneDetachAlt + "       detach from the whole workspace, leaving",
		"                   every session running. The same key everywhere:",
		"                   Ctrl-C is the application's, in a harness as much",
		"                   as in a shell, because someone who types it to",
		"                   stop an agent and gets a detached session instead",
		"                   has not stopped anything and cannot tell from the",
		"                   screen. A finished pane is different: there is",
		"                   nothing left to type at, so q, Esc or Enter on its",
		"                   own dismisses it",
		"    " + m.leader() + " " + paneQuitKey + "       quit the window entirely, every session",
		"                   left running — the exit Ctrl-C is everywhere else",
		"    " + m.leader() + " ← / " + m.leader() + " →  move along the screen, terminals then shells,",
		"                   or h and l. Hold Ctrl and they keep going:",
		"                   " + m.leader() + " ^→ ^→ walks across without pressing the",
		"                   leader again",
		"    " + m.leader() + " 0-9     jump straight there: 0 is the primary,",
		"                   and the rest wear their number in the strip",
		"    " + m.leader() + " " + paneZoomKey + "       give the focused column the whole window and",
		"                   hide the other, or give the window back — the",
		"                   same toggle as the [+] / [-] button each box",
		"                   wears at the right of its top border. What is",
		"                   hidden stays connected and stays running",
		"    " + m.paneMouseHint() + "       take the mouse from a box that is using it,",
		"                   to select and copy; press again to give it back",
		"",
		"  A shell or a terminal that exits keeps its last screen as a tab",
		"  to be read; q, Esc or Enter dismisses it. Detach is the",
		"  workspace's, not a pane's: the way to be rid of a running one is",
		"  to exit it. The primary is the exception: the workspace is above",
		"  all a view onto that session, so its ending ends the screen.",
		"",
		"  A finished command is a screen to read rather than a terminal",
		"  to type at: ↑ ↓, pgup/pgdn, g and G walk through output longer",
		"  than the pane, and q closes it.",
		"",
		"  The mouse goes to the box only while something in it has",
		"  asked for one, and while it does you lose your terminal's own",
		"  click-to-select — as you would in tmux, and with the same way",
		"  round it: most terminals let Shift through to their own",
		"  selection.",
		"",
		"  Ctrl-C reaches the program in every pane, and quits the window",
		"  only when no pane is up; from inside one, " + m.leader() + " " + paneQuitKey + " is the quit.",
		"",
		"───────────────────────────────────────────────────────────────",
		"Back in the discobox list",
		"",
		"  The keys along the bottom are only the ones the discoboxes under",
		"  the cursor can take: upgrade appears when one is available,",
		"  unarchive when something is archived, purge only for archived",
		"  discoboxes. Archiving is reversible and asks nothing; purge",
		"  destroys the disk and asks first.",
		"",
		"    A      show or hide archived discoboxes",
		"    r      refresh now — the list refreshes itself anyway",
		"",
		"───────────────────────────────────────────────────────────────",
		"The folder filter",
		"",
		"  The path in the header is which folder's discoboxes are listed.",
		"  The window opens on the one it is running in, which is what",
		"  `disco ls` shows; every other folder anything was started from",
		"  is one press away, and so is showing all of them at once.",
		"",
		"    ↑              reach it, from the top of the discobox list",
		"    ← →            change it without opening anything",
		"    Enter          open the list of folders, with what is in each",
		"    ↓              back down into the discoboxes",
		"",
		"  It is why a row carries no folder column: every row on screen",
		"  has already been filtered to one, so a column would repeat the",
		"  same value all the way down.",
		"",
		"───────────────────────────────────────────────────────────────",
		"The harnesses (" + HarnessesKeyName + ")",
		"",
		"  The harnesses a discobox can be run on, and everything you do to",
		"  them. It is `disco configure`: that command opens the window",
		"  here.",
		"",
		"    ↑ ↓ / k j      move            g / G   first / last",
		"    e or Enter     enable it, or set it up again. The harness's own",
		"                   setup takes the terminal and asks its own",
		"                   questions; the window comes back when it exits",
		"    d              disable it, which deletes the secrets and files",
		"                   its setup created. It asks first, and releases",
		"                   the project default when it is that",
		"    s              make it the default, which is what a discobox",
		"                   with no harness of its own runs",
		"    v              its whole configuration: what it runs, which",
		"                   secret answers each variable it needs, and the",
		"                   files it carries",
		"    f              edit one of those files in $EDITOR",
		"    Esc or " + HarnessesKeyName + "      back to the launcher",
		"",
		"      ● enabled    ○ disabled    ✗ its setup did not finish",
		"      ★ the project default",
		"",
		"  Every harness the project has is offered on the run options,",
		"  enabled or not — one that needs no credentials is runnable",
		"  without ever being set up — and the default leads the list.",
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
		"  The source follows the folder in the header: switching folders",
		"  switches where a new discobox is cut from as well as which ones",
		"  are listed. Setting it here overrides that, which is the one",
		"  case the strip below the prompt bothers to show it in.",
		"",
		"  The strip always shows what is set, so the panel never has to",
		"  be open to know what Enter will do. The panel shows the",
		"  `disco run` command it describes, live: what the window does",
		"  is reproducible from a shell.",
		"",
		"  Press Esc to close.",
	}, "\n")
}
