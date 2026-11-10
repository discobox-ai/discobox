package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The harnesses screen is the project's harnesses and everything you do to
// them: make one the project default, read what it is set to, edit the files
// it carries, and reset it to its bundle's defaults. Every harness can run;
// there is no setup to pass first (ADR 26-11-02-152). A harness is signed in
// inside a discobox, and what a box changes reaches the harness when somebody
// saves it there (harness_changes.go).
//
// It is `discobox configure`, folded into the window. That command is this
// screen opened directly (WithHarnesses), so there is one list of harnesses
// with one set of keys rather than a second program with its own idea of both.
//
// It is a list, drawn like the discobox list and acting like it: a chevron for
// the cursor, a colored glyph for the state, a letter per action, and the same
// modal layer for the menu, the confirmations and the card. The two screens are
// the same kind of thing — a list of things you act on one at a time — so
// nothing here invents a second idiom for it.

// harnessesKey opens and closes the screen. It is a function key because the
// prompt takes every letter as text and the discobox list has spent them on its
// own actions — which is the same reason help is on F1 and the editor on F2.
var harnessesKey = bareKey("harnesses")

// HarnessesKeyName is how that key is spelled to the user. It is exported
// because `discobox configure` opens the window on this screen and says so in its
// help, and a key named in two places is a key that ends up spelled two ways.
var HarnessesKeyName = keyLabel(harnessesKey)

// harnessList is the screen's list: the harnesses, and where the cursor is in
// them.
type harnessList struct {
	all    []console.Harness
	cursor int
	offset int

	// loaded is whether the listing has arrived at least once. An empty listing
	// and one that has not landed yet look the same in `all` and mean opposite
	// things: the first says the project has no harnesses, the second says
	// nothing at all, and a prompt must not be refused on the strength of it.
	loaded bool

	width, height int

	// now is when the frame is being drawn, so the age column is a pure
	// function of the model and a test can render a fixed one.
	now func() time.Time

	// drawn is which rows the last render put on screen; see zones.go.
	drawn drawn
}

func newHarnessList() *harnessList { return &harnessList{now: time.Now} }

// setAll takes a refreshed listing, keeping the cursor on the harness it was on
// rather than on the row number it was at: nothing reorders the listing
// today, but a cursor that follows the harness is the cursor that acts on the
// one you were looking at.
func (l *harnessList) setAll(all []console.Harness) {
	l.loaded = true
	var onID string
	if h := l.current(); h != nil {
		onID = h.ID
	}
	l.all = all
	if onID != "" {
		for i, h := range all {
			if h.ID == onID {
				l.cursor = i
				break
			}
		}
	}
	l.clamp()
}

// clear forgets the listing, for a server whose harnesses have not been read
// yet: not loaded, so nothing is refused or asked on the strength of it.
func (l *harnessList) clear() {
	l.all, l.loaded, l.cursor, l.offset = nil, false, 0, 0
}

func (l *harnessList) current() *console.Harness {
	if l.cursor < 0 || l.cursor >= len(l.all) {
		return nil
	}
	return &l.all[l.cursor]
}

func (l *harnessList) move(delta int) {
	l.cursor += delta
	l.clamp()
}

func (l *harnessList) moveTo(i int) {
	l.cursor = i
	l.clamp()
}

func (l *harnessList) clamp() {
	if l.cursor >= len(l.all) {
		l.cursor = len(l.all) - 1
	}
	if l.cursor < 0 {
		l.cursor = 0
	}
	if l.height <= 0 {
		return
	}
	if l.cursor < l.offset {
		l.offset = l.cursor
	}
	if l.cursor >= l.offset+l.height {
		l.offset = l.cursor - l.height + 1
	}
	if l.offset < 0 {
		l.offset = 0
	}
}

func (l *harnessList) view(st *styles, z *zones) string {
	right := plural(len(l.all), "harness", "harnesses")
	blank := strings.Repeat(" ", max(l.width, 0))
	out := []string{renderTitle(st.titleList, "Harnesses", right, l.width)}

	body := make([]string, 0, l.height)
	if len(l.all) == 0 {
		body = append(body, st.dimText.Render(pad("  no harnesses in this project — register one with `discobox admin harness create`", l.width)))
	}
	l.drawn = drawn{top: len(out), first: l.offset}
	for i := l.offset; i < len(l.all) && len(body) < l.height; i++ {
		body = append(body, l.row(st, l.all[i], i))
		l.drawn.count++
	}
	for len(body) < l.height {
		body = append(body, blank)
	}
	block := append(append(out, body...), blank)
	z.markList(hitHarnessRow, l.drawn, l.width, len(block))
	return lipgloss.JoinVertical(lipgloss.Left, block...)
}

// row draws one harness, budgeted the way a discobox row is: the columns are
// added in the order they matter and drop off the right end as the terminal
// narrows. The name never goes.
func (l *harnessList) row(st *styles, h console.Harness, i int) string {
	atCursor := i == l.cursor

	tail := ""
	addCol := func(text string, w int) {
		if l.width-lipgloss.Width(tail)-w < 20 {
			return
		}
		tail += padANSI(text, w)
	}

	def := ""
	if h.Default {
		def = st.chipOn.Render("★ default")
	}
	addCol("  "+def, 13)

	kind := ""
	if h.BuiltIn {
		kind = st.dimText.Render("built-in")
	}
	addCol(kind, 10)

	secrets := ""
	if n := len(h.Secrets); n > 0 {
		secrets = st.dimText.Render(plural(n, "secret", "secrets"))
	}
	addCol(secrets, 11)

	files := ""
	if n := len(h.Files); n > 0 {
		files = st.dimText.Render(plural(n, "file", "files"))
	}
	addCol(files, 9)

	addCol(st.dimText.Render(pad(harnessAge(h, l.now()), 8)), 9)
	// The bundle is what the harness actually runs, and the longest thing on
	// the row, so it goes last: it is the first column a narrow terminal gives
	// up.
	addCol(st.dimText.Render(truncate(h.Bundle, 34)), 34)

	marker := "  "
	if atCursor {
		marker = st.key.Render("❯") + " "
	}
	head := marker
	name := h.DisplayName()
	nameW := max(l.width-lipgloss.Width(head)-lipgloss.Width(tail), 4)
	nameStyle := st.name
	if atCursor {
		nameStyle = st.cursorName
	}

	line := padANSI(head+padANSI(nameStyle.Render(truncate(name, nameW)), nameW)+tail, l.width)
	if atCursor {
		return highlight(st, line, colHighlightBG)
	}
	return line
}

// harnessAge is how long ago the harness was last changed, in the discobox
// list's own spelling.
func harnessAge(h console.Harness, now time.Time) string {
	return console.Ago(h.Updated, now)
}

// ---------------------------------------------------------------------------
// messages

// harnessesLoadedMsg is the listing, which is read at startup for the run
// options as well as for this screen.
type harnessesLoadedMsg struct {
	// server is the server the listing was read from, so one that lands after
	// the header has moved to another server is not drawn under its name.
	server    string
	selectID  string
	harnesses []console.Harness
	err       error
}

// harnessVerbMsg is a confirmed verb on its way back to the live model.
type harnessVerbMsg struct {
	verb    console.HarnessVerb
	harness console.Harness
}

// harnessFileMsg is the file chosen out of the file picker, on its way back for
// the same reason.
type harnessFileMsg struct {
	harness console.Harness
	path    string
}

// harnessDoneMsg reports an action that ran and returned, whichever kind it
// was.
type harnessDoneMsg struct {
	text string
	err  error
}

// harnessCardMsg is the config card, once the secret bindings behind it have
// arrived.
type harnessCardMsg struct {
	title string
	body  string
	err   error
}

// ---------------------------------------------------------------------------
// the screen

func (m *Model) loadHarnesses() tea.Cmd {
	server := m.configServer()
	return func() tea.Msg {
		harnesses, err := m.ds.Harnesses(m.ctx, server)
		return harnessesLoadedMsg{server: server, harnesses: harnesses, err: err}
	}
}

// harnessesLoaded takes a listing. It feeds the screen and the run options
// both: what the panel offers as a harness to run is what this reports, so a
// harness registered while the window is up is selectable without reopening it.
func (m *Model) harnessesLoaded(msg harnessesLoadedMsg) tea.Cmd {
	if m.session.ServerName(msg.server) != m.configServer() {
		// Read for a server the header has since moved off. The one it is on
		// now was asked for when it moved (configServerChanged).
		return nil
	}
	m.harnessesAnswered = true
	if msg.err != nil {
		// A run waiting on this listing goes anyway: what the questions here
		// would have caught, the server catches, and a run held forever behind
		// a listing that will not come is worse than the refusal.
		return tea.Batch(m.report(true, "cannot list the harnesses: %v", msg.err), m.startPendingRun())
	}
	m.harnesses.setAll(msg.harnesses)
	if msg.selectID != "" {
		for i, h := range msg.harnesses {
			if h.ID == msg.selectID {
				m.harnesses.moveTo(i)
				break
			}
		}
	}
	m.opts.SetHarnesses(msg.harnesses)
	m.layout()
	// `discobox run` handed its request to this window, and this is the first
	// moment the window can ask what it asks before a create. See pendingRun.
	return m.startPendingRun()
}

// openHarnesses brings the screen up.
func (m *Model) openHarnesses() tea.Cmd {
	m.commands.Use("harnesses")
	m.harnessesOpen = true
	m.optionsOpen = false
	// The secrets screen is this one's peer, not what it is drawn over: one of
	// them is the window while it is up, and leaving it goes back to the
	// launcher rather than to the other one still standing behind it.
	m.secretsOpen = false
	m.layout()
	return m.loadHarnesses()
}

func (m *Model) closeHarnesses() {
	m.harnessesOpen = false
	m.layout()
}

// updateHarnesses handles the screen. A letter is a command here, the way it is
// in the discobox list: there is no text to type.
func (m *Model) updateHarnesses(msg tea.KeyPressMsg) tea.Cmd {
	switch keyName(msg) {
	case "esc", "q", "tab":
		m.closeHarnesses()
		return nil
	case "down", "j":
		m.harnesses.move(1)
	case "up", "k":
		m.harnesses.move(-1)
	case "pgdown":
		m.harnesses.move(m.harnesses.height)
	case "pgup":
		m.harnesses.move(-m.harnesses.height)
	case "home", "g":
		m.harnesses.moveTo(0)
	case "end", "G":
		m.harnesses.moveTo(len(m.harnesses.all) - 1)
	case "?":
		m.dialog = m.helpDialog()
	case "left", "right":
		// The header's server, which is what this screen is showing: the
		// keys the control itself answers to when the list has focus.
		return m.cycleServer(serverStep(msg))
	case ".":
		return m.harnessMenu()
	case "enter":
		return m.harnessAct("v")
	default:
		if harness := m.harnesses.current(); harness != nil {
			for _, a := range harnessActions(*harness) {
				if a.key == keyName(msg) {
					return m.harnessAct(a.key)
				}
			}
		}
	}
	return nil
}

// harnessMenu is every action the harness under the cursor can take, as a menu
// — what `.` opens on the discobox list, and what the right button opens on a
// row here. A screen whose actions are only reachable by knowing their letters
// is one the pointer cannot work.
func (m *Model) harnessMenu() tea.Cmd {
	harness := m.harnesses.current()
	if harness == nil {
		return status("no harnesses to act on")
	}
	m.dialog = actionsDialog(harness.DisplayName(), "", harnessActions(*harness),
		func(key string) tea.Cmd { return m.harnessAct(key) })
	return nil
}

// harnessKeys is the letter each of the core's harness actions is pressed
// with here: the actions and whether each applies are the core's
// (console.Harness.Actions), their keys this screen's.
var harnessKeys = map[console.HarnessAction]string{
	console.HarnessActionVariant: "c",
	console.HarnessActionDefault: "s",
	console.HarnessActionConfig:  "v",
	console.HarnessActionFiles:   "f",
	console.HarnessActionReset:   "r",
}

// resetKeys is the letter each of the core's reset choices is pressed with.
var resetKeys = map[console.HarnessVerb]string{
	console.HarnessReset:          "r",
	console.HarnessResetOverrides: "a",
}

// harnessActions is what can be done to one harness, as the core offers it,
// each on its letter. An action that does not apply stays on the menu with
// the reason, the way the discobox menu keeps upgrade.
func harnessActions(h console.Harness) []action {
	offers := h.Actions()
	out := make([]action, 0, len(offers))
	for _, o := range offers {
		key := harnessKeys[o.Action]
		out = append(out, action{key: key, press: key, label: o.Label, detail: o.Detail, enabled: o.Enabled, why: o.Why})
	}
	return out
}

// harnessAct runs an action against the harness under the cursor.
func (m *Model) harnessAct(key string) tea.Cmd {
	current := m.harnesses.current()
	if current == nil {
		return status("no harnesses to act on")
	}
	harness := *current

	var chosen *action
	for _, a := range harnessActions(harness) {
		if a.key == key {
			chosen = &a
			break
		}
	}
	if chosen == nil {
		return nil
	}
	if !chosen.enabled {
		m.dialog = errorDialog("Cannot "+chosen.label+" "+harness.DisplayName(), chosen.why)
		return nil
	}

	switch key {
	case "c":
		return m.newHarnessVariantForm(harness)
	case "r":
		// What a reset asks, and which verb each answer is, are the core's;
		// a choice's letter is this screen's.
		q := harness.ResetQuestion()
		reset := func(answer string) tea.Cmd {
			verb, ok := harness.ResetVerb(answer)
			if !ok {
				return nil
			}
			return func() tea.Msg { return harnessVerbMsg{verb: verb, harness: harness} }
		}
		if len(q.Choices) == 0 {
			m.dialog = questionDialog(q, reset)
			return nil
		}
		items := make([]action, 0, len(q.Choices))
		for _, c := range q.Choices {
			key := resetKeys[console.HarnessVerb(c.Key)]
			items = append(items, action{key: key, press: key, label: c.Label, detail: c.Hint, enabled: true})
		}
		m.dialog = actionsDialog(q.Title, q.Body, items, func(key string) tea.Cmd {
			for verb, k := range resetKeys {
				if k == key {
					return reset(string(verb))
				}
			}
			return nil
		})
		return nil
	case "s":
		return m.runHarnessVerb(console.HarnessSetDefault, harness)
	case "v":
		return m.showHarnessCard(harness)
	case "f":
		m.dialog = m.harnessFilesDialog(harness)
		return nil
	}
	return nil
}

type harnessVariantCreatedMsg struct {
	server     string
	harness    console.Harness
	form, wait *dialog
	err        error
}

func (m *Model) newHarnessVariantForm(parent console.Harness) tea.Cmd {
	server := m.configServer()
	var d *dialog
	d = formDialog("Create variant", console.HarnessVariantForm(parent), func(variant console.NewHarnessVariant) tea.Cmd {
		return m.createHarnessVariant(server, variant, d)
	})
	d.body = "Follows the parent's bundle updates. Starts with its own account: saved logins, secret bindings and personal files are not copied."
	d.keys = []hint{says("↑↓ moves"), pressing("enter creates", "enter"), pressing("esc cancels", "esc")}
	m.dialog = d
	return nil
}

func (m *Model) createHarnessVariant(server string, variant console.NewHarnessVariant, form *dialog) tea.Cmd {
	wait := statusDialog("Create variant", "creating…")
	m.dialog = wait
	return func() tea.Msg {
		created, err := m.ds.CreateHarnessVariant(m.ctx, server, variant)
		return harnessVariantCreatedMsg{server: server, harness: created, form: form, wait: wait, err: err}
	}
}

func (m *Model) harnessVariantCreated(msg harnessVariantCreatedMsg) tea.Cmd {
	waiting := m.dialog == msg.wait
	if msg.err != nil {
		if waiting && m.session.ServerName(msg.server) == m.configServer() {
			msg.form.form.err = fmt.Sprintf("not created: %v", msg.err)
			m.dialog = msg.form
			return nil
		}
		return m.report(true, "variant: %v", msg.err)
	}
	if waiting {
		m.dialog = nil
	}
	if m.session.ServerName(msg.server) != m.configServer() {
		return nil
	}
	return tea.Batch(m.report(false, "created variant %q", msg.harness.Name), func() tea.Msg {
		harnesses, err := m.ds.Harnesses(m.ctx, msg.server)
		return harnessesLoadedMsg{server: msg.server, harnesses: harnesses, selectID: msg.harness.ID, err: err}
	})
}

// runHarnessVerb sends one verb to the API. Nothing takes the terminal, so the
// screen stays up and reports on its own status line.
func (m *Model) runHarnessVerb(verb console.HarnessVerb, harness console.Harness) tea.Cmd {
	name := harness.DisplayName()
	m.busy = string(verb) + " " + name + "…"
	server := m.configServer()
	return func() tea.Msg {
		// The core reads the harness again and refuses what its menu would.
		note, err := console.RunHarnessVerb(m.ctx, m.ds, server, harness.ID, verb)
		return harnessDoneMsg{text: note, err: err}
	}
}

// editHarnessFile has one of the harness's files edited and saved back. The
// data source writes it out and asks for it to be edited on its narration,
// which hands the terminal to $EDITOR on it (editInTerminal).
func (m *Model) editHarnessFile(harness console.Harness, path string) tea.Cmd {
	feed, next := m.narrate()
	m.busy = "editing " + path + "…"
	server, ctx, ds := m.configServer(), m.ctx, m.ds
	edit := func() tea.Msg {
		defer feed.Close()
		note, err := console.EditHarness(ctx, ds, server, harness.ID, path, feed.Narration)
		return harnessDoneMsg{text: note, err: err}
	}
	// The edit waits on the person in their editor: a listener, not a step.
	return tea.Batch(listen(edit), next)
}

// editInTerminal hands the terminal to $EDITOR on a file a running command
// asked to have edited, and tells it when the editor exits.
func (m *Model) editInTerminal(editing *console.Editing) tea.Cmd {
	exec := &harnessExec{
		title: editing.Title,
		ctx:   m.ctx,
		run: func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
			command, err := editorCommand(ctx, editing.Path)
			if err != nil {
				return err
			}
			command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
			return command.Run()
		},
	}
	return m.exec(exec, func(err error) tea.Msg {
		editing.Done(err)
		return nil
	})
}

// harnessDone takes an action's outcome, whichever kind it was, and re-reads
// the listing: every one of them changes what the rows say.
func (m *Model) harnessDone(msg harnessDoneMsg) tea.Cmd {
	m.busy = ""
	if msg.err != nil {
		return tea.Batch(m.loadHarnesses(), m.report(true, "%v", msg.err))
	}
	return tea.Batch(m.loadHarnesses(), m.refresh(), m.report(false, "%s", msg.text))
}

// harnessFilesDialog is the file picker: the harness's files, the ones saved
// from discoboxes first, since those are the ones worth editing by hand.
func (m *Model) harnessFilesDialog(harness console.Harness) *dialog {
	items := make([]action, 0, len(harness.Files))
	for i, file := range harness.Files {
		n := itoa(i + 1)
		items = append(items, action{
			// The key is the row's index, so the first nine files can be picked
			// by number as well as by moving to them.
			key:     n,
			press:   n,
			label:   file.Path,
			detail:  harnessFileDetail(file),
			enabled: true,
		})
	}
	files := harness.Files
	menu := actionsDialog("Files — "+harness.DisplayName(), "", items, func(key string) tea.Cmd {
		for i, file := range files {
			if itoa(i+1) == key {
				path := file.Path
				return func() tea.Msg { return harnessFileMsg{harness: harness, path: path} }
			}
		}
		return nil
	})
	menu.keys = []hint{pressing("Enter opens it in $EDITOR", "enter"), pressing("Esc cancels", "esc")}
	return menu
}

// harnessFileDetail is a file as the picker and the card describe it: its
// size, then what the core says of it.
func harnessFileDetail(file console.HarnessFile) string {
	return strings.Join(append([]string{console.Bytes(int64(len(file.Content)))}, file.Notes()...), " · ")
}

// showHarnessCard opens the config card. The bindings behind it are a request
// of their own, so the card is built when it is asked for rather than kept up
// to date for every row.
func (m *Model) showHarnessCard(harness console.Harness) tea.Cmd {
	m.busy = "reading " + harness.DisplayName() + "…"
	st, server := m.st, m.configServer()
	return func() tea.Msg {
		secrets, err := m.ds.HarnessSecrets(m.ctx, server, harness.ID)
		if err != nil {
			return harnessCardMsg{err: err}
		}
		return harnessCardMsg{title: harness.DisplayName(), body: harnessCard(st, harness, secrets)}
	}
}

// harnessCardLabel is the width of the card's label column, which every value
// on it is lined up against.
const harnessCardLabel = 13

// harnessCard is everything the harness is set to, as one readable block: the
// core's card (console.HarnessCard), each row's label lined up, its value in
// its tones, and a file's contents one step in under its path.
//
// Nothing here pads inside a style: the card goes into the modal layer, which
// wraps lines too long for it, and a padded label that got wrapped would leave
// its spaces behind on the previous line.
func harnessCard(st *styles, h console.Harness, secrets []console.HarnessSecret) string {
	label := func(text string) string {
		return "  " + st.dimText.Render(text) + strings.Repeat(" ", max(harnessCardLabel-2-len(text), 1))
	}
	// A file's contents sit one step in from the path they belong to, so a card
	// with several files reads as a list of files rather than as one block.
	indent := strings.Repeat(" ", harnessCardLabel+2)
	var b strings.Builder
	for _, row := range console.HarnessCard(h, secrets) {
		var value strings.Builder
		for _, span := range row.Spans {
			switch span.Tone {
			case console.CardQuiet:
				value.WriteString(st.dimText.Render(span.Text))
			case console.CardStandout:
				value.WriteString(st.chipOn.Render(span.Text))
			case console.CardCommand:
				value.WriteString(st.command.Render(span.Text))
			default:
				value.WriteString(span.Text)
			}
		}
		fmt.Fprintln(&b, label(row.Label), value.String())
		for _, line := range row.Lines {
			fmt.Fprintln(&b, indent+st.dimText.Render(line))
		}
		if row.More > 0 {
			fmt.Fprintln(&b, indent+st.dimText.Render(fmt.Sprintf("… %d more lines — %s opens it in your editor", row.More, harnessKeys[console.HarnessActionFiles])))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---------------------------------------------------------------------------
// view

// harnessesChrome is what the screen costs in rows before a single harness is
// drawn: the box's two edges, the header and the blank under it, the title bar,
// the blank below the rows, and the message and keys rows.
const harnessesChrome = 8

func (m *Model) viewHarnesses() string {
	m.zones.push(bodyLeft+m.logoColumn(), m.pageBodyTop())
	body := m.harnesses.view(m.st, &m.zones)
	m.zones.pop()
	if m.showLogo() {
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.logo.view(lipgloss.Height(body)), body)
	}
	if m.treeShown() {
		return m.pageFrame(strings.Split(body, "\n"))
	}

	m.zones.push(bodyLeft, headerTop)
	rows := []string{m.viewHeader(m.inner()), ""}
	m.zones.pop()
	rows = append(rows, strings.Split(body, "\n")...)

	m.zones.push(bodyLeft, headerTop+len(rows))
	rows = append(rows, strings.Split(m.viewStatus(), "\n")...)
	m.zones.pop()
	return m.box(rows)
}

// harnessHints is the bottom line here: only the actions the harness under the
// cursor can actually take, the way the discobox list does it.
func (m *Model) harnessHints() []hint {
	var out []hint
	if harness := m.harnesses.current(); harness != nil {
		for _, a := range harnessActions(*harness) {
			if a.enabled {
				out = append(out, keyed(a.key, a.key, a.label))
			}
		}
	}
	if m.manyServers() {
		out = append(out, says("←→ server"))
	}
	return append(out, pressing("Esc back", "esc"))
}

// ---------------------------------------------------------------------------
// exec

// harnessExec hands the terminal to an action that needs a real one: $EDITOR
// on one of a harness's files, and $EDITOR on a tool's config (tools.go).
//
// Bubble Tea releases the terminal — the alternate screen, raw mode, its own
// input reader — for as long as the action runs and takes it back when it
// returns, so both run on the screen the window was started from and the window
// comes back over the top of them.
type harnessExec struct {
	// title names what is taking the terminal, printed at the top of the
	// screen this clears.
	title string
	// ctx is the window's, and the parent of the one the action is given.
	ctx context.Context
	run func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error

	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func (c *harnessExec) SetStdin(r io.Reader)  { c.stdin = r }
func (c *harnessExec) SetStdout(w io.Writer) { c.stdout = w }
func (c *harnessExec) SetStderr(w io.Writer) { c.stderr = w }

// Run gives the action a screen of its own.
//
// tea.Exec puts the window back on the primary screen and steps aside, and the
// primary screen still holds whatever was on it before the window opened — the
// shell prompt, the command line, whatever the CLI printed on its way here. The
// action then draws into the middle of all that, which reads as two screens
// drawn over each other. Clearing first is what makes the handover
// look like one.
func (c *harnessExec) Run() error {
	clearScreen(c.stdout, c.title)
	// And ^C has to work while it does.
	//
	// Bubble Tea keeps its SIGINT handler installed while it steps aside —
	// ReleaseTerminal only sets ignoreSignals, so the signal is still delivered
	// to it and then dropped. A registered handler is also what turns off Go's
	// default terminate-on-interrupt. So without this nothing is listening,
	// and ^C during the action does nothing at all.
	//
	// Notifying here puts a second listener on the same signal, which Go
	// delivers to both, and this one cancels the action.
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return c.run(ctx, c.stdin, c.stdout, c.stderr)
}

// clearScreen wipes the terminal, its scrollback, and homes the cursor, then
// names what is about to use it.
//
// The scrollback goes too (\x1b[3J). Without it the cleared rows are still
// there to scroll back into, and what a user finds above a fresh action is the
// window that was supposed to have stepped aside.
func clearScreen(out io.Writer, title string) {
	if out == nil {
		return
	}
	fmt.Fprint(out, "\x1b[H\x1b[2J\x1b[3J")
	if title != "" {
		fmt.Fprintf(out, "%s\r\n\r\n", title)
	}
}
