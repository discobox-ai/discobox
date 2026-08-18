package tui

import (
	"fmt"
	"io"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The harnesses screen is the project's harnesses and everything you do to
// them: enable one — which runs the harness's own setup — disable it, make it
// the project default, read what it is set to, and edit the files it carries.
//
// It is `disco configure`, folded into the window. That command is now this
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
const harnessesKey = "f3"

// HarnessesKeyName is how that key is spelled to the user. It is exported
// because `disco configure` opens the window on this screen and says so in its
// help, and a key named in two places is a key that ends up spelled two ways.
const HarnessesKeyName = "F3"

// harnessList is the screen's list: the harnesses, and where the cursor is in
// them.
type harnessList struct {
	all    []Harness
	cursor int
	offset int

	width, height int

	// now is when the frame is being drawn, so the age column is a pure
	// function of the model and a test can render a fixed one.
	now func() time.Time
}

func newHarnessList() *harnessList { return &harnessList{now: time.Now} }

// setAll takes a refreshed listing, keeping the cursor on the harness it was on
// rather than on the row number it was at: enabling one can reorder nothing
// today, but a cursor that follows the harness is the cursor that acts on the
// one you were looking at.
func (l *harnessList) setAll(all []Harness) {
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

func (l *harnessList) current() *Harness {
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

// enabled is how many harnesses can actually be run, which is what the title
// bar says: a project full of harnesses none of which are set up is the case
// the screen exists to fix.
func (l *harnessList) enabled() int {
	n := 0
	for _, h := range l.all {
		if h.State == HarnessEnabled {
			n++
		}
	}
	return n
}

func harnessDot(h Harness) string {
	switch h.State {
	case HarnessEnabled:
		return "●"
	case HarnessFailed:
		return "✗"
	default:
		return "○"
	}
}

func harnessStateStyle(st *styles, h Harness) lipgloss.Style {
	switch h.State {
	case HarnessEnabled:
		return st.stateRun
	case HarnessFailed:
		return st.stateErr
	default:
		return st.stateOff
	}
}

func (l *harnessList) view(st *styles) string {
	right := plural(len(l.all), "harness", "harnesses")
	if n := l.enabled(); n != len(l.all) {
		right = itoa(n) + " enabled  ·  " + right
	}
	blank := strings.Repeat(" ", max(l.width, 0))
	out := []string{renderTitle(st.titleList, "Harnesses", right, l.width)}

	body := make([]string, 0, l.height)
	if len(l.all) == 0 {
		body = append(body, st.dimText.Render(pad("  no harnesses in this project — register one with `disco box harnesses create`", l.width)))
	}
	for i := l.offset; i < len(l.all) && len(body) < l.height; i++ {
		body = append(body, l.row(st, l.all[i], i))
	}
	for len(body) < l.height {
		body = append(body, blank)
	}
	return lipgloss.JoinVertical(lipgloss.Left, append(append(out, body...), blank)...)
}

// row draws one harness, budgeted the way a discobox row is: the columns are
// added in the order they matter and drop off the right end as the terminal
// narrows. The name and the state never go.
func (l *harnessList) row(st *styles, h Harness, i int) string {
	atCursor := i == l.cursor
	glyph := st.color

	tail := ""
	addCol := func(text string, w int) {
		if l.width-lipgloss.Width(tail)-w < 20 {
			return
		}
		tail += padANSI(text, w)
	}

	// Half of what the glyph carries is its color, so without one the state
	// comes back as a word, exactly as it does in the discobox list.
	if !glyph {
		addCol("  "+pad(string(h.State), 8), 10)
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

	addCol(st.dimText.Render(pad(harnessAge(h, l.now()), 7)), 8)
	// The image is what the harness actually runs, and the longest thing on the
	// row, so it goes last: it is the first column a narrow terminal gives up.
	addCol(st.dimText.Render(truncate(h.Image, 34)), 34)

	marker := "  "
	if atCursor {
		marker = st.key.Render("❯") + " "
	}
	head := marker
	if glyph {
		head += harnessStateStyle(st, h).Render(harnessDot(h)) + " "
	}

	// A failed harness's row says why under the cursor. It takes the whole row to
	// say it: the columns beside the name describe a harness that works, and on
	// one that does not the reason is worth more than all of them.
	name := h.displayName()
	if h.State == HarnessFailed && h.Error != "" && atCursor {
		name, tail = h.Error, ""
	}
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
// list's own spelling. A harness nothing has ever configured has nothing to
// say.
func harnessAge(h Harness, now time.Time) string {
	age := since(h.Updated, now)
	if age == "" {
		return ""
	}
	return age + " ago"
}

// ---------------------------------------------------------------------------
// messages

// harnessesLoadedMsg is the listing, which is read at startup for the run
// options as well as for this screen.
type harnessesLoadedMsg struct {
	harnesses []Harness
	err       error
}

// harnessSetupMsg carries a confirmed "set it up" back to the live model. The
// prompt asks it when a run names a harness that has never been through its
// setup; a dialog closed over the model by value and cannot run anything
// against it.
type harnessSetupMsg struct{ harness Harness }

// harnessVerbMsg is a confirmed verb on its way back to the live model.
type harnessVerbMsg struct {
	verb    HarnessVerb
	harness Harness
}

// harnessFileMsg is the file chosen out of the file picker, on its way back for
// the same reason.
type harnessFileMsg struct {
	harness Harness
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
	return func() tea.Msg {
		harnesses, err := m.ds.Harnesses(m.ctx)
		return harnessesLoadedMsg{harnesses: harnesses, err: err}
	}
}

// harnessesLoaded takes a listing. It feeds the screen and the run options
// both: what the panel offers as a harness to run is what this reports, so
// enabling one makes it selectable without the window being reopened.
func (m *Model) harnessesLoaded(msg harnessesLoadedMsg) tea.Cmd {
	if msg.err != nil {
		return m.report(true, "cannot list the harnesses: %v", msg.err)
	}
	m.harnesses.setAll(msg.harnesses)
	m.opts.setHarnesses(msg.harnesses)
	m.layout()
	return nil
}

// openHarnesses brings the screen up. It opens the window out for the same
// reason a terminal does: the screen is the whole window, and the opening
// prompt is not a place to put one.
func (m *Model) openHarnesses() tea.Cmd {
	m.expand()
	m.harnessesOpen = true
	m.optionsOpen = false
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
		m.dialog = textDialog("Keys", m.helpText())
	case "enter":
		return m.harnessAct("e")
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

// harnessActions is what can be done to one harness, filtered against what it
// can actually take. An action that does not apply stays on the menu with the
// reason, the way the discobox menu keeps upgrade.
func harnessActions(h Harness) []action {
	enable, detail := "enable", "run the harness's own setup, in this terminal"
	if h.State != HarnessDisabled {
		enable, detail = "reconfigure", "run its setup again, with what it has now"
	}
	defaultWhy := "enable it first — only a working harness can be the default"
	if h.Default {
		defaultWhy = "already the default"
	}
	// A harness whose image declares no setup — `shell` is the one that ships
	// that way — has nothing to enable and nothing to turn off. The server
	// refuses both, and disabling it would be a door that only opens one way,
	// so neither is offered rather than offered and rejected.
	nothingToSetUp := "it needs no setup — there is nothing to run"
	return []action{
		{key: "e", label: enable, detail: detail, enabled: h.Configurable,
			why: nothingToSetUp},
		{key: "d", label: "disable", detail: "delete its secrets and configuration",
			enabled: h.Configurable && h.State == HarnessEnabled,
			why:     disableWhy(h, nothingToSetUp)},
		{key: "s", label: "default", detail: "run it when a discobox says no harness", enabled: h.State == HarnessEnabled && !h.Default,
			why: defaultWhy},
		{key: "v", label: "config", detail: "everything the harness is set to", enabled: true},
		{key: "f", label: "files", detail: "edit one of its files in $EDITOR", enabled: len(h.Files) > 0,
			why: "it carries no files"},
	}
}

// disableWhy says which of the two reasons disable does not apply for.
func disableWhy(h Harness, nothingToSetUp string) string {
	if !h.Configurable {
		return nothingToSetUp
	}
	return "not enabled, so there is nothing to take away"
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
		m.dialog = errorDialog("Cannot "+chosen.label+" "+harness.displayName(), chosen.why)
		return nil
	}

	switch key {
	case "e":
		return m.configureHarness(harness)
	case "d":
		// Disabling runs the harness's deconfigure flow, which deletes the secrets
		// and files its setup created. Archiving a discobox is reversible and
		// asks nothing; this is not, so it asks.
		question := fmt.Sprintf("Disable %s? The secrets and configuration its setup created are deleted, and enabling it again means answering its questions again.",
			harness.displayName())
		if harness.Default {
			question += " It is the project default, which is released first."
		}
		m.dialog = confirmDialog("Disable", question, func(string) tea.Cmd {
			return func() tea.Msg { return harnessVerbMsg{verb: HarnessDisable, harness: harness} }
		})
		return nil
	case "s":
		return m.runHarnessVerb(HarnessSetDefault, harness)
	case "v":
		return m.showHarnessCard(harness)
	case "f":
		m.dialog = m.harnessFilesDialog(harness)
		return nil
	}
	return nil
}

// runHarnessVerb sends one verb to the API. Nothing takes the terminal, so the
// screen stays up and reports on its own status line.
func (m *Model) runHarnessVerb(verb HarnessVerb, harness Harness) tea.Cmd {
	name := harness.displayName()
	m.busy = string(verb) + " " + name + "…"
	return func() tea.Msg {
		if err := m.ds.DoHarness(m.ctx, verb, harness.ID); err != nil {
			return harnessDoneMsg{err: fmt.Errorf("%s %s: %w", verb, name, err)}
		}
		return harnessDoneMsg{text: verb.done(name)}
	}
}

// configureHarness hands the terminal to the harness's own setup.
//
// It is a program that asks questions — a login, a device code, a key pasted in
// — and draws its own screen to ask them on, so the window steps aside for it
// exactly as it does for apply, rather than trying to draw it in a pane. The
// flow itself is the CLI's, on the far side of the data seam.
func (m *Model) configureHarness(harness Harness) tea.Cmd {
	name := harness.displayName()
	m.busy = "configuring " + name + "…"
	exec := &harnessExec{run: func(stdin io.Reader, stdout, stderr io.Writer) error {
		return m.ds.ConfigureHarness(m.ctx, harness.ID, stdin, stdout, stderr)
	}}
	return m.exec(exec, func(err error) tea.Msg {
		if err != nil {
			return harnessDoneMsg{err: fmt.Errorf("configure %s: %w", name, err)}
		}
		return harnessDoneMsg{text: "configured " + name}
	})
}

// editHarnessFile hands the terminal to $EDITOR on one of the harness's files,
// and saves back what it wrote.
func (m *Model) editHarnessFile(harness Harness, path string) tea.Cmd {
	m.busy = "editing " + path + "…"
	var changed bool
	exec := &harnessExec{run: func(stdin io.Reader, stdout, stderr io.Writer) error {
		var err error
		changed, err = m.ds.EditHarnessFile(m.ctx, harness.ID, path, stdin, stdout, stderr)
		return err
	}}
	return m.exec(exec, func(err error) tea.Msg {
		switch {
		case err != nil:
			return harnessDoneMsg{err: fmt.Errorf("edit %s: %w", path, err)}
		case !changed:
			return harnessDoneMsg{text: path + " unchanged"}
		default:
			return harnessDoneMsg{text: "updated " + path}
		}
	})
}

// harnessDone takes an action's outcome, whichever kind it was, and re-reads
// the listing: every one of them changes what the rows say.
func (m *Model) harnessDone(msg harnessDoneMsg) tea.Cmd {
	m.busy = ""
	if msg.err != nil {
		return tea.Batch(m.loadHarnesses(), m.report(true, "%v", msg.err))
	}
	return tea.Batch(m.loadHarnesses(), m.report(false, "%s", msg.text))
}

// harnessFilesDialog is the file picker: the harness's files, the ones its
// setup wrote first, since those are the ones worth editing by hand.
func (m *Model) harnessFilesDialog(harness Harness) *dialog {
	items := make([]action, 0, len(harness.Files))
	for i, file := range harness.Files {
		items = append(items, action{
			// The key is the row's index, so the first nine files can be picked
			// by number as well as by moving to them.
			key:     itoa(i + 1),
			label:   file.Path,
			detail:  harnessFileDetail(file),
			enabled: true,
		})
	}
	files := harness.Files
	menu := actionsDialog("Files — "+harness.displayName(), "", items, func(key string) tea.Cmd {
		for i, file := range files {
			if itoa(i+1) == key {
				path := file.Path
				return func() tea.Msg { return harnessFileMsg{harness: harness, path: path} }
			}
		}
		return nil
	})
	menu.footer = "Enter opens it in $EDITOR · Esc cancels"
	return menu
}

func harnessFileDetail(file HarnessFile) string {
	notes := []string{humanBytes(int64(len(file.Content)))}
	if file.Configured {
		notes = append(notes, "written by its setup")
	} else {
		notes = append(notes, "from the image")
	}
	if file.CreateOnly {
		notes = append(notes, "create-only")
	}
	if file.Template {
		notes = append(notes, "template")
	}
	return strings.Join(notes, " · ")
}

// showHarnessCard opens the config card. The bindings behind it are a request
// of their own, so the card is built when it is asked for rather than kept up
// to date for every row.
func (m *Model) showHarnessCard(harness Harness) tea.Cmd {
	m.busy = "reading " + harness.displayName() + "…"
	st := m.st
	return func() tea.Msg {
		secrets, err := m.ds.HarnessSecrets(m.ctx, harness.ID)
		if err != nil {
			return harnessCardMsg{err: err}
		}
		return harnessCardMsg{title: harness.displayName(), body: harnessCard(st, harness, secrets)}
	}
}

// harnessCardFileLines is how much of a file the card shows before saying how
// much is left. A card is something to glance at; a file long enough to page
// through is one to open in the editor, which f does.
const harnessCardFileLines = 40

// harnessCardLabel is the width of the card's label column, which every value
// on it is lined up against.
const harnessCardLabel = 13

// harnessCard is everything the harness is set to, as one readable block: what
// it is, what it runs, which secret answers each variable it needs, and the
// files it carries.
//
// Nothing here pads inside a style: the card goes into the modal layer, which
// wraps lines too long for it, and a padded label that got wrapped would leave
// its spaces behind on the previous line.
func harnessCard(st *styles, h Harness, secrets []HarnessSecret) string {
	label := func(text string) string {
		return "  " + st.dimText.Render(text) + strings.Repeat(" ", max(harnessCardLabel-2-len(text), 1))
	}
	// A file's contents sit one step in from the path they belong to, so a card
	// with several files reads as a list of files rather than as one block.
	indent := strings.Repeat(" ", harnessCardLabel+2)
	var b strings.Builder

	state := harnessStateStyle(st, h).Render(string(h.State))
	if h.Default {
		state += st.chipOn.Render("  ★ default")
	}
	if h.BuiltIn {
		state += st.dimText.Render("  built-in")
	}
	fmt.Fprintln(&b, label("State"), state)
	if h.Slug != "" && h.Slug != h.displayName() {
		fmt.Fprintln(&b, label("Run as"), h.Slug)
	}
	fmt.Fprintln(&b, label("ID"), h.ID)
	if h.Error != "" {
		fmt.Fprintln(&b, label("Error"), st.statusER.Render(h.Error))
	}
	if h.Image != "" {
		fmt.Fprintln(&b, label("Image"), h.Image)
	}
	if h.Digest != "" {
		fmt.Fprintln(&b, label("Digest"), h.Digest)
	}
	if len(h.Run) > 0 {
		fmt.Fprintln(&b, label("Run"), st.command.Render(strings.Join(h.Run, " ")))
	}
	if len(h.Relaunch) > 0 {
		fmt.Fprintln(&b, label("Relaunch"), st.command.Render(strings.Join(h.Relaunch, " ")))
	}

	name := "Secrets"
	for _, secret := range secrets {
		fmt.Fprintln(&b, label(name), harnessSecretLine(st, secret))
		name = ""
	}
	name = "Files"
	for _, file := range h.Files {
		fmt.Fprintln(&b, label(name), file.Path+st.dimText.Render("  ("+harnessFileDetail(file)+")"))
		name = ""
		for _, line := range harnessFileLines(file) {
			fmt.Fprintln(&b, indent+st.dimText.Render(line))
		}
	}
	if !h.Updated.IsZero() {
		fmt.Fprintln(&b, label("Updated"), h.Updated.Format(time.RFC3339))
	}
	return strings.TrimRight(b.String(), "\n")
}

// harnessSecretLine is one environment variable and whatever answers it.
func harnessSecretLine(st *styles, secret HarnessSecret) string {
	var notes []string
	if secret.Required {
		notes = append(notes, "required")
	}
	if secret.OneOf != "" {
		notes = append(notes, "one of "+secret.OneOf)
	}
	if !secret.Declared {
		notes = append(notes, "bound by hand")
	}
	line := secret.Name
	if len(notes) > 0 {
		line += st.dimText.Render(" (" + strings.Join(notes, ", ") + ")")
	}
	if secret.SecretID == "" {
		return line + st.dimText.Render(" → nothing bound")
	}
	var about []string
	if secret.SecretType != "" {
		about = append(about, secret.SecretType)
	}
	if secret.SecretName != "" && secret.SecretName != secret.SecretID {
		about = append(about, secret.SecretName)
	}
	if secret.Anonymous {
		about = append(about, "anonymous")
	}
	line += " → " + secret.SecretID
	if len(about) > 0 {
		line += st.dimText.Render(" (" + strings.Join(about, ", ") + ")")
	}
	return line
}

// harnessFileLines is the file's contents as the card shows them: the first
// screenful, and how much was left.
func harnessFileLines(file HarnessFile) []string {
	if strings.TrimSpace(file.Content) == "" {
		return nil
	}
	lines := strings.Split(strings.TrimRight(file.Content, "\n"), "\n")
	if len(lines) <= harnessCardFileLines {
		return lines
	}
	out := append([]string(nil), lines[:harnessCardFileLines]...)
	return append(out, fmt.Sprintf("… %d more lines — f opens it in your editor", len(lines)-harnessCardFileLines))
}

// ---------------------------------------------------------------------------
// view

// harnessesChrome is what the screen costs in rows before a single harness is
// drawn: the box's two edges, the header and the blank under it, the title bar,
// the blank below the rows, and the status line.
const harnessesChrome = 7

func (m *Model) viewHarnesses() string {
	body := m.harnesses.view(m.st)
	if m.showLogo() {
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.logo.view(lipgloss.Height(body)), body)
	}
	rows := []string{m.viewHeader(m.inner()), ""}
	rows = append(rows, strings.Split(body, "\n")...)
	rows = append(rows, m.viewStatus())
	return m.box("", rows)
}

// harnessHints is the bottom line here: only the actions the harness under the
// cursor can actually take, the way the discobox list does it.
func (m *Model) harnessHints() string {
	var parts []string
	if harness := m.harnesses.current(); harness != nil {
		for _, a := range harnessActions(*harness) {
			if a.enabled {
				parts = append(parts, a.key+" "+a.label)
			}
		}
	}
	parts = append(parts, "Esc back")
	return strings.Join(parts, " · ")
}

// ---------------------------------------------------------------------------
// exec

// harnessExec hands the terminal to one of the two harness actions that need a
// real one: the harness's own setup, and $EDITOR on one of its files.
//
// Bubble Tea releases the terminal — the alternate screen, raw mode, its own
// input reader — for as long as the action runs and takes it back when it
// returns, so both run on the screen the window was started from and the window
// comes back over the top of them.
type harnessExec struct {
	run func(stdin io.Reader, stdout, stderr io.Writer) error

	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func (c *harnessExec) SetStdin(r io.Reader)  { c.stdin = r }
func (c *harnessExec) SetStdout(w io.Writer) { c.stdout = w }
func (c *harnessExec) SetStderr(w io.Writer) { c.stderr = w }

func (c *harnessExec) Run() error { return c.run(c.stdin, c.stdout, c.stderr) }
