package tui

import (
	"context"
	"io"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/discobox-ai/discobox/termpane"
)

// The tools are the programs you reach for beside the agent: a diff viewer, an
// editor, the IDE running on this machine. They are not harnesses — nothing
// runs them for you and nothing is pinned to them — and they are not shells,
// which is why they are not tabs.
//
// A tool that runs in the discobox is one exec session with the whole window,
// opened over the workspace the way apply is, and it is the only kind of pane
// that survives being put away: minimizing leaves the session running and the
// stream attached, and choosing the tool again shows the same screen where it
// was. Closing is the other thing entirely — it ends the session — which is why
// the two are different buttons.
//
// The session carries the tool's id as exec metadata, so the listing says which
// sessions are tools and which tool each one is. That is what lets a window
// that has never seen this discobox before — a second `discobox tui`, this one
// restarted — pick a running diff back up instead of drawing it as a stray
// shell. See ADR 0071.

const (
	// toolsKey opens the picker behind the leader. Not t, which is stop in the
	// key map the list and the workspace share, and not x, which is archive in
	// it; o is what the picker does — open one of them.
	toolsKey = "o"
	// toolCloseKey ends the tool that has the screen and the session behind
	// it. It is shifted for the reason repair is: the letter that only hides a
	// window and the letter that kills what is in it should not be one finger
	// apart, and this is the destructive one. It is bound only inside a tool
	// window, where the list's own x — archive — is not.
	toolCloseKey = "X"
	// toolFileKey opens the files of the tool the picker's cursor is on. It is
	// the picker's second action, on a key no tool answers to, because a row
	// there means "run this" and its files are the other thing you might want
	// from the same row.
	toolFileKey = "e"

	// addressSSHKey and addressGitKey copy the two ways into this discobox
	// that are not a program: the ssh command that opens a session in it, and
	// the git URL its working tree answers on. They sit in the picker because
	// the question is the picker's own — how do I get at this box — and the
	// answer for a shell and for git happens to be a line of text rather than
	// something to launch.
	addressSSHKey = "s"
	addressGitKey = "g"
)

// toolsTitle names the picker's card. It is a constant because a lookup that
// finishes while the card is up has to know whether the card still on screen
// is the one it was for.
const toolsTitle = "Tools"

// tool is one entry in the picker: what it is called, the key it answers to,
// and what running it means.
type tool struct {
	id     string
	key    string
	label  string
	detail string

	// command is what a tool session runs inside the discobox, in its primary
	// source directory. A tool with no command is not a session at all — vscode
	// is a program on this machine, handed the discobox and left to it — and is
	// run rather than opened.
	command []string

	// files are the configuration this tool carries into a discobox, kept on
	// this machine and copied in the first time the tool runs in a box that has
	// none. See ToolFile.
	files []ToolFile
}

// session reports whether this tool is a window in the discobox rather than a
// request that returns.
func (t tool) session() bool { return len(t.command) > 0 }

// spec is the part of a tool the outside needs to run it.
func (t tool) spec() ToolSpec {
	return ToolSpec{ID: t.id, Command: t.command, Files: t.files}
}

// tools is every tool, in the order the picker lists them.
//
// The two that run in the discobox come from its image: discobox-review and
// fresh are installed there, not here, so they are the same version for
// everyone looking at the same discobox and there is nothing to have on your
// machine. vscode is the opposite and is listed anyway: it is the same
// question — "open this discobox in X" — and a picker that answered it for two
// of the three would leave the third on a key you had to remember separately.
var tools = []tool{
	{
		id: "diff", key: "d", label: "diff",
		detail:  "what has changed, in discobox-review",
		command: []string{"discobox-review"},
	},
	{
		id: "fresh", key: "f", label: "fresh",
		detail: "the fresh editor, in the box",
		// The "." is not decoration. fresh opens the directory — with its file
		// tree, and the project as a workspace it can remember — only when it
		// is given exactly one argument and that argument is a directory;
		// with none it comes up on an empty [No Name] buffer and no tree,
		// however promising the working directory looked. The exec already
		// lands in the primary source directory, so "." is that directory, and
		// fresh makes it absolute before keying anything on it.
		command: []string{"fresh", "."},
		files:   []ToolFile{freshConfig, freshLiveDiff, freshTrust},
	},
	{
		id: "vscode", key: vscodeKey, label: "vscode",
		detail: "open the box in VS Code, in a window of its own",
	},
}

// freshConfig is the editor's own configuration, which is the one thing a tool
// so far needs carried into a discobox.
//
// The two names differ on purpose. fresh reads ~/.config/fresh/config.json, so
// that is where it has to land; the copy on this machine is config.jsonc,
// because that is what the contents actually are and because the extension is
// how every editor works out how to color it. A .json file full of comments is
// a screenful of syntax errors in vim, VS Code and everything else that keys off
// the name.
//
// The modeline covers the other copy — the one inside the discobox, which has to
// be called config.json and which you may well end up opening there. Vim ships
// a jsonc syntax (json5 and hjson get a filetype and no highlighting, so jsonc
// is the one to claim), reads modelines from the first five lines by default,
// and lets one override what the extension said. It is a comment before the
// top-level value, which is exactly what JSONC is for.
var freshConfig = ToolFile{
	Tool: "fresh",
	Name: "config.jsonc",
	Home: ".config/fresh/config.json",
	Default: `// vim: set ft=jsonc :
//
// fresh's configuration, as discobox carries it into a discobox.
//
// This file lives on your machine. It is copied to ~/.config/fresh/config.json
// the first time fresh runs in a discobox that has none, and never over one
// that is already there — so editing here changes what the next discobox gets,
// not what an open one is using.
//
// The format is JSONC: JSON, plus // comments and trailing commas.
// Everything that can go here: https://getfresh.dev/docs/configuration
{
  // A discobox is disposable and its fresh comes from the image, so there is
  // nothing for an upgrade check to usefully tell you. Turning it off takes
  // the anonymous telemetry with it.
  "check_for_updates": false,

  // "theme": "default",
  // "editor": {
  //   "tab_size": 4,
  //   "line_numbers": true,
  //   "line_wrap": false,
  // },
}
`,
}

// freshLiveDiff turns the live-diff plugin on, and pins what it diffs against.
//
// This is not configuration — fresh has no config key for it. It is plugin
// state, which fresh keeps per plugin under its data directory and reads at
// startup, so seeding the file is the same thing as having enabled the plugin
// in an earlier run. The plugin is opt-in and off until something says
// otherwise, which in a discobox nothing ever does.
//
// HEAD is already the plugin's own default for the diff base, and it is written
// out anyway: a default that is only a default moves when upstream changes its
// mind, and "against HEAD" is the whole point of the thing here — the agent has
// been editing the working tree and what you want to see is what it did.
//
// Strict JSON, unlike the config beside it: this file is parsed with serde_json
// and a comment in it would make the whole file unreadable — taking the enable
// with it, silently, since unparseable plugin state is skipped rather than
// reported.
var freshLiveDiff = ToolFile{
	Tool: "fresh",
	Name: "live_diff.json",
	Home: ".local/share/fresh/orchestrator/state/live_diff.json",
	Default: `{
  "live_diff.global_enabled": true,
  "live_diff.default_mode": { "kind": "head" }
}
`,
}

// freshTrust records that the discobox's own source directory is trusted.
//
// fresh gates repo-controlled execution per folder: a directory carrying
// anything that can run code — a package.json, a Cargo.toml, an .envrc — opens
// Restricted, which means no language servers and no environment activation
// until somebody says otherwise. The decision is remembered per folder, in the
// user's data directory rather than in the repository, because a repository
// must not be able to vouch for itself.
//
// Nothing in a discobox ever says otherwise: it is a new folder every time, so
// every discobox opens Restricted and stays there. Recording the decision up
// front is the discobox's whole premise stated in fresh's terms — the sandbox
// is the boundary, the agent already runs this repository's code inside it, and
// an editor refusing to start its language server is protecting the sandbox
// from the thing the sandbox exists to run.
//
// This is the one file whose destination is not fixed: fresh keys it on the
// working directory, which only the discobox knows. See installToolFileScript
// for what {workspace} is replaced with.
var freshTrust = ToolFile{
	Tool:    "fresh",
	Name:    "trust.json",
	Home:    ".local/share/fresh/workspaces/{workspace}/trust.json",
	Default: "{\n  \"level\": \"trusted\"\n}\n",
}

// toolByID and toolByKey find one, or report that there is none.
func toolByID(id string) (tool, bool) {
	for _, t := range tools {
		if t.id == id {
			return t, true
		}
	}
	return tool{}, false
}

// openToolsMsg is the leader plus the tools key: the picker, over whatever is
// on screen.
type openToolsMsg struct{}

// runToolMsg carries a choice out of the picker back to the live model. A
// dialog closes over the model by value, so it emits the choice rather than
// running it, the way the action menu does.
type runToolMsg struct{ id string }

// closeToolMsg is the leader plus the close key, or the [x] button: end the
// tool that has the screen, and the session behind it.
type closeToolMsg struct{}

// toolFilesMsg is the picker's second action on the highlighted row: show that
// tool's files. Like runToolMsg it is a message rather than a call, because a
// dialog closes over the model by value.
type toolFilesMsg struct{ id string }

// toolFileMsg is the file chosen out of that list, on its way to $EDITOR.
type toolFileMsg struct{ file ToolFile }

// addressesMsg is one discobox's ssh and git addresses, looked up.
type addressesMsg struct {
	id   string
	addr Addresses
	err  error
}

// copyAddressMsg is one of the picker's address rows, chosen. It is a message
// rather than a call for the same reason runToolMsg is: a dialog closes over
// the model by value, so it emits what was chosen and the live model acts.
type copyAddressMsg struct{ text string }

// toolFileDoneMsg is what came back from the editor.
type toolFileDoneMsg struct {
	file    ToolFile
	changed bool
	err     error
}

// toolTermMsg carries one connected tool session back to the model: a tool the
// picker asked for, or one the workspace's poll found already running.
type toolTermMsg struct {
	gen  int
	id   string
	exec Exec
	term Terminal
	err  error
	// show is whether this tool should take the screen when it arrives. The
	// picker's do; the ones the poll picks up arrive minimized, because
	// attaching to a discobox should show you the discobox.
	show bool
}

// openTools opens the picker, and starts the lookup its two address rows are
// waiting on. Every tool is offered whatever is running: the row says which are
// up, and choosing one is "show me that", not "start another".
func (m *Model) openTools() tea.Cmd {
	box := m.currentBox()
	// A receipt belongs to the press that earned it, not to the card: reopening
	// the picker must not greet a reader with a "copied" from last time.
	m.copied = ""
	// Before the card is built, so the rows are drawn against the lookup this
	// open started rather than against the state before it.
	fetch := m.resolveAddresses(box)
	m.dialog = m.toolsDialog(box)
	return fetch
}

// toolsDialog is the picker's card, built from what is known right now. It is
// separate from openTools because the addresses arrive after the card does, and
// the card is then built again rather than patched — see addressesResolved.
func (m *Model) toolsDialog(box Sandbox) *dialog {
	items := make([]action, 0, len(tools)+2)
	for _, t := range tools {
		detail := t.detail
		if m.toolPane(t.id) != nil {
			// Every tool pane is a running one: a tool that exits takes its
			// window with it (see paneClosed).
			detail = t.detail + " · running"
		}
		items = append(items, action{
			key: t.key, press: t.key, label: t.label, detail: detail,
			enabled: box.attachable(),
			why:     attachWhy(true, []Sandbox{box}),
		})
	}
	// An address is only worth printing for a discobox the config has a stanza
	// for, which is the same boxes the tools apply to: an archived one is out
	// of the listing the stanzas are rendered from.
	addr, unreachable := m.addresses[box.ID], attachWhy(true, []Sandbox{box})
	ssh := addr.action(addressSSHKey, "ssh", addr.SSH, unreachable)
	git := addr.action(addressGitKey, "git url", addr.Git, unreachable)
	ssh.note, git.note = m.copiedNote(addr.SSH), m.copiedNote(addr.Git)
	items = append(items, ssh, git)

	d := actionsDialog(toolsTitle, "Run a tool against "+displayName(box)+", or take an address to it.", items,
		func(key string) tea.Cmd {
			switch key {
			case addressSSHKey:
				return copyAddress(addr.SSH)
			case addressGitKey:
				return copyAddress(addr.Git)
			}
			for _, t := range tools {
				if t.key == key {
					return func() tea.Msg { return runToolMsg{id: t.id} }
				}
			}
			return nil
		})
	d.altKey = toolFileKey
	d.alt = func(key string) tea.Cmd {
		for _, t := range tools {
			if t.key == key {
				return func() tea.Msg { return toolFilesMsg{id: t.id} }
			}
		}
		return nil
	}
	d.footer = "ssh and git url copy rather than open. They are the whole of what it takes " +
		"from any shell on this machine — opening this refreshed the ssh config behind them."
	// The two address rows are the whole of what stays: a copy is done when the
	// word appears beside it, and taking the card away would take the word with
	// it. The tools are the other thing — running one is a window, and the card
	// has to get out of its way.
	d.keys = []hint{
		pressing("Enter opens or copies", "enter"),
		keyed(toolFileKey, toolFileKey, "its config"),
		pressing(m.leader()+" "+paneDetachAlt+" puts one away", m.leader(), paneDetachAlt),
		pressing(m.leader()+" "+toolCloseKey+" ends it", m.leader(), toolCloseKey),
		pressing("Esc cancels", "esc"),
	}
	return d
}

// resolvedAddresses is one discobox's entry in the picker's address cache: what
// was found, or why nothing was.
//
// An entry that is present and empty is a lookup still running, which is what
// keeps reopening the picker from starting a second one — so it is the presence
// of the entry, not its contents, that means "already asked".
type resolvedAddresses struct {
	Addresses
	err string
}

// action is one address as a row of the picker.
//
// The address itself is the row's detail, not a caption offering to produce it:
// what makes `ssh mybox` worth a row is reading it and seeing that there is
// nothing else to it, and a row that said "copy the ssh address" would be a
// button for a fact it declined to show.
func (r resolvedAddresses) action(key, label, value, unreachable string) action {
	row := action{key: key, press: key, label: label, stays: true}
	switch {
	case unreachable != "":
		row.why = unreachable
	case value != "":
		row.detail, row.enabled = value, true
	case r.err != "":
		row.why = r.err
	case r.Addresses == (Addresses{}):
		row.why = "looking it up…"
	default:
		// Resolved, and this half of it came back empty: a discobox with no
		// unambiguous host pattern, or none whose source ever landed.
		row.why = "nothing to reach it by"
	}
	return row
}

// resolveAddresses starts one discobox's lookup, or reports that there is
// nothing to start: it is already known, or already running.
//
// A failed lookup is retried on the next open rather than cached as the answer.
// What it failed at — writing an ssh config, reaching the server — is the kind
// of thing that stops being true, and the retry is a person reopening the card
// to see whether it has.
func (m *Model) resolveAddresses(box Sandbox) tea.Cmd {
	if box.ID == "" || !box.attachable() {
		return nil
	}
	if m.addresses == nil {
		m.addresses = map[string]resolvedAddresses{}
	}
	if known, asked := m.addresses[box.ID]; asked && known.err == "" {
		return nil
	}
	m.addresses[box.ID] = resolvedAddresses{}
	ctx, ds, id := m.ctx, m.ds, box.ID
	return func() tea.Msg {
		addr, err := ds.Addresses(ctx, id)
		return addressesMsg{id: id, addr: addr, err: err}
	}
}

// addressesResolved records what came back and, when the picker is still the
// card on screen and still on that discobox, builds it again.
//
// Again rather than patched: the rows close over the addresses they were built
// with — a card's callback is fixed when the card is — so a row whose text was
// replaced in place would still copy the nothing it was built around.
func (m *Model) addressesResolved(msg addressesMsg) tea.Cmd {
	entry := resolvedAddresses{Addresses: msg.addr}
	if msg.err != nil {
		entry.err = msg.err.Error()
	}
	if m.addresses == nil {
		m.addresses = map[string]resolvedAddresses{}
	}
	m.addresses[msg.id] = entry

	box := m.currentBox()
	if m.dialog == nil || m.dialog.title != toolsTitle || box.ID != msg.id {
		return nil
	}
	// The cursor is where the reader put it, and a card rebuilt under them
	// must not move it.
	cursor := m.dialog.cursor
	m.dialog = m.toolsDialog(box)
	m.dialog.cursor = min(cursor, len(m.dialog.items)-1)
	return nil
}

// copyAddress is a chosen address on its way to the clipboard. Nothing to copy
// is nothing to do: the row it came from was not selectable in the first place.
func copyAddress(text string) tea.Cmd {
	if text == "" {
		return nil
	}
	return func() tea.Msg { return copyAddressMsg{text: text} }
}

// copiedNote is what an address row says about itself: "copied", on the one
// whose text is on the clipboard, and nothing on the other.
//
// It is matched on the text rather than on which key was pressed, so a row
// whose address has since been resolved to something else does not go on
// claiming a copy that was of the old one.
func (m *Model) copiedNote(value string) string {
	if value == "" || value != m.copied {
		return ""
	}
	return "copied"
}

// addressCopied puts one address on the clipboard and marks the row it came
// from, leaving the card up.
//
// The card stays because the confirmation is on it: a press that copied and
// closed reported itself on a status line under a window the reader had just
// been taken back to, which is where a small green word goes unseen. Staying
// also answers the other half of it — the address beside this one is usually
// the next thing wanted.
func (m *Model) addressCopied(msg copyAddressMsg) tea.Cmd {
	m.copied = msg.text
	if d := m.dialog; d != nil && d.title == toolsTitle {
		cursor := d.cursor
		m.dialog = m.toolsDialog(m.currentBox())
		m.dialog.cursor = min(cursor, len(m.dialog.items)-1)
	}
	return m.copyText(msg.text, "copied "+msg.text)
}

// openToolFiles lists what one tool carries, so a file can be picked to edit.
//
// A tool with one file skips the list: it is a menu with a single row, which is
// a press to answer a question that has one answer. A tool with none says so —
// the key is offered on every row, because which rows have files is not
// something to have to remember.
func (m *Model) openToolFiles(id string) tea.Cmd {
	t, ok := toolByID(id)
	if !ok {
		return status("no such tool: %s", id)
	}
	if len(t.files) == 0 {
		return status("%s carries no config", t.label)
	}
	if len(t.files) == 1 {
		file := t.files[0]
		return func() tea.Msg { return toolFileMsg{file: file} }
	}
	items := make([]action, 0, len(t.files))
	for i, file := range t.files {
		n := itoa(i + 1)
		items = append(items, action{
			// The key is the row's index, so the first nine files can be
			// picked by number as well as by moving to them — the way the
			// harnesses screen numbers its own.
			key:     n,
			press:   n,
			label:   file.Name,
			detail:  m.toolFileDetail(file),
			enabled: true,
		})
	}
	files := t.files
	menu := actionsDialog("Config — "+t.label, "", items, func(key string) tea.Cmd {
		for i, file := range files {
			if itoa(i+1) == key {
				return func() tea.Msg { return toolFileMsg{file: file} }
			}
		}
		return nil
	})
	menu.keys = []hint{pressing("Enter opens it in $EDITOR", "enter"), pressing("Esc cancels", "esc")}
	m.dialog = menu
	return nil
}

// toolFileDetail is what a file's row says about it: where it lands inside a
// discobox, and where the copy being edited actually is — the second because a
// file you can only reach through this window is one you cannot put in your
// dotfiles.
func (m *Model) toolFileDetail(file ToolFile) string {
	detail := "→ ~/" + file.Home
	if path := m.ds.ToolFilePath(file); path != "" {
		detail += "  ·  " + path
	}
	return detail
}

// editToolFile hands the terminal to $EDITOR on the local copy, creating it
// from the tool's default when there is none yet.
func (m *Model) editToolFile(file ToolFile) tea.Cmd {
	m.busy = "editing " + file.Name + "…"
	var changed bool
	run := &harnessExec{
		title: "Editing " + file.Name,
		ctx:   m.ctx,
		run: func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
			var err error
			changed, err = m.ds.EditToolFile(ctx, file, stdin, stdout, stderr)
			return err
		},
	}
	return m.exec(run, func(err error) tea.Msg {
		return toolFileDoneMsg{file: file, changed: changed, err: err}
	})
}

// toolFileEdited reports what the editor did.
//
// A change is worth a sentence rather than a word, because what it did *not* do
// is the surprising half: the discoboxes already carrying this file keep the
// copy they have, and this is what the next one will get.
func (m *Model) toolFileEdited(msg toolFileDoneMsg) tea.Cmd {
	m.busy = ""
	switch {
	case msg.err != nil:
		return m.report(true, "edit %s: %v", msg.file.Name, msg.err)
	case !msg.changed:
		return m.report(false, "%s unchanged", msg.file.Name)
	default:
		return m.report(false, "%s saved — the next discobox to open %s gets it",
			msg.file.Name, msg.file.Tool)
	}
}

// runTool is what choosing a row does.
//
// A tool that is not a session is simply run — vscode is a request that
// returns. A tool already on screen is shown again, wherever it was left; one
// that is not is created, which is the only path that talks to the server.
func (m *Model) runTool(id string) tea.Cmd {
	t, ok := toolByID(id)
	if !ok {
		return status("no such tool: %s", id)
	}
	if !t.session() {
		return m.openEditor(m.currentBox())
	}
	if !m.inPanes() {
		// A tool is a window over the workspace, and there is no workspace to
		// put it over. Nothing offers this today; saying so beats opening a
		// screen with nothing under it.
		return status("%s opens over a workspace — attach first", t.label)
	}
	if p := m.toolPane(t.id); p != nil {
		m.showTool(p)
		return nil
	}
	if m.toolOpening[t.id] {
		return nil
	}
	return m.newTool(t, true)
}

// newTool creates a tool session and attaches to it. The pane is sized for the
// window before it is opened — it is drawn at the full width whether or not it
// is showing — for the same reason every other pane is: the size is what the
// far end is told.
func (m *Model) newTool(t tool, show bool) tea.Cmd {
	if m.toolOpening == nil {
		m.toolOpening = map[string]bool{}
	}
	m.toolOpening[t.id] = true
	m.busy = t.label + "…"
	gen := m.wsGen
	cols, rows := m.paneCells(max(m.width, 4))
	ctx, ds, box, id, spec := m.ctx, m.ds, m.paneBox.ID, t.id, t.spec()
	return func() tea.Msg {
		exec, term, err := ds.NewTool(ctx, box, spec, cols, rows)
		return toolTermMsg{gen: gen, id: id, exec: exec, term: term, err: err, show: show}
	}
}

// openToolExec attaches to a tool session the poll found already running: this
// discobox's diff, left open by a window that has since exited, or by this one
// before it was restarted.
func (m *Model) openToolExec(gen int, exec Exec) tea.Cmd {
	if m.toolOpening == nil {
		m.toolOpening = map[string]bool{}
	}
	m.toolOpening[exec.Tool] = true
	cols, rows := m.paneCells(max(m.width, 4))
	ctx, ds, box := m.ctx, m.ds, m.paneBox.ID
	return func() tea.Msg {
		term, err := ds.OpenExec(ctx, box, exec.ID, cols, rows)
		return toolTermMsg{gen: gen, id: exec.Tool, exec: exec, term: term, err: err}
	}
}

// toolOpened starts drawing a connected tool session, or reports why there is
// none.
//
// A tool that failed is a report and nothing else: the workspace under it is
// untouched, and there was no screen to take away.
func (m *Model) toolOpened(msg toolTermMsg) tea.Cmd {
	if msg.gen != m.wsGen {
		// From a workspace that has since been left; its stream must not leak.
		if msg.term != nil {
			_ = msg.term.Close()
		}
		return nil
	}
	m.busy = ""
	delete(m.toolOpening, msg.id)
	label := msg.id
	if t, ok := toolByID(msg.id); ok {
		label = t.label
	}
	if msg.err != nil {
		return m.report(true, "%s: %v", label, msg.err)
	}
	if existing := m.toolPane(msg.id); existing != nil {
		// The poll and the picker raced onto the same tool; the pane that
		// arrived first is the pane.
		_ = msg.term.Close()
		if msg.show {
			m.showTool(existing)
		}
		return nil
	}

	m.nextPaneID++
	p := &pane{
		id:      m.nextPaneID,
		term:    termpane.New(m.paneOptions(paneTool, false)...),
		stream:  msg.term,
		action:  Interaction(label),
		sandbox: m.paneBox,
		execID:  msg.exec.ID,
		title:   label,
		tool:    msg.id,
	}
	// The strip is in session order like the other two, and a tool arriving
	// beside the one being looked at must not move the window onto it.
	m.tools.insert(p, msg.exec, m.toolOpen)
	if msg.show {
		m.showTool(p)
	}
	m.layout()
	return tea.Batch(
		fromPane(p.id, p.term.Attach(msg.term)),
		fromPane(p.id, m.paneEvents(msg.term)),
	)
}

// toolPane is the pane running one tool, showing or put away, or nil when that
// tool is not open in this window.
func (m *Model) toolPane(id string) *pane {
	for _, p := range m.tools.panes {
		if p.tool == id {
			return p
		}
	}
	return nil
}

// showingTool is the tool pane with the window, or nil when the workspace
// itself is on screen.
func (m *Model) showingTool() *pane {
	if !m.toolOpen {
		return nil
	}
	return m.tools.visible()
}

// showTool gives one tool the window. The keys go with it: it is the whole
// screen, and every key in it is the tool's.
func (m *Model) showTool(p *pane) {
	if i := m.tools.index(p); i >= 0 {
		m.tools.active = i
	}
	m.toolOpen = true
	m.focus = focusPane
	m.prompt.Blur()
	m.layout()
}

// minimizeTool puts the showing tool away: the window goes back to the
// workspace, which has been running underneath the whole time, and the tool's
// session keeps running with its stream still attached — so choosing it again
// shows the screen it is on now, not the screen it was on when you left.
func (m *Model) minimizeTool() tea.Cmd {
	p := m.showingTool()
	if p == nil {
		return nil
	}
	m.toolOpen = false
	m.layout()
	return status("%s put away — %s %s reopens it", p.name(), m.leader(), toolsKey)
}

// closeTool ends the showing tool: the session is killed in the discobox and
// the pane goes with it. It is the one place this window ends a session rather
// than closing its own view of one, which is why it is a separate button from
// the one that only hides it.
func (m *Model) closeTool() tea.Cmd {
	p := m.showingTool()
	if p == nil {
		return nil
	}
	name := p.name()
	return tea.Batch(m.dropTool(p, true), status("%s closed", name))
}

// dropTool takes a tool off the screen and out of the strip, ending its session
// when there is one still running.
func (m *Model) dropTool(p *pane, kill bool) tea.Cmd {
	execID := p.execID
	i := m.tools.index(p)
	if i < 0 {
		return nil
	}
	// Only the tool being looked at gives the window back. One that was put
	// away and has since died leaves the strip without disturbing the screen.
	showing := m.hasScreen(p)
	m.tools.close(i)
	if showing {
		m.toolOpen = false
	}
	m.layout()
	if !kill || execID == "" {
		return nil
	}
	ctx, ds, box := m.ctx, m.ds, m.paneBox.ID
	return func() tea.Msg {
		if err := ds.EndExec(ctx, box, execID); err != nil {
			return statusMsg{text: "could not end the session: " + err.Error(), err: true}
		}
		return nil
	}
}

// toolExec reports whether a session is a tool's. It is the exec's own label
// that answers, so every window agrees — see ADR 0071 — and it is checked
// before the terminal/shell question, because a tool is neither.
func toolExec(exec Exec) bool { return exec.Tool != "" }

// toolControls is the tool window's chrome: `[-]` puts it away and `[x]` ends
// it, set into the right end of its top border the way a column's maximize
// button is, and recorded so a click on either can be routed back.
//
// They are the two things you can do to a window that outlives being looked at,
// and they are drawn rather than only bound because a window with no visible
// way to close it is one people leave running.
//
// toolControlsMinWidth is the box width below which they go rather than the
// border: a control that overruns its own corner is worse than no control, and
// the keys reach both at any width.
func (m *Model) toolControls(edge lipgloss.Style, width int) string {
	const toolControlsMinWidth = 16
	if width < toolControlsMinWidth {
		return ""
	}
	// `…[-][x]─╮`: the bracketed cells end two columns short of the box's right
	// edge, leaving the rule cell that keeps them off the corner.
	end := width - 3
	m.buttonSpans = append(m.buttonSpans,
		buttonSpan{action: buttonMinimize, start: end - 5, end: end - 3},
		buttonSpan{action: buttonClose, start: end - 2, end: end},
	)
	button := func(glyph string) string {
		return edge.Render("[") + m.st.dimText.Render(glyph) + edge.Render("]")
	}
	return button("-") + button("x")
}

// buttonAction is what a press on the tool window's chrome means.
type buttonAction int

const (
	buttonMinimize buttonAction = iota
	buttonClose
)

// buttonSpan is where one of those buttons sits on the box's top border row, in
// absolute screen columns, both ends inclusive.
type buttonSpan struct {
	action     buttonAction
	start, end int
}

// buttonAt is the tool window button under a screen position, if any. The spans
// were recorded when the border was drawn, so only a button actually on screen
// has one.
func (m *Model) buttonAt(x, y int) (buttonAction, bool) {
	if y != 1+m.bannerTop() {
		return 0, false
	}
	for _, s := range m.buttonSpans {
		if x >= s.start && x <= s.end {
			return s.action, true
		}
	}
	return 0, false
}

// toolHints is the bottom line while a tool has the screen: what it is, and the
// two ways out of it, which are not the same thing.
//
// Most worth keeping first, since the row drops from the end; see fitHints.
// Putting it away leads, because it is the one people reach for and the one
// that does not destroy anything.
func (m *Model) toolHints(p *pane) []hint {
	leader := m.leader()
	return []hint{
		says("every key goes to " + p.name()),
		pressing(m.detachHint()+" put away", leader, paneDetachAlt),
		pressing(leader+" "+toolCloseKey+" close", leader, toolCloseKey),
		pressing(leader+" "+toolsKey+" tools", leader, toolsKey),
	}
}
