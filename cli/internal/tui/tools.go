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
// shell. See ADR 0071 on tool sessions.

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

// Which tools there are is not this package's answer (ADR 0125): they are
// declarations — the CLI's, the discobox's image's and primary source's, and
// the user's — merged by the data source. The picker asks for one box's
// catalog each time it opens, and it is the only way to a tool from this
// window — the list of boxes offers none.

// resolvedTools is one discobox's entry in the picker's catalog cache: what
// came back, or why nothing did. It is kept across opens, so reopening the
// picker draws the last answer while the next one is on its way.
type resolvedTools struct {
	tools []Tool
	err   string
}

// toolRowKey is the identity a tool's row answers the picker's callback with.
// It is not the key the row is pressed by: that is the declaration's to ask
// for, and two declarations can ask for the same one.
func toolRowKey(id string) string { return "tool:" + id }

// toolByID is the tool with an id in the catalog of the discobox the window is
// on.
//
// A catalog can hold a declaration the merge refused beside the tool it tried
// to replace, under the same id (ADR 0125 §5). The one that runs is the tool.
func (m *Model) toolByID(id string) (Tool, bool) {
	var refused *Tool
	catalog := m.toolCatalogs[m.currentBox().ID].tools
	for i, t := range catalog {
		if t.ID != id {
			continue
		}
		if t.Problem == "" {
			return t, true
		}
		if refused == nil {
			refused = &catalog[i]
		}
	}
	if refused != nil {
		return *refused, true
	}
	return Tool{}, false
}

// toolKeys decides which tools get the keys they ask for on a card whose other
// rows already answer to taken. Only a tool that can run gets one — a refused
// row holding a key would turn the key into an error. Tools that run on this
// machine choose first, because what a key on your machine does is not a
// discobox's to change (ADR 0125 §5); then first come, first served, in the
// catalog's order. A tool that loses its key is still on the card, reached by
// moving to it.
func toolKeys(catalog []Tool, taken ...string) map[string]string {
	used := map[string]bool{}
	for _, key := range taken {
		used[key] = true
	}
	keys := map[string]string{}
	for _, host := range []bool{true, false} {
		for _, t := range catalog {
			if t.Host != host || t.Problem != "" || t.Key == "" || used[t.Key] {
				continue
			}
			used[t.Key] = true
			keys[t.ID] = t.Key
		}
	}
	return keys
}

// pickerTools is a catalog as the picker lists it: every tool, and every
// declaration that cannot run with the reason, except one the merge refused
// beside a tool that does run under its id — that row would be a second entry
// for the same tool, and choosing either would mean the one that runs.
func pickerTools(catalog []Tool) []Tool {
	runs := map[string]bool{}
	for _, t := range catalog {
		if t.Problem == "" {
			runs[t.ID] = true
		}
	}
	out := make([]Tool, 0, len(catalog))
	for _, t := range catalog {
		if t.Problem != "" && runs[t.ID] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// runToolWhenKnown runs a tool by id from outside the picker, which has no card
// to wait on: straight away when the discobox's catalog is known, and when it
// arrives otherwise. The availability check is the picker's own, so a box that
// could not run a tool from there cannot from here either.
func (m *Model) runToolWhenKnown(id string) tea.Cmd {
	box := m.currentBox()
	if why := attachWhy(true, []Sandbox{box}); why != "" {
		return status("%s: %s", id, why)
	}
	// A lookup that failed is not a known catalog: asking again is how the
	// click learns whether it still fails, and the reason reaches the status
	// line (toolsResolved) rather than "no such tool" against no tools.
	if entry, known := m.toolCatalogs[box.ID]; known && entry.err == "" {
		return m.runTool(id)
	}
	m.toolWanted = toolWant{box: box.ID, id: id}
	return m.resolveTools(box)
}

// toolWant is a tool asked for by id before its discobox's catalog was known.
type toolWant struct {
	box string
	id  string
}

// toolsMsg is one discobox's catalog, looked up.
type toolsMsg struct {
	id    string
	tools []Tool
	err   error
}

// resolveTools starts one discobox's catalog lookup.
func (m *Model) resolveTools(box Sandbox) tea.Cmd {
	if box.ID == "" {
		return nil
	}
	ctx, ds, id := m.ctx, m.ds, box.ID
	return func() tea.Msg {
		catalog, err := ds.Tools(ctx, id)
		return toolsMsg{id: id, tools: catalog, err: err}
	}
}

// toolsResolved records a catalog and, when the picker is still the card on
// screen and still on that discobox, builds it again — for the reason
// addressesResolved does.
func (m *Model) toolsResolved(msg toolsMsg) tea.Cmd {
	if m.toolCatalogs == nil {
		m.toolCatalogs = map[string]resolvedTools{}
	}
	entry := resolvedTools{tools: msg.tools}
	if msg.err != nil {
		// A failed lookup keeps the last catalog it had, with the reason: the
		// tools a box offered a moment ago are a better card than none.
		entry = resolvedTools{tools: m.toolCatalogs[msg.id].tools, err: msg.err.Error()}
	}
	_, hadCatalog := m.toolCatalogs[msg.id]
	m.toolCatalogs[msg.id] = entry
	box := m.currentBox()
	if want := m.toolWanted; want.box == msg.id {
		m.toolWanted = toolWant{}
		// A lookup that failed is the answer to the click that was waiting on
		// it: running the tool against no catalog would report "no such tool"
		// and lose the reason there is none.
		if msg.err != nil {
			return m.report(true, "%s: %v", want.id, msg.err)
		}
		if box.ID == want.box {
			return m.runTool(want.id)
		}
	}
	if m.dialog == nil || m.dialog.title != toolsTitle || box.ID != msg.id {
		return nil
	}
	cursor := m.dialog.cursor
	m.dialog = m.toolsDialog(box)
	// A card that had no tools on it had nothing for the cursor to be on: the
	// first tool is where it starts, as it would have on a card built with
	// them. Otherwise it stays where the reader put it.
	if hadCatalog {
		m.dialog.cursor = min(cursor, len(m.dialog.items)-1)
	}
	return nil
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

// openTools opens the picker, and starts the two lookups its rows are waiting
// on: the discobox's tools, and its addresses. Every tool is offered whatever
// is running: the row says which are up, and choosing one is "show me that",
// not "start another".
func (m *Model) openTools() tea.Cmd {
	box := m.currentBox()
	// A receipt belongs to the press that earned it, not to the card: reopening
	// the picker must not greet a reader with a "copied" from last time.
	m.copied = ""
	// Before the card is built, so the rows are drawn against the lookup this
	// open started rather than against the state before it.
	fetch := tea.Batch(m.resolveTools(box), m.resolveAddresses(box))
	m.dialog = m.toolsDialog(box)
	return fetch
}

// toolsDialog is the picker's card, built from what is known right now. It is
// separate from openTools because the addresses arrive after the card does, and
// the card is then built again rather than patched — see addressesResolved.
func (m *Model) toolsDialog(box Sandbox) *dialog {
	resolved, known := m.toolCatalogs[box.ID]
	catalog := pickerTools(resolved.tools)
	keys := toolKeys(catalog, toolFileKey, addressSSHKey, addressGitKey, "q", "j", "k")
	items := make([]action, 0, len(catalog)+3)
	for _, t := range catalog {
		detail := t.Detail
		if m.toolPane(t.ID) != nil {
			// Every tool pane is a running one: a tool that exits takes its
			// window with it (see paneClosed).
			detail = t.Detail + " · running"
		}
		row := action{
			key: toolRowKey(t.ID), press: keys[t.ID], label: t.Label, detail: detail,
			enabled: box.attachable(),
			why:     attachWhy(true, []Sandbox{box}),
		}
		if t.Problem != "" {
			row.enabled, row.why = false, t.Problem
		}
		items = append(items, row)
	}
	switch {
	case !known:
		items = append(items, action{key: "tools", label: "tools", why: "looking them up…"})
	case resolved.err != "" && len(catalog) == 0:
		items = append(items, action{key: "tools", label: "tools", why: resolved.err})
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
			for _, t := range catalog {
				if toolRowKey(t.ID) == key {
					id := t.ID
					return func() tea.Msg { return runToolMsg{id: id} }
				}
			}
			return nil
		})
	d.altKey = toolFileKey
	d.alt = func(key string) tea.Cmd {
		for _, t := range catalog {
			if toolRowKey(t.ID) == key {
				id := t.ID
				return func() tea.Msg { return toolFilesMsg{id: id} }
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
	t, ok := m.toolByID(id)
	if !ok {
		return status("no such tool: %s", id)
	}
	if len(t.Files) == 0 {
		return status("%s carries no config", t.Label)
	}
	if len(t.Files) == 1 {
		file := t.Files[0]
		return func() tea.Msg { return toolFileMsg{file: file} }
	}
	items := make([]action, 0, len(t.Files))
	for i, file := range t.Files {
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
	files := t.Files
	menu := actionsDialog("Config — "+t.Label, "", items, func(key string) tea.Cmd {
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
// A tool that runs on this machine is simply run — it is a request that
// returns. A tool already on screen is shown again, wherever it was left; one
// that is not is created, which is the only path that talks to the server.
func (m *Model) runTool(id string) tea.Cmd {
	t, ok := m.toolByID(id)
	if !ok {
		return status("no such tool: %s", id)
	}
	if t.Host {
		return m.runHostTool(m.currentBox(), t)
	}
	if !m.inPanes() {
		// A tool is a window over the workspace, and there is no workspace to
		// put it over. Nothing offers this today; saying so beats opening a
		// screen with nothing under it.
		return status("%s opens over a workspace — attach first", t.Label)
	}
	if p := m.toolPane(t.ID); p != nil {
		m.showTool(p)
		return nil
	}
	if m.toolOpening[t.ID] {
		return nil
	}
	return m.newTool(t, true)
}

// newTool creates a tool session and attaches to it. The pane is sized for the
// window before it is opened — it is drawn at the full width whether or not it
// is showing — for the same reason every other pane is: the size is what the
// far end is told.
func (m *Model) newTool(t Tool, show bool) tea.Cmd {
	if m.toolOpening == nil {
		m.toolOpening = map[string]bool{}
	}
	m.toolOpening[t.ID] = true
	m.busy = t.Label + "…"
	gen := m.wsGen
	cols, rows := m.paneCells(max(m.width, 4))
	ctx, ds, box, id := m.ctx, m.ds, m.paneBox.ID, t.ID
	return func() tea.Msg {
		exec, term, err := ds.NewTool(ctx, box, id, cols, rows)
		return toolTermMsg{gen: gen, id: id, exec: exec, term: term, err: err, show: show}
	}
}

// hostToolRanMsg is what came of handing a sandbox to a host tool.
type hostToolRanMsg struct {
	name string
	tool Tool
	err  error
}

// runHostTool hands one sandbox to a tool on this machine and reports on the
// status line.
//
// Nothing is suspended for it. The tool is another program, usually in another
// window, and the CLI's part is over as soon as it has been told which host to
// connect to — so this is a request that returns, like a verb, rather than
// something that owns the screen.
func (m *Model) runHostTool(box Sandbox, t Tool) tea.Cmd {
	m.busy = t.ID + "…"
	ctx, ds, id, name := m.ctx, m.ds, box.ID, box.Name
	return func() tea.Msg {
		return hostToolRanMsg{name: name, tool: t, err: ds.RunHostTool(ctx, id, t.ID)}
	}
}

// hostToolRan reports it. A failure carries the tool's id, which is the name
// of the `discobox tools` command that ran it — how someone runs it again by
// hand to read the whole error the status line had to cut. Only the sentence
// about a window that did open reads as prose, and only that one takes the
// label.
func (m *Model) hostToolRan(msg hostToolRanMsg) tea.Cmd {
	m.busy = ""
	if msg.err != nil {
		return m.report(true, "%s: %v", msg.tool.ID, msg.err)
	}
	return m.report(false, "opened %s in %s", msg.name, msg.tool.Label)
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
	if t, ok := m.toolByID(msg.id); ok {
		label = t.Label
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
	execID, name, gen := p.execID, p.name(), m.wsGen
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
	// The listing is a poll behind the kill, so the next tick still reports
	// this session live and would pick the tool back up off it — put away,
	// into a strip the press just emptied. Remembering it is what keeps a
	// closed tool closed. See endPane, which ends a session the same way.
	if m.ending == nil {
		m.ending = map[string]bool{}
	}
	m.ending[execID] = true
	ctx, ds, box := m.ctx, m.ds, m.paneBox.ID
	return func() tea.Msg {
		if err := ds.EndExec(ctx, box, execID); err != nil {
			return endExecFailedMsg{gen: gen, execID: execID, name: name, err: err}
		}
		return nil
	}
}

// toolExec reports whether a session is a tool's. It is the exec's own label
// that answers, so every window agrees — see ADR 0071 on tool sessions — and it
// is checked before the terminal/shell question, because a tool is neither.
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
