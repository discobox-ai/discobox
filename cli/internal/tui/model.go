// Package tui is the `discobox tui` launcher: one window that opens with the
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
	"math"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/discobox-ai/discobox/termpane"
	"github.com/discobox-ai/x/selection"
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

	list *sandboxList
	// resources is what Discobox has on this machine and what it is using,
	// refreshed on the listing's beat and drawn in the header. Zero until the
	// first report arrives, which draws nothing rather than zeroes.
	resources Resources
	prompt    textarea.Model
	opts      *optionSet
	logo      logo

	// draft is the prompt as the store last had it. The window writes only
	// when the field has moved away from it, so an idle window writes nothing
	// and a window closed mid-sentence has the sentence. See saveDraft.
	draft string

	// edits is the composer's kill ring and undo history — the readline state
	// the textarea does not keep for itself. See readline.go.
	edits promptEditor

	// requests is the project's pending credential requests, indexed by the
	// discobox they were asked from. Read on the same poll as the listing —
	// there is no client-facing event stream (ADR 0061) and an approval is
	// answered on human time — and it is what both the row's mark and the
	// workspace's banner are drawn from. See credentials.go.
	requests map[string][]CredentialRequest

	// allRequests is every pending request, the ones no discobox owns
	// included. The secrets screen counts and answers from it; the map above
	// only exists to mark rows.
	allRequests []CredentialRequest

	// harnesses is the project's harnesses: the screen that manages them, and
	// the listing the run options' harness choices are built from. It is read
	// whether or not the screen is up. See harnesses.go.
	harnesses *harnessList

	// secrets is the project's credentials and the grants standing on them:
	// the screen that manages them, and the operator's side of the credential
	// inbox. See secrets.go.
	secrets *secretList

	// requestRows is the credential requests waiting on a person, drawn under
	// the secrets on that screen: what the project holds, and what is waiting
	// on it. onRequests is which of the two tables has the keys. See
	// requests.go.
	requestRows *requestList
	onRequests  bool

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
	// The tools open on this discobox: a strip of panes like the two columns,
	// except that only one is ever drawn and it is drawn over everything.
	// toolOpen is whether that column has the window; put away, its panes stay
	// attached and their sessions keep running. See tools.go.
	tools    column
	toolOpen bool
	// toolOpening is the tools with an attach in flight, keyed by tool id, so
	// the poll and the picker cannot open two panes onto one session.
	toolOpening map[string]bool
	// addresses is what each discobox answers to from a shell on this
	// machine, keyed by sandbox id, once the picker has asked. An entry that
	// is present and empty is a lookup still running — which is what keeps
	// reopening the picker from starting a second one — so absence, not a
	// zero value, is what means "never asked". See tools.go.
	addresses map[string]resolvedAddresses
	// copied is the address last taken off that card, so the row it came from
	// can say so. It is cleared when the card opens; see openTools.
	copied string

	// pendingRun is the run this window was opened to make — `discobox run`'s
	// own request — held until the harness listing lands. It is started from
	// there rather than from Init because the questions the window asks ahead
	// of a create (a project with no default harness, a harness that has never
	// been set up) are asked off that listing, and a run started before it
	// arrived would meet the server's refusal instead of the question.
	pendingRun *RunRequest
	// oneRun is whether the window was opened to make one discobox and show
	// it. It is what the window is for, so the window ends with it: the
	// workspace it opens on the discobox it made closes the window when it is
	// left, exactly as `discobox attach`'s does. See WithRun.
	oneRun bool

	// attach is the discobox this window was opened as an attach on, and nil
	// for the launcher proper. `discobox run` and `discobox attach` open the
	// window on that discobox's workspace rather than drawing a terminal of
	// their own, so the window is that attach: leaving the workspace —
	// detaching, or the session ending — leaves the window, because there is
	// no list behind it that anybody asked for. See WithAttach.
	attach *Sandbox
	// exitErr is what an attach that never came up ends the window with. Run
	// returns it, so the command that opened the window fails the way a plain
	// attach fails rather than reporting on a status line nobody is left to
	// read. See Model.exit.
	exitErr error

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

	// zones is where the last frame's controls landed, recorded as it was
	// drawn, so a press is a lookup rather than the layout computed a second
	// time. See zones.go and ADR 0088 §3.
	zones zones

	// primaryText is the last thing selected in the window, which is what the
	// middle button pastes — X11's primary selection, kept here because the
	// window took the terminal's over when it started reporting the mouse.
	primaryText string

	// promptCapture is a selection being dragged inside the composer, which
	// owns every event until the button comes up. The composer's selection is
	// the textarea's own rather than the chrome's: it is a field, and a
	// selection there has to be one typing replaces. See ADR 0088 §2.
	promptCapture bool
	// clicks counts a run of presses on one cell, so the second press on a row
	// can mean "open it" where the first meant "point at it". now is the clock
	// it is counted against, replaceable so a test can decide what is one
	// gesture and what is two.
	clicks         int
	clickX, clickY int
	clickAt        time.Time
	now            func() time.Time

	// hoverX and hoverY are where the pointer is resting, so a control can be
	// drawn as live before it is pressed. Off the window until it has moved
	// once: nothing is under a pointer that has not been seen.
	hoverX, hoverY int

	// tabSpans is where each visible tab label sits in the shell box's top
	// border, recorded as the strip is drawn (tabbedEdge) so a click on
	// [2 bash] can mean tab 2. Box-relative columns.
	tabSpans []tabSpan

	// zoomSpans is where each box's maximize control sits, recorded as the
	// boxes are drawn (zoomControl) so a click on [+] can mean that box.
	// Absolute screen columns.
	zoomSpans []zoomSpan

	// banner is where the workspace's attention band sits, and which band it
	// is, recorded as it is drawn so a press can be matched against the frame
	// on screen. See banner.go.
	banner bannerSpan

	// buttonSpans is where the showing tool window's [-] and [x] sit, recorded
	// as its border is drawn (toolControls). Absolute screen columns.
	buttonSpans []buttonSpan

	// leaderKey is the pane's prefix; empty takes the default. See Model.leader.
	leaderKey string

	// leaderArmed is the leader held open on one of the window's own screens,
	// where there is no termpane to own the prefix. It is set by the leader
	// and consumed by the very next key, so it can never outlive the sequence
	// that opened it.
	leaderArmed bool

	// expanded is whether the window has opened out from the prompt it starts
	// as into the full launcher. See compact.go.
	expanded bool

	// printed is whether the last frame was drawn inline — that is, printed on
	// the screen the window was started from, which the alternate screen does
	// not take with it. clearing is how many acknowledgements the window is
	// still waiting for while those rows are erased, zero for none outstanding.
	// Both are compact.go's; see clearPrinted.
	printed  bool
	clearing int

	// shimmer is the frame the opening glint is on, or zero when there is none.
	// See shimmer.go.
	shimmer int
	noise   *rand.Rand

	focus       focusArea
	optionsOpen bool
	// secretsOpen is whether the secrets screen has the window, on the same
	// terms the harnesses screen has it.
	secretsOpen bool

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
	// a dead window. An operation that can say more than "busy" replaces it as
	// it goes; see narration.go.
	busy string

	// busyGen numbers the operations that narrate themselves onto the busy
	// line, so a report from one the window has moved on from is dropped rather
	// than overwriting what replaced it.
	busyGen int

	// stopNarrating ends a narration that follows something other than a call —
	// the provisioning watch, which runs until it is told to stop.
	stopNarrating func()

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

	// welcoming is whether the introduction has the window. It is shown once
	// per project and dismissed with Enter; see welcome.go.
	welcoming bool

	// One-time server setup, reported under the window rather than waited on
	// before it opens. See initializing.go.
	initTitle   string
	initLine    string
	initUpdates <-chan string

	// copySize is the measurement behind the "copy this directory?" question:
	// the running total, the walk's stop, and the dialog the number is being
	// written into, which is what says the answer has not been given yet.
	copySize   func() DirectoryTotal
	copyStop   func()
	copyDialog *dialog
	copyDir    string
}

// Option configures the window at construction.
type Option func(*Model)

// WithLeader sets the pane's prefix key, as the keys package normalizes it.
// Empty takes the default.
func WithLeader(key string) Option {
	return func(m *Model) { m.leaderKey = key }
}

// WithHarnesses opens the window on the harnesses screen, which is what
// `discobox configure` is: the same window, opened on the screen that command is
// about. The window is opened out with it — the screen is the whole of it, and
// the opening prompt has no room for one.
func WithHarnesses() Option {
	return func(m *Model) { m.harnessesOpen, m.expanded = true, true }
}

// WithAttach opens the window on one discobox's workspace, which is what
// `discobox run` and `discobox attach` are: the window is that attach rather
// than a launcher that happens to have one open. It is opened out with it — the
// workspace is the whole of it — and leaving the workspace closes the window
// instead of falling back to a list nobody asked for. The introduction never
// stands in front of it, project unwelcomed or not — see New — because there
// is nothing behind the workspace for it to hand over to.
func WithAttach(sandbox Sandbox) Option {
	return func(m *Model) { m.attach = &sandbox }
}

// WithRun opens the window on one run: `discobox run` builds its request from
// its flags and hands it over rather than creating the discobox itself, so the
// question about uncommitted work is the window's own dialog, the wait is the
// window's own screen, and what it lands on is the workspace. The window is
// that run, so it closes when the workspace it opened is left.
//
// A project that has not been welcomed still gets the introduction here: it
// opens over the wait for the discobox this run is making, the run proceeds
// behind it exactly as everything else loads behind it, and Enter uncovers
// however far that has gotten.
func WithRun(req RunRequest) Option {
	return func(m *Model) { m.oneRun, m.pendingRun = true, &req }
}

func New(ctx context.Context, ds DataSource, options ...Option) *Model {
	color := detectColor()
	st := newStyles(color)

	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	// Emacs mode, the whole of it. See readline.go.
	ta.KeyMap = promptKeyMap()
	// The field is as tall as what is in it, wrapped rows counted, and stops
	// growing at promptMaxRows — past that it scrolls under the cursor.
	ta.DynamicHeight = true
	ta.MinHeight = 1
	ta.MaxHeight = promptMaxRows
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
		ctx:         ctx,
		ds:          ds,
		st:          st,
		list:        newSandboxList(session),
		harnesses:   newHarnessList(),
		secrets:     newSecretList(),
		requestRows: newRequestList(),
		prompt:      ta,
		logo:        newLogo(color),
		focus:       focusPrompt,
		session:     session,
		exec:        tea.Exec,
		copyOS:      func(text string) error { return osClipboard(ctx, text) },
		chromeGrid:  &frameGrid{},
		noise:       newNoise(),
		now:         time.Now,
		hoverX:      -1,
		hoverY:      -1,
	}
	m.chromeSel = selection.New(m.chromeGrid)
	// The label above the field already says what an empty prompt does, and the
	// placeholder is gone the moment you type.
	m.prompt.Placeholder = m.placeholder()
	for _, option := range options {
		option(m)
	}
	if m.attach != nil {
		// `discobox attach` opens directly on the discobox it names, and
		// dismissing the introduction here would only uncover that exact
		// workspace again — there is nothing behind it the introduction is
		// standing in front of. `discobox run` is left welcoming: it opens on
		// a discobox that does not exist yet, and the introduction stands in
		// front of the wait for it exactly as it stands in front of the list
		// on every other first run. See WithRun.
		m.welcoming = false
	}
	m.opts = newOptions(session)
	return m
}

// oneShot reports whether this window is one command's own — `discobox run` or
// `discobox attach` — rather than the launcher. Such a window opens on the
// discobox it is about, never shows the prompt or the list, and goes when the
// workspace does.
func (m *Model) oneShot() bool { return m.oneRun || m.attach != nil }

// Init loads what the window is drawn from. The session comes first because the
// header and the filter are read from it; the listing and the harnesses follow
// on their own, so a slow server delays the rows rather than the window. The
// harnesses are read here rather than when their screen is opened because the
// run options offer them as the harness to run.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{textarea.Blink, m.loadSession(), m.refresh(), m.loadResources(), m.loadCredentialRequests(), m.loadHarnesses(), m.tick(), m.startShimmer(), m.awaitInitialization()}
	// An attach opens on its workspace rather than on the prompt, from the row
	// the command already has: waiting for the listing to come back would hold
	// the attach behind a request it does not need. The listing is still read,
	// because the header and the workspace's keys take the discobox off it as
	// it moves (currentBox).
	//
	// Until the terminal is up the window says what it is waiting for, on the
	// same screen a run's wait uses. A window that is one command's own has no
	// list to sit on while it waits, and the list of everything else is not
	// what somebody who named one discobox is looking at.
	if m.attach != nil {
		m.dialog = m.waitDialog("Opening "+sandboxLabel(*m.attach), "attaching")
		cmds = append(cmds, m.openWorkspace(*m.attach, false))
	}
	// A run is not started here — it waits for the harnesses, see pendingRun —
	// but the window says what it is for from the first frame rather than
	// showing the list of everything else while the run it was opened for gets
	// going.
	if m.oneRun {
		m.dialog = m.waitDialog(creatingTitle, "starting")
	}
	return tea.Batch(cmds...)
}

// exit ends an attached window, carrying whatever ended it back to the command
// that opened it. Nothing is reported on screen: the window is going, and the
// command it belongs to is what reports.
func (m *Model) exit(err error) tea.Cmd {
	m.exitErr = err
	return m.closeWindow()
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

// loadResources rides the same beat as refresh but is its own command, so a
// machine readout that fails or is slow costs the listing nothing.
func (m *Model) loadResources() tea.Cmd {
	return func() tea.Msg {
		resources, err := m.ds.Resources(m.ctx)
		if err != nil {
			// Silent by design. This is ambient, and a machine that cannot be
			// measured should draw nothing rather than push an error at
			// somebody who did not ask a question.
			return resourcesLoadedMsg{}
		}
		return resourcesLoadedMsg{resources: resources}
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

type resourcesLoadedMsg struct {
	resources Resources
}

type tickMsg struct{}

type statusMsg struct {
	text string
	err  bool
}

// workspaceCheckedMsg answers --include-dirty=auto: what the source directory
// would carry into the discobox, and so what there is to ask about.
type workspaceCheckedMsg struct {
	req       RunRequest
	workspace SourceWorkspace
	err       error
}

// directorySizeMsg is the copy question's measurement coming due for another
// read.
type directorySizeMsg struct{}

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

// runActionMsg carries a choice out of the action menu. The menu cannot run the
// action itself: a dialog closes over the model by value, so it emits the
// choice and the update loop runs it against the live model.
type runActionMsg struct {
	key     string
	targets []Sandbox
}

// applyFinishedChoiceMsg carries the cleanup choice out of the successful
// apply dialog so it runs against the live model after the dialog closes.
type applyFinishedChoiceMsg struct{ choice string }

// statusHolds is how long a message sits there when nothing else is pressed.
const statusHolds = 4 * time.Second

type statusExpiredMsg struct{ generation int }

func status(format string, args ...any) tea.Cmd {
	return func() tea.Msg { return statusMsg{text: fmt.Sprintf(format, args...)} }
}

// ---------------------------------------------------------------------------
// update

// Update is the window's one entry point for a message. Everything it does is
// in update below; what is here is the one thing that has to happen around
// every message rather than in any handler — the opening prompt being wiped off
// the screen it was printed on, the first time the window takes the whole
// terminal. See clearPrinted.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// The erasing frame is acknowledged by the terminal, and given up on by the
	// backstop under it. Neither answer is anything the window itself is asked
	// about, so neither reaches update below. See clearPrinted.
	switch msg.(type) {
	case screenClearedMsg:
		m.clearing, m.printed = 0, false
		return m, nil
	case tea.CursorPositionMsg:
		if m.clearing > 0 {
			return m, m.screenCleared()
		}
	}
	cmd := m.clearPrinted(m.update(msg))
	// Whatever just happened may have changed which pane is on screen, and a
	// pane on screen is one being read. Doing it here rather than at each of
	// the several places focus can move means no new way to move it can forget.
	m.markSeen()
	return m, cmd
}

func (m *Model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case initializationMsg:
		return m.applyInitialization(msg)

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		// Columns mean nothing across a resize; see the panes' own clearing.
		m.chromeSel.Clear()
		m.layout()
		return nil

	case sessionLoadedMsg:
		if msg.err != nil {
			return m.report(true, "cannot read the session: %v", msg.err)
		}
		m.session = msg.session
		m.list.session = msg.session
		cmd := m.restoreDraft(msg.session.Draft)
		// The window opens on the folder it was opened in, which is what
		// `discobox ls` shows and what the header has always said. Everything
		// else is one press away in the dropdown.
		m.list.folder = msg.session.Directory
		m.opts = newOptions(msg.session)
		m.opts.setFolder(m.list.folder)
		// The two loads race, and either order has to end with the panel
		// offering the harnesses that are actually there.
		m.opts.setHarnesses(m.harnesses.all)
		return cmd

	case resourcesLoadedMsg:
		m.resources = msg.resources
		m.list.resources = msg.resources
		return nil

	case listLoadedMsg:
		if msg.err != nil {
			return m.report(true, "cannot list discoboxes: %v", msg.err)
		}
		m.list.setAll(msg.sandboxes)
		// The rows are replaced wholesale, so the marks are stamped back onto
		// them: the two reads land independently and either can be the later.
		m.list.setPending(m.requests)
		// What the project has been cut from is read off the same listing the
		// folder dropdown is, for the same reason: the sources worth offering
		// are exactly the ones something is already sitting in.
		m.opts.setSources(m.list.sources())
		m.layout()
		return nil

	case secretsLoadedListMsg:
		return m.secretsLoaded(msg)

	case secretActionMsg:
		return m.secretActed(msg)

	case credentialsLoadedMsg:
		if msg.err != nil {
			// Reported quietly: the inbox is polled, and a window that shouts
			// every five seconds about a server it cannot reach is a window
			// you stop reading.
			return nil
		}
		m.setCredentialRequests(msg.requests)
		return nil

	case secretsLoadedMsg:
		if msg.err != nil {
			m.dialog = errorDialog("Credential request", fmt.Sprintf("Cannot read the project's secrets: %v\n\nThe request is still waiting.", msg.err))
			return nil
		}
		req, ok := m.credentialRequestByID(msg.requestID)
		if !ok {
			// Answered elsewhere while the secrets were being read.
			m.dialog = nil
			return m.report(false, "that request is no longer waiting")
		}
		return m.askAboutCredential(req, msg.secrets)

	case credentialAnsweredMsg:
		return m.credentialAnswered(msg)

	case tickMsg:
		cmds := []tea.Cmd{m.refresh(), m.loadResources(), m.loadCredentialRequests(), m.tick()}
		// The draft is written on the same clock the listing is read on, so a
		// window killed outright — a closed terminal, a lost ssh session —
		// loses at most the last few seconds of what was typed. The keys that
		// close the window save it themselves; see closeWindow.
		if cmd := m.saveDraft(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		// Harnesses change when somebody changes them, which is here — so they are
		// re-read after every action rather than on a clock. The exception is
		// the screen itself, where a listing going stale under the cursor is
		// exactly what a stale listing costs.
		if m.harnessesOpen {
			cmds = append(cmds, m.loadHarnesses())
		}
		return tea.Batch(cmds...)

	case narrationMsg:
		return m.narrated(msg)

	case statusMsg:
		// A message is an answer to the last key, so it goes when the next
		// one is pressed — and if none is, it still goes: a line that stays
		// green all afternoon stops meaning "just happened".
		m.status, m.statusE = msg.text, msg.err
		m.statusGen++
		expires := m.statusGen
		return tea.Tick(statusHolds, func(time.Time) tea.Msg {
			return statusExpiredMsg{generation: expires}
		})

	case statusExpiredMsg:
		if msg.generation == m.statusGen {
			m.status, m.statusE = "", false
		}
		return nil

	case workspaceCheckedMsg:
		return m.workspaceChecked(msg)

	case directorySizeMsg:
		return m.directorySized()

	case createMsg:
		return m.create(msg.req)

	case createdMsg:
		return m.created(msg)

	case provisioningDoneMsg:
		// The attach can finish, so the report on it goes and what is
		// underneath — the pane it was covering — comes forward.
		if m.dialog != nil && m.dialog.kind == dlgStatus {
			m.dialog = nil
		}
		return nil

	case runVerbMsg:
		return m.runVerb(msg.verb, msg.ids)

	case verbDoneMsg:
		return m.verbDone(msg)

	case renameMsg:
		return m.rename(msg.id, msg.name)

	case renameDoneMsg:
		return m.renameDone(msg)

	case editorOpenedMsg:
		return m.editorOpened(msg)

	case folderChosenMsg:
		return m.selectFolder(msg.folder)

	case sourceChosenMsg:
		if msg.enter {
			m.askForSource(m.opts.typedSource(), "", msg.run)
			return nil
		}
		if msg.typed {
			return m.resolveSource(msg)
		}
		m.opts.chooseSource(msg.source)
		return m.sourceApplied(msg.run)

	case sourceResolvedMsg:
		if msg.err != nil {
			// Back into the field, holding what was typed and saying what is
			// wrong with it: a path off by one character is corrected there,
			// where a dismissed dialog would mean typing the whole of it again.
			m.askForSource(msg.typed, msg.err.Error(), msg.run)
			return nil
		}
		m.opts.chooseSource(msg.source)
		return m.sourceApplied(msg.run)

	case paneOpenedMsg:
		return m.paneOpened(msg)

	case configurePaneOpenedMsg:
		return m.configurePaneOpened(msg)

	case workspaceExecsMsg:
		return m.workspaceExecs(msg)

	case workspaceTermMsg:
		return m.workspaceTermOpened(msg)

	case toolTermMsg:
		return m.toolOpened(msg)

	case runToolMsg:
		return m.runTool(msg.id)

	case toolFilesMsg:
		return m.openToolFiles(msg.id)

	case toolFileMsg:
		return m.editToolFile(msg.file)

	case toolFileDoneMsg:
		return m.toolFileEdited(msg)

	case addressesMsg:
		return m.addressesResolved(msg)

	case copyAddressMsg:
		return m.addressCopied(msg)

	case workspaceTickMsg:
		if msg.gen != m.wsGen {
			return nil
		}
		return tea.Batch(m.listExecs(msg.gen), m.listServices(msg.gen), m.workspaceTick(msg.gen))

	case workspaceServicesMsg:
		return m.workspaceServices(msg)

	case serviceTermMsg:
		return m.serviceTermOpened(msg)

	case workspaceForwardMsg:
		return m.workspaceForward(msg)

	case workspaceForwardChangedMsg:
		// Nothing to apply: the header reads the forward itself. This is the
		// redraw, and re-arming the wait is what keeps it coming.
		if msg.gen != m.wsGen || m.forward == nil {
			return nil
		}
		return m.forwardEvents(msg.gen, m.forward)

	case paneMsg:
		// Addressed to the pane it came from, which may exist before the
		// screen does: a tab can connect while the primary is still opening.
		return m.updatePaneMsg(msg)

	case runActionMsg:
		return m.actOn(msg.key, msg.targets)

	case applyFinishedChoiceMsg:
		return m.finishSuccessfulApply(msg.choice)

	case harnessesLoadedMsg:
		return m.harnessesLoaded(msg)

	case harnessSetupMsg:
		if msg.andDefault {
			return m.configureHarnessThen(msg.harness, &msg.harness, msg.resume)
		}
		return m.configureHarnessThen(msg.harness, nil, msg.resume)

	case harnessDefaultMsg:
		return m.chooseDefaultHarness(msg)

	case resumeRunMsg:
		return m.startRun(msg.req)

	case harnessVerbMsg:
		return m.runHarnessVerb(msg.verb, msg.harness, nil)

	case harnessFileMsg:
		return m.editHarnessFile(msg.harness, msg.path)

	case harnessDoneMsg:
		return m.harnessDone(msg)

	case servicesMsg:
		return m.showServices(msg)

	case serviceMenuMsg:
		return m.showServiceMenu(msg.service)

	case serviceVerbMsg:
		return m.runServiceVerb(msg.verb, msg.service)

	case serviceDoneMsg:
		return m.serviceDone(msg)

	case harnessCardMsg:
		m.busy = ""
		if msg.err != nil {
			return m.report(true, "cannot read the harness: %v", msg.err)
		}
		m.dialog = m.readableDialog(msg.title, msg.body)
		return nil

	case editorDoneMsg:
		m.promptEdited(msg)
		return nil

	case shimmerTickMsg:
		return m.advanceShimmer(msg)

	case tea.PasteMsg:
		return m.updatePaste(msg)

	case tea.MouseMsg:
		// The layout the press was aimed at is the one the last frame left
		// behind, and the composer's height can change under a click, so
		// geometry is recomputed after one exactly as it is after a key.
		cmd := m.updateMouse(msg)
		m.layout()
		return cmd

	case tea.KeyPressMsg:
		// Ctrl-L repaints, and is not consumed doing it. The redraw here is
		// the window's own — Bubble Tea throws its picture of the screen away
		// and draws every cell again, which is what clears clutter written
		// over the window by something that was not the window. It cannot
		// clear what a pane is showing, because that is drawn from the
		// emulator's grid and would be drawn from it again identically; only
		// the program on the far end can redraw that, so the key goes on to
		// whatever is focused as well. In a pane the two repaints are one
		// press: the window's, and the box's.
		var repaint tea.Cmd
		if keyName(msg) == repaintKey {
			repaint = tea.ClearScreen
		}
		cmd := m.updateKey(msg)
		// The composer's height is a function of its contents, so geometry is
		// recomputed after every key rather than only on a resize.
		m.layout()
		return tea.Batch(repaint, cmd)
	}

	// Anything the window itself does not handle goes to whatever is focused.
	// For a pane that is most of what it needs: the terminal's output arrives
	// as the pane library's own messages, which nothing here can name, and
	// holding any of them back stops the pane dead.
	if m.inPanes() {
		return m.updatePane(msg)
	}
	var cmd tea.Cmd
	m.prompt, cmd = m.prompt.Update(msg)
	return cmd
}

// updatePaste routes pasted text the way updateKey routes a key press.
//
// A terminal reports a bracketed paste as a message of its own, not as the keys
// it would have taken to type it, so it reaches nothing that only handles key
// presses. It goes where typing would go: to the modal in front of everything
// else, then to the pane that owns every key while one is focused, and
// otherwise to the composer. On a screen drawn in place of the composer —
// the introduction, the secrets and harnesses lists, the options — there is
// nothing on screen a paste could land in, and text pushed into a composer
// nobody can see is worse than a paste that does nothing.
func (m *Model) updatePaste(msg tea.PasteMsg) tea.Cmd {
	switch {
	case m.dialog != nil:
		return m.dialog.paste(msg)
	case m.welcoming, m.optionsOpen:
		return nil
	// Before the two screens, not after: a pane opened from one of them is
	// drawn over it and owns every key, paste included. See updateKey.
	case m.inPanes():
		return m.updatePane(msg)
	case m.harnessesOpen, m.secretsOpen:
		return nil
	}
	before := m.promptState()
	var cmd tea.Cmd
	m.prompt, cmd = m.prompt.Update(msg)
	// A paste into the composer is one change, and usually a large one, so
	// Ctrl-_ takes the whole of it back. See readline.go.
	if m.prompt.Value() != before.value {
		m.pushUndo(before, false)
	}
	return cmd
}

// repaintKey redraws the window. It is Ctrl-L because that is the repaint on
// every terminal there has been — readline's, vi's, screen's, tmux's — and a
// window drawn over by something outside it is exactly the moment a person
// reaches for it without thinking.
const repaintKey = "ctrl+l"

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

	// A copy chord over a selection the window is drawing is that copy,
	// wherever the selection was made: the window took the terminal's own
	// selection when it started reporting the mouse, and its chords with it.
	if cmd, taken := m.copyChord(msg); taken {
		return cmd
	}

	// Ctrl-C quits the window, but never from inside a pane: there it belongs
	// to whatever is running, and a key that sometimes interrupts an agent and
	// sometimes closes the window it is running in is a key nobody can press
	// with any confidence. See detachHint.
	if keyName(msg) == "ctrl+c" && (m.focus != focusPane || m.dialog != nil) {
		return m.closeWindow()
	}

	// The introduction stands in front of everything, including whatever screen
	// the window was opened on, so it takes the keys first. See welcome.go.
	if m.welcoming {
		return m.updateWelcome(msg)
	}
	if m.dialog != nil {
		// The dialog that was answered is the one taken down. An action may
		// have put the next question up on its way out — a new secret asks for
		// a name, then a host, then the value, and an approval reports what it
		// is doing while it does it — and nilling the field afterwards would
		// wipe it, leaving a chain that answers its first question and then
		// silently goes back to the list.
		answered := m.dialog
		cmd, closed := m.dialog.update(msg)
		if closed && m.dialog == answered {
			m.dialog = nil
		}
		return cmd
	}
	// The leader is the window's own prefix outside a pane, where there is no
	// termpane to own one. It exists so the way out is the same key sequence
	// on every screen: the workspace already quits on leader-q, and a window
	// that quits differently depending on which screen you are looking at is
	// one you have to think about before leaving.
	//
	// Not in the prompt. Ctrl-A is the composer's own "start of line", and
	// taking an editing key away to save a quit that Ctrl-C already does there
	// is the wrong trade. Ctrl-C still quits on every one of these screens; it
	// is simply no longer what the window advertises.
	if m.leaderArmed {
		m.leaderArmed = false
		if keyName(msg) == paneQuitKey {
			return m.closeWindow()
		}
		// A mistyped leader costs nothing: the key it preceded is handled as
		// though the leader had never been pressed, the way it is in a pane.
	} else if m.focus != focusPrompt && !m.inPanes() && keyName(msg) == m.leader() {
		m.leaderArmed = true
		return nil
	}
	if keyName(msg) == "f1" {
		m.dialog = m.helpDialog()
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
	//
	// These are answered before every screen's own keys and before the run
	// options panel, because they are the way *between* the screens as much as
	// the way in: the header offers F3 and F4 from everywhere and the panel's
	// harness row names F3 in its hint, and a surface that swallowed the key
	// to its neighbor would make those offers a lie for as long as it was up —
	// which is exactly where somebody reads them. The screens are peers, so
	// opening one closes the other and Esc from either goes back to the
	// launcher, rather than to a screen left open underneath.
	if keyName(msg) == harnessesKey && !m.inPanes() {
		if m.harnessesOpen {
			m.closeHarnesses()
			return nil
		}
		return m.openHarnesses()
	}
	// The secrets screen, on the next function key along and on the same terms.
	if keyName(msg) == secretsKey && !m.inPanes() {
		if m.secretsOpen {
			m.closeSecrets()
			return nil
		}
		return m.openSecrets()
	}
	// The run options on the same terms again: one key that opens the panel and
	// closes it, from every screen the window has of its own.
	//
	// It is the third peer of the two screens rather than a layer over one, so
	// opening it puts them away the same way openHarnesses and openSecrets put
	// it away. A screen left standing underneath would take its own key back
	// while nothing could see it: F3 over the panel would reach the harnesses
	// dispatch above, close a screen that was already covered, and leave the
	// frame exactly as it was — one press of the key the panel's own harness
	// row advertises, doing nothing.
	if keyName(msg) == optionsKey && !m.inPanes() {
		m.optionsOpen = !m.optionsOpen
		if m.optionsOpen {
			m.harnessesOpen, m.secretsOpen = false, false
		}
		m.layout()
		return nil
	}
	if m.optionsOpen {
		return m.updateOptions(msg)
	}
	// Neither screen while a pane is on screen. They are drawn *under* a pane,
	// never over one — View reaches `case m.inPanes()` first, and the mouse
	// leaves them out of modalUp for the same reason — so a key taken here is
	// one the program in the pane could never receive. That is not a corner
	// case: both of these screens open panes of their own and stay open behind
	// them, so the configure terminal a harness opens sat there reading nothing
	// while Enter went on meaning "reconfigure the highlighted harness" to the
	// list underneath, restarting the flow the user was looking at.
	if !m.inPanes() {
		if m.harnessesOpen {
			return m.updateHarnesses(msg)
		}
		if m.secretsOpen {
			return m.updateSecrets(msg)
		}
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
	// The editor's whole round trip is one Ctrl-_: an editor left empty is how
	// you throw a prompt away, and throwing one away by accident should be
	// takeable back.
	m.pushUndo(m.promptState(), false)
	m.prompt.SetValue(strings.TrimRight(string(edited), "\n"))
	m.promptEnd()
	m.layout()
	m.status, m.statusE = "", false
}

// updatePrompt handles the default mode. Every key is text, except the ones
// that leave or act on the composer: Up walks out of the top of the field,
// Enter launches, modified Enter inserts a newline, Tab moves to the list, and
// Shift-Tab cycles the harness.
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
		// A prompt with something in it is one you are still writing, and
		// holding Up to walk back through it should stop at the top rather
		// than throw the window open behind what you typed. Tab is the way
		// out; it is a key you press once and mean.
		if m.prompt.Value() != "" {
			return status("Tab for the discoboxes")
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
		// both directions.
		m.leavePrompt(landFirst)
		return nil

	case "shift+tab":
		m.opts.opts[optHarness].cycle(1)
		return nil

	case "alt+e", "f2":
		// Alt-E, and F2 for the terminals where Option is not Meta.
		return editPrompt(m.ctx, m.prompt.Value())

	case "enter":
		return m.run()

	case "shift+enter", "alt+enter", "ctrl+enter", "ctrl+j":
		// A terminal that can tell Ctrl-Enter apart sends ctrl+enter; most
		// send it as ctrl+j, which is the same byte as Ctrl-J. Terminals with
		// key disambiguation report Shift-Enter directly.
		m.prompt.InsertString("\n")
		return nil

	case "ctrl+d":
		// EOF on an empty line quits, the way a shell does. With text in the
		// buffer it is the shell's other meaning, delete forward, which the
		// textarea already implements — so it is passed through.
		if strings.TrimSpace(m.prompt.Value()) == "" {
			return m.closeWindow()
		}

	case "esc":
		return nil
	}

	return m.promptKey(msg)
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
// is in discobox-review's file lists, because there is no text to type.
func (m *Model) updateList(msg tea.KeyPressMsg) tea.Cmd {
	switch keyName(msg) {
	case credentialsKey:
		// The marked row, answered from where you saw the mark. The request
		// belongs to one discobox, so this takes the row under the cursor and
		// never the selection: approving a credential for several boxes at
		// once is not a thing anyone means to do.
		if s := m.list.current(); s != nil {
			return m.openCredentialDialog(s.ID)
		}
		return nil
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
	case "shift+tab":
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
		m.dialog = m.helpDialog()
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

// followSource points the list at the folder the chosen source files its
// discoboxes under, so what is listed is what the next Enter will join. The
// header and the Source row are one control in both directions: the header
// moves the source (selectFolder), and the source moves the header here.
func (m *Model) followSource() tea.Cmd {
	folder := m.opts.followSource()
	if folder != m.list.folder {
		m.list.folder = folder
		m.list.resetCursor()
		m.layout()
	}
	return status("cutting from %s", m.opts.opts[optSource].display())
}

// updateOptions handles the run options panel.
func (m *Model) updateOptions(msg tea.KeyPressMsg) tea.Cmd {
	opt := m.opts.current()
	switch keyName(msg) {
	case "esc", "tab", "shift+tab":
		m.optionsOpen = false
		return nil
	case "up", "k":
		m.opts.move(-1)
	case "down", "j":
		m.opts.move(1)
	case "left", "h":
		if m.opts.cursor == optSource {
			m.opts.cycleSource(-1)
			return m.followSource()
		}
		opt.cycle(-1)
	case "right", "l", " ":
		if m.opts.cursor == optSource {
			m.opts.cycleSource(1)
			return m.followSource()
		}
		opt.cycle(1)
	case "enter":
		if m.opts.cursor == optSource {
			// The whole list, the way the header's folder filter opens its
			// own: left and right are for the two or three you switch between,
			// and everything else — including a path the listing has never
			// seen — is behind Enter.
			m.dialog = m.opts.sourceDialog(false)
			return nil
		}
		switch opt.kind {
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
	anyUpgrade, anyRepair, anyRunning, anyStopped := false, false, false, false
	anyArchived, allArchived := false, len(targets) > 0
	for _, s := range targets {
		anyUpgrade = anyUpgrade || s.Upgrade
		anyRepair = anyRepair || s.repairable()
		anyRunning = anyRunning || s.State == StateRunning
		anyStopped = anyStopped || s.State == StateStopped
		anyArchived = anyArchived || s.State == StateArchived
		allArchived = allArchived && s.State == StateArchived
	}
	// Repair rebuilds; it is not something to reach for on a box that is
	// working, so the reason says which of the two it is looking at.
	repairWhy := "nothing is wrong with it — repair rebuilds a box that is broken or was never built"
	if !anyRepair && anyArchived {
		repairWhy = "an archived box is unarchived, not repaired"
	}
	attachable := one && targets[0].attachable()
	// Apply is drawn in a pane, which shows one discobox, and it runs git in
	// the sandbox — so an archived one, which has no container to run it in,
	// cannot take it however much it changed. A diffstat that has not come back
	// yet is not the same answer as "nothing changed", so it stays available
	// until it has.
	applyable, applyWhy := one, "takes exactly one box"
	if one {
		switch box := targets[0]; {
		case box.State == StateArchived:
			applyable, applyWhy = false, "an archived box has no working tree to look at"
		case !box.hasDiff() && box.Diff.Known:
			applyable, applyWhy = false, "nothing has changed yet"
		}
	}
	// A row named by its terminal's title is not showing the configured name,
	// which is the one rename edits: accepting a rename there would change
	// nothing on screen.
	renameable, renameWhy := one, "takes exactly one box"
	if one && targets[0].nameIsTitle() {
		renameable = false
		renameWhy = "the name shown is the terminal's title, which the harness sets — a rename would not change it"
	}
	return []action{
		{key: "a", press: "a", label: "attach", detail: "join the harness terminal", enabled: attachable,
			why: attachWhy(one, targets)},
		{key: "s", press: "s", label: "shell", detail: "open a shell in the box", enabled: attachable,
			why: attachWhy(one, targets)},
		{key: applyKey, press: applyKey, label: "apply", detail: "bring the changes back to " + m.session.Directory, enabled: applyable,
			why: applyWhy},
		{key: vscodeKey, press: vscodeKey, label: "vscode", detail: "open the box in VS Code, in a window of its own", enabled: attachable,
			why: attachWhy(one, targets)},
		{key: renameKey, press: renameKey, label: "rename", detail: "type a new name for the box", enabled: renameable,
			why: renameWhy},
		{key: "u", press: "u", label: "upgrade", detail: "re-pin to the current harness image", enabled: anyUpgrade,
			why: "already on the current image"},
		{key: repairKey, press: repairKey, label: "repair", detail: "rebuild a broken box on the current image, keeping its work", enabled: anyRepair,
			why: repairWhy},
		{key: "t", press: "t", label: "stop", detail: "power the box off, keeping its disk", enabled: anyRunning,
			why: "not running"},
		{key: "T", press: "T", label: "start", detail: "power a stopped box back on", enabled: anyStopped,
			why: "already on, or archived"},
		{key: "x", press: "x", label: "archive", detail: "put it away, disk and all, reversibly", enabled: !allArchived,
			why: "already archived"},
		{key: "U", press: "U", label: "unarchive", detail: "bring an archived box back", enabled: anyArchived,
			why: "not archived"},
		{key: "P", press: "P", label: "purge", detail: "destroy it and its data, for good", enabled: allArchived,
			why: "archive it first — purge only takes archived boxes"},
	}
}

// attachWhy names what is missing, which for the two ways a box can have no
// container is not the same thing. Archived is reversible by intent and says
// so. Anything else with nothing to attach to is a create or rebuild that never
// produced a container, and a settled failure is never retried on its own
// (ADR 0017 §4) — repair is the intent that rebuilds it (ADR 0035).
//
// The state name is not the reason. "the box is error" described the row rather
// than the obstacle, and it was wrong as often as not: the boxes it refused
// mostly had a container and a stale error latched over it.
func attachWhy(one bool, targets []Sandbox) string {
	if !one {
		return "takes exactly one box"
	}
	box := targets[0]
	if box.attachable() {
		return ""
	}
	if box.State == StateArchived {
		return "the box is archived — unarchive it first"
	}
	return "the box has no container; repair rebuilds it"
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
	// visual selection does in discobox-review.
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
		// Every interaction is drawn in the window, and a pane shows one
		// discobox. The enabled check has already said so with the reason; this
		// is the menu's own path, where the key was not offered.
		if len(targets) != 1 {
			return status("%s takes exactly one box", action)
		}
		box := targets[0]
		switch action {
		case InteractAttach:
			// On the workspace screen the terminal is already there — back to
			// the primary, which is what attach means there: it is the session
			// the workspace is a view onto. From the list it is the screen to
			// open.
			if m.inPanes() {
				// By pointer rather than by number: the primary is 0 in the
				// digits, but the services sit ahead of it in the strip and a
				// jump by position would land on one of those.
				if primary := m.primary(); primary != nil {
					m.focusPane(primary)
				}
				return nil
			}
			return m.openFromList(action, box)
		case InteractShell:
			if m.inPanes() {
				return m.newShell()
			}
			return m.openFromList(action, box)
		default:
			// A command that runs and finishes takes the screen, over the
			// workspace when there is one and over the list when there is not.
			return m.openOverlay(action, box)
		}
	}

	verb, ok := verbs[key]
	if !ok {
		return nil
	}
	if verb == VerbPurge {
		// Archiving is reversible and needs no ceremony. Purging is not.
		question := fmt.Sprintf("Purge %s? The disk and everything on it goes, and unarchive cannot bring it back.",
			actionTitle(targets))
		d := confirmDialog("Purge", question, func(string) tea.Cmd {
			return func() tea.Msg { return runVerbMsg{verb: VerbPurge, ids: ids} }
		})
		// Enter means no: a destruction nobody can undo has to be typed for,
		// not fallen into by answering the dialog the way every other one is.
		d.defaultNo = true
		m.dialog = d
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

// repairKey is the letter repair answers to. It is R rather than r, which
// refreshes the list — and the shift is no loss on an action that rebuilds a
// container, next to one that redraws a table.
const repairKey = "R"

var verbs = map[string]Verb{
	"u":       VerbUpgrade,
	repairKey: VerbRepair,
	"t":       VerbStop,
	"T":       VerbStart,
	"x":       VerbArchive,
	"U":       VerbUnarchive,
	"P":       VerbPurge,
}

var interactions = map[string]Interaction{
	"a":      InteractAttach,
	"s":      InteractShell,
	applyKey: InteractApply,
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

// ---------------------------------------------------------------------------
// run

// run is Enter in the prompt: the whole point of the window.
//
// An empty prompt is not an error. It means the other thing you come here for:
// a sandbox of your own to work in, with no harness given anything to do.
func (m *Model) run() tea.Cmd {
	if m.askWhereToCutFrom() {
		return nil
	}
	return m.runRequest(m.opts.request(m.prompt.Value()))
}

// askWhereToCutFrom stops a create the window has no folder for and asks what
// it should be cut from. It reports whether the run was stopped.
//
// "All folders" is the one filter that is not a place. Every other choice in
// the header names the directory a new discobox is cut from, and this one names
// them all — so the source fell back to whatever directory the window happens
// to be running in, and Enter cut from somewhere nobody had named. The question
// is the Source row's own list (sourceDialog), so the answer is a choice the
// panel already offers, and answering it moves the header onto that folder: the
// next Enter has a place to cut from and asks nothing.
//
// A window whose session has not landed yet is on no folder for a different
// reason — nothing has told it which — and has no directory or sources to
// offer. It is left to `discobox run`'s own resolution, the way a prompt
// submitted before the harnesses land is.
func (m *Model) askWhereToCutFrom() bool {
	if m.opts.folder != "" || m.session.Directory == "" {
		return false
	}
	m.dialog = m.opts.sourceDialog(true)
	return true
}

// askForSource opens the field a source is typed into. problem is what was
// wrong with the last thing typed, which the field opens holding: a path that
// is not there is corrected in place rather than retyped from memory.
func (m *Model) askForSource(value, problem string, run bool) {
	body := sourceHint
	if problem != "" {
		body = problem
	}
	d := inputDialog("Source", body, m.opts.sourceLabel(), value,
		func(v string) tea.Cmd {
			return func() tea.Msg {
				return sourceChosenMsg{source: strings.TrimSpace(v), typed: true, run: run}
			}
		})
	d.err = problem != ""
	m.dialog = d
}

// resolveSource checks a source typed by hand before the window takes it. The
// list's own choices are places the project has already been cut from and need
// no checking; a path typed into the field is vouched for by nothing, and the
// create is the wrong place to find out — it fails on it seconds later with the
// field gone.
func (m *Model) resolveSource(msg sourceChosenMsg) tea.Cmd {
	if msg.source == "" {
		// Nothing typed is the folder the field offered as its placeholder,
		// which is where the window was cutting from already.
		m.opts.chooseSource("")
		return m.sourceApplied(msg.run)
	}
	return func() tea.Msg {
		resolved, err := m.ds.ResolveSource(m.ctx, msg.source)
		return sourceResolvedMsg{typed: msg.source, source: resolved, run: msg.run, err: err}
	}
}

// sourceResolvedMsg is what the check came back with: the source as the create
// path will take it, or what is wrong with what was typed.
type sourceResolvedMsg struct {
	// typed is what was in the field, which a refusal puts back into it.
	typed  string
	source string
	run    bool
	err    error
}

// sourceApplied is what happens once a source is settled: the list follows it,
// and an answer given to a create's own question carries that create on.
func (m *Model) sourceApplied(run bool) tea.Cmd {
	cmd := m.followSource()
	if !run {
		return cmd
	}
	// The request is built again rather than resumed, because the panel is what
	// a run is made of and the answer has just changed it. The follow's status
	// line goes with it: the create says what it is doing from here.
	return m.runRequest(m.opts.request(m.prompt.Value()))
}

// runRequest is a run past the questions about the harness it lands on, from
// wherever the request came: the composer, or the command the window was opened
// by (WithRun). Both take the same path, which is the point of handing the
// command's request to the window rather than having it create for itself.
func (m *Model) runRequest(req RunRequest) tea.Cmd {
	if cmd, stop := m.askForADefaultHarness(req); stop {
		return cmd
	}
	if cmd, stop := m.askToSetUpHarness(req); stop {
		return cmd
	}
	return m.startRun(req)
}

// startPendingRun starts the run the window was opened on, once there is a
// harness listing to ask its questions against. It is a no-op for every window
// that was not opened on one, and for the second listing of one that was.
func (m *Model) startPendingRun() tea.Cmd {
	if m.pendingRun == nil {
		return nil
	}
	req := *m.pendingRun
	m.pendingRun = nil
	return m.runRequest(req)
}

// startRun is the run itself, past the questions about which harness it lands
// on. A run those questions interrupted resumes here rather than at run(): they
// have already been answered, and asking again against a listing that has not
// caught up yet would ask the same one twice.
func (m *Model) startRun(req RunRequest) tea.Cmd {
	// A discobox with no source has no working tree to carry anything out of,
	// so there is nothing the question could be about.
	if req.IncludeDirty != "" || req.NoSource {
		return m.create(req)
	}
	// --include-dirty=auto means ask, and there is only something to ask about
	// when the working tree has something in it.
	m.waiting("checking the working tree")
	return func() tea.Msg {
		workspace, err := m.ds.Workspace(m.ctx, req.Source)
		return workspaceCheckedMsg{req: req, workspace: workspace, err: err}
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
		return m.report(true, "this project has no harness to run; register one with `discobox admin harnesses create`"), true
	}

	items := make([]action, 0, len(candidates))
	for i, harness := range candidates {
		detail := "make it the project default"
		if harness.State != HarnessEnabled {
			detail = "set it up, then make it the default"
		}
		n := itoa(i + 1)
		items = append(items, action{
			key: n, press: n, label: harness.displayName(), detail: detail, enabled: true,
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
	menu.keys = []hint{pressing("Enter chooses", "enter"), pressing("Esc cancels", "esc")}
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

func (m *Model) workspaceChecked(msg workspaceCheckedMsg) tea.Cmd {
	m.busy = ""
	if msg.err != nil {
		return m.report(true, "cannot read the working tree: %v", msg.err)
	}
	if !msg.workspace.Carries {
		return m.create(msg.req)
	}
	if !msg.workspace.Repository {
		return m.askToCopyDirectory(msg.req, msg.workspace.Directory)
	}
	// Excluding leads, the way it does in `discobox run`: the default answer is
	// the one that changes nothing about what the sandbox sees. The repository
	// named is the one the run is cut from, which is not the window's own
	// directory when the source option names another.
	m.dialog = m.includeDirtyDialog("Uncommitted changes", dirtyWorkspaceBody(msg.workspace), true, msg.req)
	return nil
}

// dirtyWorkspaceBody says what the question is about: the directory, how many
// paths differ from the checked-out commit, and enough of them to recognize the
// change by. A listing that reported no paths still asks the question — what it
// is about is the working tree, not the count.
func dirtyWorkspaceBody(workspace SourceWorkspace) string {
	subject := workspace.Directory + " has uncommitted changes"
	if n := len(workspace.Changes); n > 0 {
		subject = fmt.Sprintf("%s has %s (%s)",
			workspace.Directory, plural(n, "uncommitted change", "uncommitted changes"), summarizePaths(workspace.Changes))
	}
	return subject + ". Carry them into the discobox as a snapshot on top of the checked-out commit?"
}

// summarizePaths is a few of the paths and then how many more there are: enough
// to recognize the change by, without turning the question into a file listing.
func summarizePaths(paths []string) string {
	const shown = 3
	summary := strings.Join(paths[:min(shown, len(paths))], ", ")
	if len(paths) > shown {
		summary = fmt.Sprintf("%s and %d more", summary, len(paths)-shown)
	}
	return summary
}

// askToCopyDirectory is the same question for a source directory that is in no
// Git repository: there is no commit to fall back to, so what it decides is
// whether the directory itself is copied in. Answering no still creates the
// discobox — with nothing checked out in it, the way --no-source does
// (ADR 0077 §1).
//
// The size is counted while the question is up rather than before it: a home
// directory takes long enough to walk that waiting for the number would be the
// thing the question exists to avoid.
func (m *Model) askToCopyDirectory(req RunRequest, dir string) tea.Cmd {
	total, stop := m.ds.MeasureDirectory(m.ctx, dir)
	m.copySize, m.copyStop, m.copyDir = total, stop, dir
	m.dialog = m.includeDirtyDialog("Copy this directory?", copyDirectoryBody(dir), false, req)
	m.dialog.emphasis = directoryCopySize(total())
	m.copyDialog = m.dialog
	return m.pollDirectorySize()
}

// includeDirtyDialog is the two-answer question about what local content
// reaches the discobox. It uses the same two descriptive rows as the standalone
// run prompt, with the excluding answer first so Enter means no. Both answers
// are answers — the discobox is created either way — so cancel is heard as no.
func (m *Model) includeDirtyDialog(title, body string, repository bool, req RunRequest) *dialog {
	answer := func(includeDirty string) tea.Cmd {
		req := req
		req.IncludeDirty = includeDirty
		return func() tea.Msg { return createMsg{req: req} }
	}
	items := []action{
		{key: "false", press: "n", label: "Do not copy the directory", detail: "Create the discobox with nothing checked out in it", enabled: true},
		{key: "true", press: "y", label: "Copy the directory in", detail: "Everything in it arrives as uncommitted changes", enabled: true},
	}
	if repository {
		items = []action{
			{key: "false", press: "n", label: "Start from the last commit", detail: "Leave the uncommitted changes here", enabled: true},
			{key: "true", press: "y", label: "Include uncommitted changes", detail: "Start the discobox from a snapshot of the working tree", enabled: true},
		}
	}
	d := actionsDialog(title, body, items, answer)
	d.singleLineBody = true
	d.onCancel = func() tea.Cmd { return answer("false") }
	return d
}

// directorySized writes the walk's latest total into the question it belongs
// to. A dialog that is no longer up has been answered, and the walk behind it
// is stopped rather than left reading the disk for nobody.
func (m *Model) directorySized() tea.Cmd {
	if m.copyDialog == nil || m.dialog != m.copyDialog {
		m.endDirectorySize()
		return nil
	}
	total := m.copySize()
	m.copyDialog.emphasis = directoryCopySize(total)
	if total.Done {
		m.endDirectorySize()
		return nil
	}
	return m.pollDirectorySize()
}

func (m *Model) pollDirectorySize() tea.Cmd {
	return tea.Tick(directorySizeInterval, func(time.Time) tea.Msg { return directorySizeMsg{} })
}

// endDirectorySize stops the walk and forgets it. It is safe on a question that
// was never asked, and on one whose walk already finished.
func (m *Model) endDirectorySize() {
	if m.copyStop != nil {
		m.copyStop()
	}
	m.copySize, m.copyStop, m.copyDialog, m.copyDir = nil, nil, nil, ""
}

// directorySizeInterval is how often the copy question re-reads its walk: often
// enough that the number is visibly climbing, and no more than that.
const directorySizeInterval = 200 * time.Millisecond

// copyDirectoryBody is the copy question. What it would cost is not in here:
// it is the dialog's emphasis, on a line of its own under this one, because it
// is the whole of what the answer turns on and it is still arriving.
func copyDirectoryBody(dir string) string {
	return dir + " is not a Git repository, so copying it into the discobox means copying all of it. " +
		"Answering no creates the discobox anyway, with nothing checked out in it."
}

// directoryCopySize is that line, with as much of the count as the walk behind
// the question has reached. Nothing counted yet says so rather than reporting a
// zero that is about to be wrong.
func directoryCopySize(total DirectoryTotal) string {
	if total.Files == 0 && !total.Done {
		return "calculating…"
	}
	counted := fmt.Sprintf("%s in %s", humanBytes(total.Bytes), plural(int(total.Files), "file", "files"))
	if !total.Done {
		counted += ", still counting…"
	}
	return counted
}

// createMsg carries a settled request back to the live model.
type createMsg struct{ req RunRequest }

// creatingTitle is the wait's title before there is a discobox to name. It
// becomes "Starting <name>" the moment the create comes back with one.
const creatingTitle = "Creating a discobox"

// waitDialog is the screen a window waits on: what it is waiting for, with the
// newest report under it.
//
// Leaving it is where the two kinds of window differ. The launcher goes back to
// the list it was started from, which is where the discobox it is waiting for
// will appear. A window that is one command's own has no list behind it, so
// leaving the wait leaves the window — and leaves the discobox alone: it was
// created before this screen went up, or is being created by a request the
// server has already taken, and it carries on coming up either way. Nothing is
// stopped and nothing is attached to.
func (m *Model) waitDialog(title, body string) *dialog {
	d := statusDialog(title, body)
	if m.oneShot() {
		// What the key does, and then what it does not do: the second half is
		// the reassurance, not an offer, so it names no key.
		d.keys = []hint{pressing("Esc closes this window", "esc"), says("the discobox carries on")}
		d.onCancel = func() tea.Cmd { return m.exit(nil) }
	}
	return d
}

func (m *Model) create(req RunRequest) tea.Cmd {
	if m.oneShot() {
		// The window is this run, so the wait is the screen: there is no list
		// behind it for a busy line to sit under. The launcher's own runs keep
		// the list they were started from and report on the busy line there.
		m.dialog = m.waitDialog(creatingTitle, "")
	}
	m.waiting("creating the discobox")
	// Create resolves a source, may snapshot a dirty tree, and may push the
	// whole thing to a server that cannot reach this directory. Which of those
	// is underway is knowable only here, so the shared creation path reports it
	// and the busy line follows along (ADR 0060).
	feed, next := m.narrate()
	create := func() tea.Msg {
		defer feed.close()
		sandbox, err := m.ds.Run(m.ctx, req, feed.report)
		return createdMsg{sandbox: sandbox, req: req, err: err}
	}
	return tea.Batch(create, next)
}

func (m *Model) created(msg createdMsg) tea.Cmd {
	m.endNarration()
	m.busy = ""
	if msg.err != nil {
		// The wait is over and there is nothing to wait for, so it goes: the
		// report belongs on a screen somebody can read it on. Only this
		// window's own wait is taken down; a dialog the user opened is theirs.
		if m.dialog != nil && m.dialog.kind == dlgStatus {
			m.dialog = nil
		}
		return m.report(true, "cannot create the discobox: %v", msg.err)
	}
	// The prompt has been spent. Clearing it is what makes the window usable
	// twice in a row without reaching for a delete key.
	m.prompt.SetValue("")
	m.edits.reset()
	m.layout()
	if msg.req.Detach {
		return tea.Batch(m.refresh(), m.report(false, "created %s", msg.sandbox.ID))
	}
	if m.oneRun {
		// The window was opened to make this discobox and show it, so from
		// here it is the attach on it: leaving the workspace leaves the
		// window, and there is no list behind it to fall back to.
		box := msg.sandbox
		m.attach = &box
	}
	// The window goes to the discobox being made, rather than sitting on a
	// list with a busy line under it. Everything the wait is spent on — a pool
	// coming up, an image arriving, a container starting — reports here until
	// the attach can take over, and this takes itself down when it does.
	m.dialog = m.waitDialog("Starting "+sandboxLabel(msg.sandbox), "creating the discobox")
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

// bodyLeft and headerTop are where a screen's content begins inside the box:
// past the border and its padding, and under the top edge. They are what the
// hit map's origins are pushed by, so a control is marked where it is drawn.
const (
	bodyLeft  = 1 + boxPad
	headerTop = 1
)

// logoColumn is how far the mark pushes the body to the right, or nothing when
// there is no room for one.
func (m *Model) logoColumn() int {
	if !m.showLogo() {
		return 0
	}
	return m.logo.column()
}

// windowChrome is what the window costs in rows before any sandbox is in it.
// See layout, which is the only place it is used and where it is counted out.
const windowChrome = 11

// promptMaxRows is how far the composer grows before it scrolls instead. Three
// rows is enough to see the sentence you are still writing, and little enough
// that the list is still a list underneath it — a field that grew with the
// text would take the window over for a prompt you are only halfway through.
const promptMaxRows = 3

func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	// The harnesses screen is a window of its own, and is measured whether or not
	// it is up: it is opened by a key that can be pressed on any frame, and a
	// list sized on the frame after that would open with no rows in it.
	m.harnesses.width, m.harnesses.height = m.bodyWidth(), max(m.height-harnessesChrome, 0)
	m.harnesses.clamp()
	// The lower table takes what its rows need, up to a third of the screen:
	// what is waiting is usually a line or two, and a fixed split would spend
	// half the screen on "nothing is waiting".
	secretsRoom := max(m.height-secretsChrome, 0)
	waiting := min(len(m.requestRows.all), max(secretsRoom/3, 1))
	if waiting == 0 {
		waiting = 1 // the empty state is a row too
	}
	m.requestRows.width, m.requestRows.height = m.bodyWidth(), waiting
	m.requestRows.clamp()
	// The secrets take only the rows they have, up to what is left. Sized to
	// the room instead, a short list would pad itself to the foot of the window
	// and push the requests down with it — two tables read together, with a
	// screen of nothing in between.
	secretsLeft := max(secretsRoom-waiting-requestsChrome, 1)
	m.secrets.width, m.secrets.height = m.bodyWidth(), min(max(len(m.secrets.all), 1), secretsLeft)
	m.secrets.clamp()
	if !m.expanded && !m.inPanes() {
		m.compactLayout()
		// An opening frame taller than the screen it is printed on loses its
		// top rows into the terminal's scrollback the moment it is printed, and
		// nothing can take them back. There is no small window at this size.
		// See fitsInline.
		if !m.compactFits() {
			m.expand()
		}
		return
	}
	// The workspace takes the whole window: a terminal wants every row it can
	// get, and the list underneath is not what you are looking at. Every pane
	// is sized for the box it is drawn in, the hidden tabs included — flipping
	// to one must show a screen drawn at the size it is shown at.
	if m.inPanes() {
		for _, p := range m.allPanes() {
			p.term.SetSize(m.paneCells(m.paneWidthOf(p)))
		}
		if m.overlay != nil {
			// It has the screen, whatever is under it.
			m.overlay.term.SetSize(m.paneCells(m.width))
		}
		return
	}
	// The composer grows with what is typed, one row at a time, the way
	// Claude Code's does — and the list gives up a row for each one it takes.
	// The width comes first: how tall the text is depends on where it wraps.
	m.prompt.SetWidth(max(m.inner()-2, 10))
	promptH := m.prompt.Height()
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
}

// View draws the window. It records what it drew as well: a frame with anything
// on it, drawn inline, is text printed on the screen the window was started
// from, and an empty inline frame is that text erased. That is what clearPrinted
// goes on.
func (m *Model) View() tea.View {
	view := m.view()
	m.printed = !view.AltScreen && view.Content != ""
	return view
}

func (m *Model) view() tea.View {
	// Every frame starts with an empty hit map and fills it as it draws. A
	// control that forgets to mark itself is one the mouse cannot reach. The
	// pointer's resting place goes in with it, so a renderer can ask whether
	// what it is drawing is under it in the same coordinates, and at the same
	// moment, as the mark it makes for it.
	m.zones.reset()
	m.zones.setHover(m.hoverX, m.hoverY)
	if m.quit {
		return tea.NewView("")
	}
	// Nothing at all, inline, while the rows the opening prompt printed are
	// erased off the screen it printed them on. See clearPrinted.
	if m.clearing > 0 {
		return tea.NewView("")
	}
	if !m.ready {
		return tea.NewView("Loading…")
	}

	// The introduction is the window until it is dismissed, ahead of the screen
	// it was opened on and ahead of any modal. See welcome.go.
	if m.welcoming {
		return m.altView(m.place(m.viewWelcome))
	}

	// A modal is drawn in place of the window rather than over it, and closing
	// it puts the window back. It carries its own border, so it needs none of
	// the frame below.
	if m.dialog != nil {
		return m.altView(m.place(func() string { return m.dialog.view(m.st, &m.zones, m.width, m.height) }))
	}
	if m.optionsOpen {
		return m.altView(m.place(func() string { return m.opts.view(m.st, &m.zones, m.width, m.prompt.Value()) }))
	}

	// The header names the session; what is under it is a different kind of
	// thing, and butted together they read as one block.
	var content string
	switch {
	case !m.expanded && !m.inPanes():
		// Nothing on this frame answers a press: it is printed inline, under
		// the command that started the window, where a coordinate belongs to
		// the terminal's screen and not to these rows. Whatever its renderers
		// marked on the way past is dropped rather than left to be read
		// against the wrong ones. See mouseMode.
		marked := m.zones.count()
		content = m.viewCompact()
		m.zones.drop(marked)
	case m.inPanes():
		// A pane wears the border itself. Everything else — the header, what
		// the sandbox is called, the keys — sits outside it, the way a caption
		// sits outside the thing it captions.
		content = m.viewPaneWindow()
	case m.harnessesOpen:
		// The harnesses screen is the window while it is up, drawn in the same box
		// with the same header: it is another list of things you act on, not a
		// panel over the launcher.
		content = m.viewHarnesses()
	case m.secretsOpen:
		content = m.viewSecrets()
	default:
		// The header spans the window; under it the mark stands beside the
		// list, and the composer spans both again. Each block is drawn with
		// the origin it is being placed at pushed, so what it marks lands
		// where it is drawn. See zones.go.
		m.zones.push(bodyLeft, headerTop)
		rows := []string{m.viewHeader(m.inner()), ""}
		m.zones.pop()

		m.zones.push(bodyLeft+m.logoColumn(), headerTop+2)
		body := m.list.view(m.st, &m.zones, m.focus == focusList)
		m.zones.pop()
		if m.showLogo() {
			// The mark is centered against whatever the list came out at, so
			// it follows the window rather than the window working around it.
			body = lipgloss.JoinHorizontal(lipgloss.Top, m.logo.view(lipgloss.Height(body)), body)
		}
		rows = append(rows, strings.Split(body, "\n")...)

		m.zones.push(bodyLeft, headerTop+2+len(strings.Split(body, "\n")))
		rows = append(rows, strings.Split(m.viewPrompt(), "\n")...)
		m.zones.pop()

		content = m.box("", rows)
	}
	view := tea.NewView(m.paintChrome(content))
	// The whole terminal, once the window has opened out — but not before: the
	// opening prompt is inline, sitting under the command that started it.
	view.AltScreen = m.takesScreen()
	view.MouseMode = m.mouseMode()
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

// place draws a modal surface in the middle of the window.
//
// It draws it twice: once to measure, and once at the origin that measurement
// gives it. A card is centered on whatever it comes out at, so where it lands
// is not known until it has been drawn — and a renderer that shades the row
// the pointer is on has to know before it draws, which no amount of moving the
// marks afterwards can give it. The first pass's marks are dropped; the
// second's are the frame's. Styling costs no cells, so the two passes are the
// same size whatever the pointer is doing.
func (m *Model) place(render func() string) string {
	marked := m.zones.count()
	content := render()
	if m.width <= 0 || m.height <= 0 {
		return m.paintChrome(content)
	}
	m.zones.drop(marked)
	m.zones.push(centerOffset(m.width, lipgloss.Width(content)), centerOffset(m.height, lipgloss.Height(content)))
	content = render()
	m.zones.pop()
	return m.paintChrome(m.center(content))
}

// centerOffset is where lipgloss.Place puts a block of this size: the leading
// half of the gap, rounded the way Place rounds it, so a hit map and a frame
// cannot disagree by a cell on an odd one.
func centerOffset(outer, inner int) int {
	gap := outer - inner
	if gap <= 0 {
		return 0
	}
	return gap - int(math.Round(float64(gap)/2))
}

// takesScreen reports whether the frame the window draws now is the whole
// terminal. Everything but the opening prompt is: a modal and the options panel
// stand in place of the window rather than inside it, and both can be up before
// the window has opened out.
func (m *Model) takesScreen() bool {
	return m.expanded || m.inPanes() || m.dialog != nil || m.optionsOpen || m.welcoming
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
	left := m.viewHeaderLeft()
	hints := m.headerHints()
	// The keys sit hard against the far end, which is where spread pins them,
	// so where they will land is known before they are drawn. A row with no
	// room for them drops them whole — spread would too — and an offer that is
	// not on screen is not marked.
	keys := hintsWidth(hints, headerSep)
	if width-lipgloss.Width(left)-keys < 1 {
		return spread(left, "", width)
	}
	return spread(left, viewHints(m.st, &m.zones, hints, width-keys, headerSep), width)
}

// viewHeaderLeft is where you are: the project when it is not the usual one,
// and the folder the window is working in.
func (m *Model) viewHeaderLeft() string {
	brand := m.viewHeaderBrand()
	folder := m.viewFolder(false)
	// The filter is a dropdown, and a dropdown opens when it is clicked. It is
	// measured before it is shaded because styling costs no cells: the width
	// the mark takes is the width either way.
	x, width := lipgloss.Width(brand), lipgloss.Width(folder)
	m.zones.mark(hit{kind: hitFolder}, x, 0, width, 1)
	if m.zones.hovering(x, 0, width, 1) {
		folder = m.viewFolder(true)
	}
	return brand + folder
}

// viewHeaderBrand is the program's own name, and the project it is pointed at
// when that is not the one you are almost always in — a header that says
// "default" every time teaches you to skip it.
//
// It is a piece of its own rather than the head of viewHeaderLeft because the
// workspace's banner gives it up separately from the folder beside it when the
// row runs out of room (viewPaneHeader).
func (m *Model) viewHeaderBrand() string {
	brand := m.st.headerLabel.Render("discobox  ")
	if m.session.Project != "" && m.session.Project != m.session.DefaultProject {
		brand += m.st.headerBar.Render(m.session.Project) + m.st.headerLabel.Render("  ")
	}
	return brand
}

// headerSep sets the header's own offers further apart than the status line's:
// there are three or four of them against a whole row, where the status line
// is a list that has to fit.
const headerSep = "  ·  "

// headerHints is the row's right end: the keys that work wherever you are.
//
// A pane owns the keyboard, so the ones the header offers everywhere else are
// not among them; the one that is, is the way out.
func (m *Model) headerHints() []hint {
	if p := m.focusedPane(); p != nil {
		out := "detach"
		if p.tool != "" {
			// The leader's way out of a screen is the same key; what it does
			// here is put this window away, not leave the workspace.
			out = "put away"
		}
		hints := []hint{pressing(m.detachHint()+" "+out, m.leader(), paneDetachAlt)}
		// Quit is only worth its half of the row where it differs from detach.
		// In a window that is one command's own the two do the same thing —
		// the window goes, the discobox and everything in it carries on — and a
		// row offering both reads as a choice between them.
		if !m.oneShot() {
			hints = append(hints, pressing(m.leader()+" "+paneQuitKey+" quit", m.leader(), paneQuitKey))
		}
		return hints
	}
	// The harnesses screen is advertised here rather than on the status line
	// because it is reachable from every one of the window's own screens, and
	// the status line says what the screen you are on can do.
	//
	// The quit is whichever one works where you are: leader-q everywhere the
	// leader is armed, so it reads the same as the workspace's, and Ctrl-C in
	// the prompt, where Ctrl-A belongs to the composer. Ctrl-C still quits on
	// both; this is what the window offers, not all it accepts.
	quit := pressing(m.leader()+" "+paneQuitKey+" quit", m.leader(), paneQuitKey)
	if m.focus == focusPrompt {
		quit = pressing("Ctrl-C quit", "ctrl+c")
	}
	return []hint{
		keyed("F1", "f1", "help"),
		keyed(HarnessesKeyName, harnessesKey, "harnesses"),
		keyed(SecretsKeyName, secretsKey, "secrets"),
		quit,
	}
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
func (m *Model) viewFolder(hovered bool) string {
	label := m.folderLabel()
	if m.focus == focusFolder {
		// The keyboard is already on it and it wears its own marks; the
		// pointer resting there has nothing left to say.
		return m.st.key.Render("‹ ") + m.st.cursorName.Render(label) + m.st.key.Render(" ›")
	}
	style := m.st.headerLabel
	if hovered {
		style = m.st.hover
	}
	return style.Render(label + " ▾")
}

// viewPrompt draws the composer the way Claude Code draws its own: a rule
// above and below the text, a chevron in front of it, and one dim line under
// the rule for the mode you are in. The rule brightens when the prompt has
// focus, which is the only thing that has to be visible from across the room.
func (m *Model) viewPrompt() string {
	// What the field does sits against the field; the keys are keys, and
	// belong on the status line with everything else transient.
	composer := m.viewComposer(m.inner())
	m.zones.push(0, lipgloss.Height(composer))
	status := m.viewStatus()
	m.zones.pop()
	return composer + "\n" + status
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
	chips := m.opts.chips(m.st)
	if m.focus != focusPrompt {
		chips = m.opts.mutedChips(m.st)
	}
	mode := padANSI("  "+chips, width)

	// The field is two rows down, under its label and its rule, and it is as
	// tall as the text has made it. A press in it is the caret moving and a
	// drag is a selection, both handed to the textarea in its own coordinates
	// — which is why the whole field is marked rather than its rows.
	m.zones.mark(hit{kind: hitPrompt}, 0, 2, width, m.prompt.Height())
	// The strip under it names the run options, so it is the way into them.
	m.zones.mark(hit{kind: hitChips}, 2, 3+m.prompt.Height(), lipgloss.Width(chips), 1)

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
//
// The right end is what is true rather than what was said — which discobox the
// cursor is on, and how many are selected — so a message displacing the keys
// leaves it standing. It is pinned there (`spreadPin`): the keys are a list
// with a tail worth losing and F1 spells all of them out anyway, while the
// identity is the one thing on the row that is written down nowhere else.
func (m *Model) viewStatus() string {
	var fields []string
	if id := m.statusIdentity(); id != "" {
		fields = append(fields, id)
	}
	if n := m.list.selectionCount(); n > 0 {
		fields = append(fields, m.st.statusWA.Render(plural(n, "selected", "selected")))
	}
	fields = withReport(fields, m.viewInitialization(), m.inner())
	right := ""
	if len(fields) > 0 {
		right = strings.Join(fields, "   ") + "  "
	}
	// The two spaces the line opens with are the origin its offers are marked
	// against.
	m.zones.push(2, 0)
	keys := m.statusLine(max(m.inner()-lipgloss.Width(right)-2, 1))
	m.zones.pop()
	return spreadPin("  "+keys, right, m.inner())
}

// withReport puts the initialization report at the end of a status row's pinned
// fields, and drops what was already there when the two cannot both fit.
//
// The report goes last because that is where it belongs on the row, and the
// identity beside it gives way rather than the report: the identity is a fact
// about the row the cursor is on, one press from being seen again, while the
// report is the only account on screen of a wait the user did not ask for.
// Half the row is what the pinned end may take before that trade is made —
// past that the keys are being squeezed for two things at once, and the report
// is the one of them that cannot be found anywhere else.
func withReport(fields []string, report string, width int) []string {
	if report == "" {
		return fields
	}
	together := append(append([]string{}, fields...), report)
	if lipgloss.Width(strings.Join(together, "   ")) > width/2 {
		return []string{report}
	}
	return together
}

// statusIdentity is the discobox under the cursor, named the two ways the row
// itself cannot name it: its id, and the name it is configured with.
//
// Neither is on the row. The name a row shows is the display name, which is
// the primary terminal's window title as soon as the harness has set one — so
// the configured name, the one rename edits and every other `discobox` command
// takes, is exactly the thing that goes missing while a box is working. The id
// is nowhere on this screen at all; the workspace banner carries it, and
// getting to it meant attaching.
//
// The id leads and the name follows it, the same order and the same colors as
// the workspace banner (`paneHeaderFields`): muted for the id, which is there
// to be looked up rather than read, and the plain text for the name. A box
// that was never named has no second half — the server names it by its id, so
// the id has already said it.
//
// Only while the list has focus, because only then is there a cursor drawn on
// a row: naming a discobox nothing on screen is pointing at would be an answer
// to a question nobody asked.
func (m *Model) statusIdentity() string {
	if m.harnessesOpen || m.focus != focusList {
		return ""
	}
	box := m.list.current()
	if box == nil {
		return ""
	}
	out := m.st.dimText.Render(box.ID)
	if box.ConfigName != "" {
		out += "  " + m.st.name.Render(box.ConfigName)
	}
	return out
}

// statusLine is what the bottom line of any screen says, in the room it has:
// the keys, or what just happened over them.
//
// Every screen draws it, the workspace included. A command that failed there —
// an apply that could not start, a key that could not do what it was pressed
// for — has nowhere else to say so, and a report the screen it was made on
// cannot show is a key that looks like it did nothing at all.
//
// The keys give up whole offers to fit, from the tail, where the least of them
// is (`fitFields`): half a key hint is not one. A message is returned whole —
// it is one thing and there is nothing of it to drop — and the caller's own
// truncation is what deals with one too long for the row.
//
// Each offer that survives is marked where it landed, because a hint that
// names a key is a button for that key: the press is handled as the key press
// itself, so the pointer and the keyboard cannot come to mean two different
// things. See ADR 0088 §5.
func (m *Model) statusLine(room int) string {
	switch {
	case m.statusE:
		return m.st.statusER.Render("✗ " + m.status)
	case m.status != "":
		return m.st.statusOK.Render(m.status)
	case m.busy != "":
		return m.st.statusWA.Render(m.busy)
	}
	return viewHints(m.st, &m.zones, fitHints(m.hints(), hintSep, room), 0, hintSep)
}

// viewHints draws a row of key hints, marks the pressable ones where they
// land, and picks out whichever the pointer is resting on. x is where the row
// starts, in the coordinates the caller has pushed.
//
// Drawing and marking are the same walk because they are the same arithmetic:
// a hint highlighted somewhere other than where it is pressed would be a
// button that lies about itself.
func viewHints(st *styles, z *zones, hints []hint, x int, sep string) string {
	parts := make([]string, 0, len(hints))
	for _, h := range hints {
		width := lipgloss.Width(h.text)
		style := st.dimText
		if len(h.keys) > 0 {
			z.mark(keyHit(h.keys...), x, 0, width, 1)
			if z.hovering(x, 0, width, 1) {
				// A control the pointer is on says so before it is pressed.
				style = st.hover
			}
		}
		parts = append(parts, style.Render(h.text))
		x += width + lipgloss.Width(sep)
	}
	return strings.Join(parts, st.dimText.Render(sep))
}

// fitHints is a key line cut to the room it has, from the tail, where the
// least of it is: half a hint is not one. See fitFields.
func fitHints(hints []hint, sep string, room int) []hint {
	text := make([]string, 0, len(hints))
	for _, h := range hints {
		text = append(text, h.text)
	}
	return hints[:len(fitFields(text, sep, room))]
}

// hintsWidth is how wide that row comes out, for the callers that have to
// place it before they can draw it. Styling costs no cells, so the answer is
// the same either way.
func hintsWidth(hints []hint, sep string) int {
	width := 0
	for i, h := range hints {
		if i > 0 {
			width += lipgloss.Width(sep)
		}
		width += lipgloss.Width(h.text)
	}
	return width
}

// hintSep is what a key hint is separated from the next by, and so what a row
// of them is joined with.
const hintSep = " · "

// detachDescription is what the leader's d does, which depends on what the
// window is: the launcher goes back to its list, and a window that is one
// command's own — `discobox run`, `discobox attach` — has no list behind it and
// goes. Neither stops anything: what follows this in the key list is "leaving
// every session running", which is true of both.
func (m *Model) detachDescription() string {
	if m.oneShot() {
		return "detach, which closes this window"
	}
	return "detach from the whole workspace"
}

// hint is one offer on a key line: what it says, and the keys that pressing it
// stands for. The two travel together rather than being formatted into a line
// and parsed back out of it, because the renderer is what knows where each
// offer landed and the mouse needs both halves. Empty keys is an offer that
// names no one key — a pair of arrows, a statement about the screen — and is
// text and nothing more.
type hint struct {
	text string
	keys []string
}

// keyed is the common shape: the key as it is spelled to the reader, the
// keystroke that is, and what it does.
func keyed(spelling, press, label string) hint {
	return hint{text: spelling + " " + label, keys: []string{press}}
}

// says is an offer with no key behind it.
func says(text string) hint { return hint{text: text} }

// pressing is an offer whose text is written out in full — a chord, or a key
// named in the middle of a sentence — with the keystrokes it stands for.
func pressing(text string, keys ...string) hint { return hint{text: text, keys: keys} }

func (m *Model) hints() []hint {
	// Only while the screen is the one on screen. A pane opened from it is
	// drawn over it and takes the keys (updateKey), so offering the list's own
	// keys under a configure terminal would be offering keys that no longer do
	// what they say.
	if !m.inPanes() {
		if m.harnessesOpen {
			return m.harnessHints()
		}
		if m.secretsOpen {
			return m.secretHints()
		}
	}
	switch m.focus {
	case focusPane:
		return m.paneHints()
	case focusFolder:
		return []hint{
			says("←→ change folder"),
			pressing("Enter lists them all", "enter"),
			pressing("↓ boxes", "down"),
			pressing("Tab or Esc prompt", "esc"),
		}
	case focusList:
		if m.list.visual {
			lo, hi := m.list.visualRange()
			return []hint{
				says(fmt.Sprintf("VISUAL  %s", plural(hi-lo+1, "box", "boxes"))),
				says("↑/↓ extend"),
				pressing("Space selects", " "),
				says("a letter acts on the range"),
				pressing("V or Esc cancel", "V"),
			}
		}
		var out []hint
		if m.list.nameFull > m.list.nameWidth {
			// Only worth saying on a row that has more name than column.
			out = append(out, says("←→ read the rest of the name"))
		}
		// Only the actions the sandboxes under the cursor can actually take:
		// a key list that offers purge on a running sandbox is a key list you
		// stop reading.
		for _, a := range m.actions(m.list.targets()) {
			if a.enabled {
				out = append(out, keyed(a.key, a.key, a.label))
			}
		}
		out = append(out, keyed("Space", " ", "select"), keyed("V", "V", "range"))
		if m.list.showArchived {
			out = append(out, keyed("A", "A", "hide archived"))
		} else if m.list.archivedCount() > 0 {
			out = append(out, keyed("A", "A", "show archived"))
		}
		return append(out, pressing("↑ or Tab folder", "tab"), pressing("Esc prompt", "esc"))
	default:
		// ↑ is only a way out of an empty prompt; with text in it, it walks
		// the text and Tab is the way to the list.
		out := pressing("Tab or ↑ discoboxes", "tab")
		if m.prompt.Value() != "" {
			out = pressing("Tab discoboxes", "tab")
		}
		return []hint{
			out,
			keyed("Shift-Tab", "shift+tab", "harness"),
			keyed("Ctrl-O", "ctrl+o", "options"),
			keyed("Alt-E", "alt+e", "editor"),
			says("Shift/Ctrl-Enter newline"),
			keyed("Ctrl-D", "ctrl+d", "quit"),
		}
	}
}

// paneHints is the workspace's key line: what the screen you are looking at
// can do, and the way out of it.
//
// Its offers are mostly the leader's, so they carry two keystrokes rather than
// one — a click on `ctrl+a s` arms the leader and then presses s, which is
// exactly what the two presses would have done.
func (m *Model) paneHints() []hint {
	p := m.focusedPane()
	if p == nil {
		return nil
	}
	leader := m.leader()
	if p.exited {
		// The same word the banner is using: a pane that failed says so at
		// both ends of itself, rather than reporting the end of the stream
		// as a result.
		done := p.status
		if done == "" {
			done = "finished"
		}
		out := []hint{says(done)}
		if p.term.ScrollbackLen() > 0 {
			out = append(out, says("↑↓ pgup/pgdn scroll"))
		}
		out = append(out, keyed("q", "q", "closes"))
		if !m.hasScreen(p) {
			out = append(out, says("←/→ pane"))
		}
		return out
	}
	if p.tool != "" {
		return m.toolHints(p)
	}
	// A workspace terminal is the discobox's own and you detach from the
	// whole workspace; the command over them is this CLI's, and you close
	// it alone.
	what, out := "the box", "detach"
	if p == m.overlay {
		what, out = string(p.action), "close"
	}
	hints := []hint{
		says("every key goes to " + what),
		pressing(m.detachHint()+" "+out, leader, paneDetachAlt),
	}
	if p.service != "" {
		// A service reads no input, so the line does not open by promising
		// it every key. What it offers instead is what can be done to the
		// service from the pane you are looking at — the two verbs that
		// mean this service here rather than the discobox.
		hints = []hint{
			says("read-only"),
			pressing(m.detachHint()+" "+out, leader, paneDetachAlt),
			pressing(leader+" t stop", leader, "t"),
			pressing(leader+" T start", leader, "T"),
			// The one place the services menu is advertised. It is where
			// restart lives, and where the services with no tab are, and
			// a pane onto one is where you would think to look.
			pressing(leader+" "+paneServicesKey+paneServicesMenuKey+" services", leader, paneServicesKey, paneServicesMenuKey),
		}
	}
	if m.screenPane() != nil {
		return hints
	}
	// Restoring outranks everything below it, because it is the way out of a
	// state the screen is in right now — the same thing that keeps detach at
	// the front — while maximizing is only something on offer. So the one key
	// changes place depending on which of the two it currently is.
	//
	// Either way it is only there with two columns to choose between: more
	// terminals are more tabs in the one box.
	var zoom []hint
	if m.shells.len() > 0 {
		zoom = []hint{pressing(leader+" "+paneZoomKey+" maximize", leader, paneZoomKey)}
		if m.maximized {
			hints = append(hints, pressing(leader+" "+paneZoomKey+" restore", leader, paneZoomKey))
			zoom = nil
		}
	}
	// Only the shell is offered here. Another terminal is the advanced one of
	// the two — a second harness session, next to a shell it sounds exactly
	// like — and a hints line that names both spends its scarcest row teaching
	// a distinction most people never need. It is in the help, under the key
	// that opens it.
	if p.service == "" {
		hints = append(hints, pressing(leader+" s shell", leader, "s"))
	}
	// The tools sit here for the same reason the services menu sits on a
	// service's line: this is the only place they are advertised, and a picker
	// nothing points at is a picker nobody opens.
	hints = append(hints, pressing(leader+" "+toolsKey+" tools", leader, toolsKey))
	if len(m.panes()) > 1 {
		hints = append(hints, says(leader+" ←/→ pane"))
		if len(m.numbered()) > 1 {
			hints = append(hints, says(leader+" 0-9 jump"))
		}
		// The services' own alphabet is only worth a row when there are
		// services to jump to; when there are none the chord is still how the
		// menu opens, which the service pane's own line already says.
		if len(m.services()) > 0 {
			hints = append(hints, says(leader+" "+paneServicesKey+"1-9 service"))
		}
	}
	hints = append(hints, zoom...)
	// The seize toggle only matters while something in the box has the mouse;
	// the rest of the time selection simply works.
	switch {
	case m.mouseSeized:
		hints = append(hints, pressing(m.paneMouseHint()+" mouse back", leader, paneMouseKey))
	case p.term.MouseMode() != termpane.MouseNone:
		hints = append(hints, pressing(m.paneMouseHint()+" take mouse", leader, paneMouseKey))
	}
	return hints
}

// readableDialog is a scrolling card the window put up to be read rather than
// answered: the help, and a harness's configuration. Both are longer than the
// terminal drawing them, so both are searched with / and taken whole with c.
//
// The copy is wired here rather than into textDialog because the clipboard is
// the window's, and it strips the color on the way out: the config card is
// rendered through styles, and what belongs on a clipboard is the text rather
// than a screenshot of it. The help is plain, so stripping it costs nothing.
func (m *Model) readableDialog(title, body string) *dialog {
	d := textDialog(title, body)
	d.copy = func(body string) tea.Cmd { return m.copyText(ansi.Strip(body), "copied") }
	return d
}

// helpDialog is the F1 card, wherever the key was pressed.
func (m *Model) helpDialog() *dialog { return m.readableDialog("Keys", m.helpText()) }

// helpText is the F1 card: a key reference, not a tour. Every screen's keys in
// a fixed order, one line each, with prose only where a key list cannot say it
// — what a glyph means, what a column counts. Why a key is the key it is
// belongs in DESIGN.md; someone who opened this is looking for the key.
func (m *Model) helpText() string {
	leader := m.leader()
	return strings.Join([]string{
		"The window opens in the prompt. Tab cycles through the prompt, the",
		"discobox list, and the folder filter above it.",
		"",
		"  This help scrolls. / searches it, Enter keeps the matches, n and N",
		"  walk them, c copies the whole of it to the clipboard, and Esc",
		"  closes.",
		"",
		"───────────────────────────────────────────────────────────────",
		"Anywhere",
		"",
		"    F1             this help (? does the same on the lists)",
		"    Ctrl-L         repaint; inside a pane, the program in it too",
		"    Ctrl-C         quit, from every screen but a pane, where it goes",
		"                   to the program running there. " + leader + " " + paneQuitKey + " is the quit",
		"                   inside one",
		"",
		"  Everywhere but inside a pane, where every key goes to the program:",
		"",
		"    " + HarnessesKeyName + "             the harnesses",
		"    " + SecretsKeyName + "             the project's secrets",
		"    Ctrl-O         run options",
		"",
		"───────────────────────────────────────────────────────────────",
		"The prompt",
		"",
		"    Enter          run the prompt in a new discobox. Empty, it just",
		"                   creates one and attaches to it",
		"    Shift-Enter    newline. Ctrl-Enter, Alt-Enter and Ctrl-J too",
		"    Alt-E or F2    edit the prompt in $EDITOR",
		"    Tab            to the discobox list",
		"    Shift-Tab      switch the harness",
		"    Ctrl-D         quit, when the prompt is empty",
		"    ↑ ↓            move a line, wrapped rows included. On the first",
		"                   or last line, to the start or end of it",
		"",
		"  ↑ on an empty prompt goes to the discobox list, to the row the",
		"  cursor was last on. With text in the prompt it stays put, and Tab",
		"  is the way out. ↓ never leaves the prompt.",
		"",
		"  The field is an emacs-mode readline:",
		"",
		"    Ctrl-A / Ctrl-E    start / end of the line",
		"    Ctrl-B / Ctrl-F    back / forward a character (← → too)",
		"    Ctrl-← / Ctrl-→    back / forward a word (Alt-B and Alt-F too)",
		"    Ctrl-P / Ctrl-N    up / down a line",
		"    Alt-< / Alt->      start / end of the whole prompt",
		"    Ctrl-W             kill the word behind the cursor",
		"    Alt-D              kill the word ahead of it",
		"    Ctrl-K / Ctrl-U    kill to the end / start of the line",
		"    Ctrl-Y             yank the last kill back. A run of kills is",
		"                       one yank",
		"    Ctrl-_             undo. A run of typing is one change",
		"    Ctrl-T / Alt-T     transpose characters / words",
		"    Alt-U / Alt-L / Alt-C   upper / lower / capitalize the next word",
		"",
		"  The prompt is remembered per folder: what is in it when the window",
		"  closes is there the next time one opens here. Running it or",
		"  clearing it discards it.",
		"",
		"───────────────────────────────────────────────────────────────",
		"The discobox list",
		"",
		"    ↑ ↓ / k j      move",
		"    g / G          first / last (pgup and pgdn move a screen)",
		"    ← →            read the rest of a name too long for its column",
		"    Space          select, for acting on several at once",
		"    V              draw a range: ↑ ↓ extend it, Space selects the",
		"                   whole of it, V or Esc cancels",
		"    c              clear the selection",
		"    Tab            to the folder filter",
		"    Esc            back to the prompt, and so does ↓ past the end",
		"",
		"    Enter  attach          s  shell",
		"    v      open it in VS Code, in its own window",
		"    y      apply back to this directory",
		"    u      upgrade to the current image",
		"    R      repair — rebuild a broken discobox in place on the",
		"           current image, keeping its workspace and changes",
		"    " + credentialsKey + "      answer the credential request waiting on it",
		"    e      rename          t  stop",
		"    T      start           x  archive",
		"    U      unarchive       P  purge",
		"    A      show or hide archived discoboxes",
		"    r      refresh now (the list also refreshes on its own)",
		"    .      every action, as a menu",
		"",
		"  The bottom line offers only the keys the discoboxes under the",
		"  cursor accept. Archiving is reversible and asks nothing; purge",
		"  destroys the disk and asks first.",
		"",
		"  attach, shell and apply open a screen over the window and act on",
		"  one discobox. apply stays up when it finishes so its report can be",
		"  read; q closes it. vscode is different: it edits the box over",
		"  Remote-SSH in a separate window, and this one keeps running.",
		"",
		"  rename opens the current name for editing. Enter accepts, Esc",
		"  cancels. A box whose harness has titled its terminal shows that",
		"  title and cannot be renamed.",
		"",
		"  A row is: state, name, harness, git position, age, cpu, memory and",
		"  disk use, and lines changed.",
		"",
		"      ● running    ◐ starting    ○ stopped    ▪ archived",
		"      ✗ error — the message shows under the cursor",
		"      ↑ an upgrade to the current harness image is available",
		"      ! an agent is waiting on a credential; " + credentialsKey + " answers it",
		"",
		"  Without color, the glyph is replaced by the state spelled out in a",
		"  column.",
		"",
		"  main@a3f9c21 is the branch and commit the discobox's agent last",
		"  reported, or the commit it was cut from until it reports. The mark",
		"  beside it is the state of the work, most losable first:",
		"",
		"      *  dirty      uncommitted changes, held only by the discobox.",
		"                    Archiving now loses them",
		"      ⇡  ready      committed work no apply has landed anywhere yet.",
		"                    This is what an apply would bring back",
		"      ✓  applied    the head commit is the last one applied back",
		"         clean      as it was cut, with nothing to bring back",
		"         -          the agent has not reported yet",
		"",
		"  +N −N is lines added and deleted against the commit it was cut",
		"  from, not counting pulled upstream work.",
		"",
		"───────────────────────────────────────────────────────────────",
		"The folder filter",
		"",
		"  The path in the header is the folder whose discoboxes are listed.",
		"  It starts as the folder the window is running in, which is what",
		"  `discobox ls` shows.",
		"",
		"    ↑              reach it, from the top of the discobox list",
		"    ← →            change it without opening anything",
		"    Enter          open the list of folders, with what is in each",
		"    ↓              back down into the discoboxes",
		"",
		"───────────────────────────────────────────────────────────────",
		"The workspace screen",
		"",
		"  One discobox as the server has it: terminals on the left, shells",
		"  on the right, one of each visible at a time. The primary terminal",
		"  is the first one. Attaching joins every session, including ones",
		"  started from another window or another machine; harness terminals",
		"  go on the left, everything else on the right.",
		"",
		"  Each pane shows the key that reaches it. Terminals and shells are",
		"  numbered from the primary, which is always 0. Services are",
		"  numbered separately, S1, S2, in the order the repository declares",
		"  them.",
		"",
		"    " + leader + " " + paneDetachAlt + "       " + m.detachDescription() + ", leaving every",
		"                   session running",
		"    " + leader + " " + paneQuitKey + "       quit the window, leaving every session running",
		"    " + leader + " a       back to the primary terminal",
		"    " + leader + " " + paneTerminalKey + "       another terminal beside the primary: a fresh",
		"                   session of the harness this discobox runs",
		"    " + leader + " s       a new shell, in a new tab",
		"    " + leader + " ← / " + leader + " →  move along the screen — services, terminals,",
		"                   then shells — or h and l. Hold Ctrl to keep",
		"                   going: " + leader + " ^→ ^→ walks across without",
		"                   pressing the leader again",
		"    " + leader + " 0-9     jump to a terminal or shell by its number",
		"    " + leader + " " + paneServicesKey + "1-9    jump to a service by its number",
		"    " + leader + " " + paneServicesKey + paneServicesMenuKey + "      every service, running or not, with start,",
		"                   stop and restart for each",
		"    " + leader + " " + toolsKey + "       the tools, as a picker",
		"    " + leader + " " + credentialsLeaderKey + "       answer the credential request in the banner",
		"    " + leader + " " + paneZoomKey + "       give the focused column the whole window, or",
		"                   give the window back. Same as the [+] / [-]",
		"                   button on its top border. What is hidden stays",
		"                   connected and running",
		"    " + m.paneMouseHint() + "       take the mouse from a box that is using it, to",
		"                   select and copy; press again to give it back",
		"    " + leader + " y       apply back to this directory",
		"    " + leader + " u       upgrade      " + leader + " t / T   stop / start",
		"    " + leader + " x / U   archive / unarchive",
		"    " + leader + " R       repair",
		"",
		"  Every other key goes to the focused pane, Ctrl-C included: there it",
		"  belongs to the program, in a harness as much as in a shell.",
		"",
		"  apply runs in the screen itself, and the terminals underneath stay",
		"  connected and running. Every other command runs against the server",
		"  and reports on the status line.",
		"",
		"  A shell or terminal that exits leaves its last screen up to be",
		"  read. ↑ ↓, pgup, pgdn, g and G scroll output longer than the pane,",
		"  and q, Esc or Enter dismisses it. When the primary exits, the",
		"  screen closes.",
		"",
		"  Services are tabs on the left, after the terminals: the scripts",
		"  under .discobox/services, started when the discobox booted. A",
		"  service pane is read-only. One that failed, ended, or cannot run",
		"  still has a tab, marked, with the reason and its output; one you",
		"  stopped yourself has no tab. A tab is marked • when the service",
		"  has printed something since you last looked.",
		"",
		"  On a service pane, " + leader + " t and " + leader + " T stop and start that",
		"  service instead of the discobox, and restart is on " + leader + " " + paneServicesKey + paneServicesMenuKey + ".",
		"  Every other key still acts on the discobox.",
		"",
		"───────────────────────────────────────────────────────────────",
		"The tools (" + leader + " " + toolsKey + ")",
		"",
		"  Programs that run beside the agent. Each opens a window over the",
		"  workspace, which keeps running underneath.",
		"",
		"    d              diff — what has changed, in discobox-review",
		"    f              fresh — the fresh editor, in the box",
		"    " + vscodeKey + "              vscode — the box in VS Code, in its own window",
		"    " + toolFileKey + "              the highlighted tool's config, in $EDITOR",
		"    " + addressSSHKey + "              copy the ssh command for this discobox",
		"    " + addressGitKey + "              copy its git url",
		"",
		"    [-]            put it away. The session keeps running and stays",
		"                   attached, so reopening the tool shows where it",
		"                   has got to. " + m.detachHint() + " does the same",
		"    [x]            close it, ending the session. " + leader + " " + toolCloseKey + " does the",
		"                   same",
		"",
		"  diff and fresh run inside the discobox, on the copies its image",
		"  carries, so there is nothing to install locally. vscode is the",
		"  other way round: it edits the box over Remote-SSH.",
		"",
		"  Tool sessions live in the discobox, not in this window. Quit the",
		"  launcher with a diff open and the next attach picks it back up,",
		"  put away.",
		"",
		"  A tool can carry a config, kept on this machine and copied into a",
		"  discobox the first time that tool runs in one without it. fresh",
		"  does this, and " + toolFileKey + " in the picker edits it. It is a default, not",
		"  a sync: a copy already in a discobox is never overwritten, so an",
		"  edit only affects the next discobox.",
		"",
		"───────────────────────────────────────────────────────────────",
		"The harnesses (" + HarnessesKeyName + ")",
		"",
		"  The harnesses a discobox can run on, and everything you do to",
		"  them. `discobox configure` opens the window here.",
		"",
		"    ↑ ↓ / k j      move            g / G   first / last",
		"    e or Enter     enable it, or run its setup again. The setup",
		"                   takes over the terminal and asks its own",
		"                   questions; the window returns when it exits",
		"    d              disable it, deleting the secrets and files its",
		"                   setup created. It asks first, and releases the",
		"                   project default if it holds it",
		"    s              make it the project default, used by any",
		"                   discobox without a harness of its own",
		"    v              its full configuration: what it runs, which",
		"                   secret answers each variable it needs, and the",
		"                   files it carries",
		"    f              edit one of those files in $EDITOR",
		"    .              every action, as a menu",
		"    Esc or " + HarnessesKeyName + "      back to the launcher",
		"",
		"      ● enabled    ○ disabled    ✗ its setup did not finish",
		"      ★ the project default",
		"",
		"  Every harness the project has appears in the run options whether",
		"  or not it is enabled; one that needs no credentials runs without",
		"  setup. The default is listed first.",
		"",
		"───────────────────────────────────────────────────────────────",
		"The secrets (" + SecretsKeyName + ")",
		"",
		"  Two tables: the project's secrets, and the requests waiting on",
		"  you. Tab crosses between them, and ↓ off the bottom of one reaches",
		"  the other.",
		"",
		"    n              a new secret",
		"    " + grantCreateKey + "              grant it before anything asks",
		"    e              its binding",
		"    d              delete it",
		"    Enter          what stands on it. There, Enter reads a grant,",
		"                   " + grantRevokeKey + " withdraws one, and the first row grants the",
		"                   secret. On a request, Enter answers it",
		"    r              refresh",
		"    Esc or " + SecretsKeyName + "      back to the launcher",
		"",
		"───────────────────────────────────────────────────────────────",
		"Run options (Ctrl-O)",
		"",
		"    ↑ ↓            move            ← →   change the value",
		"    Enter          on Source, the whole list of them and a path of",
		"                   your own; on env or secrets, add one; on any other",
		"                   row the same as →",
		"    Backspace      drop the last env / secret (- too)",
		"    Ctrl-R         run with these options",
		"    Esc            put the panel away (Tab and Ctrl-O too)",
		"",
		"  The source follows the folder in the header: switching folders",
		"  also switches where a new discobox is cut from. Setting it here",
		"  overrides that.",
		"",
		"  The strip below the prompt always shows what is set. The panel",
		"  shows the equivalent `discobox run` command, live.",
		"",
		"───────────────────────────────────────────────────────────────",
		"The mouse",
		"",
		"  Everything on screen responds to it. A press on a row points at",
		"  it, a second press opens it. The right button opens that row's",
		"  menu, the same one . opens. The wheel scrolls whatever is under",
		"  the pointer without moving the keyboard focus.",
		"",
		"  Anything that names a key is a button for that key: the " + HarnessesKeyName + " in the",
		"  header, the a attach on the status line, the ‹ › on a run option.",
		"  Controls highlight under the pointer.",
		"",
		"  The window reads the mouse, so your terminal does not: drag to",
		"  select here rather than there, double-click for a word,",
		"  triple-click for a line, middle button pastes the last selection.",
		"  A finished selection goes to the clipboard; the right button over",
		"  one copies it again and clears it, as do Ctrl-Shift-C and ⌘-C.",
		"  Hold Shift for your terminal's own selection, as in tmux.",
		"",
		"  Inside a pane the box only sees the mouse while a program there",
		"  has asked for it; " + m.paneMouseHint() + " takes it back. The opening prompt is",
		"  printed in the terminal rather than drawn over it, so the mouse",
		"  there is the terminal's.",
		"",
		"  Press Esc to close.",
	}, "\n")
}

// sandboxLabel is what to call a discobox on screen: its name when it has one,
// its id when it does not — a freshly created one may be reported before the
// name it was given comes back.
func sandboxLabel(sandbox Sandbox) string {
	if name := strings.TrimSpace(sandbox.Name); name != "" {
		return name
	}
	return sandbox.ID
}
