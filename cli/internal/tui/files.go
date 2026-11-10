package tui

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/discobox-ai/discobox/cli/internal/clipimage"
)

// The files panel is the discobox's files, as a tree down the right of the
// workspace: rooted at the box's primary source directory, read a directory at
// a time through the files API (DataSource.ListFiles) as each is opened. It is
// laid out the way the workspace tree on the left is — its cells come off the
// window's width (syncWidth), it draws against hit zones of its own, and the
// mouse over it never reaches the workspace — so nothing in the workspace knows
// it is there (ADR 26-11-10-203).
//
// A terminal has no drag and drop of its own, so the panel's is two gestures
// made out of what a terminal does have:
//
//   - A drag that starts on a row is the window's, from press to release
//     (filesDrag). Released on a directory, or on a file in one, it moves the
//     path there, or copies it with Ctrl or Alt held (TransferFile). Released
//     over a pane, it pastes the path, as it is in the discobox, into that
//     pane — which is what dropping a file onto a terminal does.
//   - A file dragged onto the window from this machine's file manager reaches
//     it as a paste of that file's local path; the terminal sends nothing else,
//     and most report no pointer while a drag from outside crosses the window.
//     So a paste that is nothing but paths to files and directories here, made
//     while the panel has the keys, is that drop, and those paths are copied
//     into the directory under the panel's cursor (UploadFiles), the way
//     `discobox cp -r` copies them. With the keys anywhere else a paste is
//     what it always was: a pane's, where an image path is still carried in
//     (imagepaste.go). Where the pointer last was is not asked: it is stale
//     the moment the pointer leaves the window, and a path pasted for an
//     agent would be taken for a drop.
//
// The panel's state for a box lives on that box's workspace (fileTree), so a
// workspace parked and brought back keeps what was open; whether the panel is
// up at all, and whether it has the keys, is the window's.

const (
	// filesPanelKey toggles the panel behind the leader: open it, give it the
	// keys, and put it away from there. F for files; f is a pull request's
	// files changed.
	filesPanelKey = "F"
	// filesWidth is the cells the panel takes off the window's right.
	filesWidth = 34
	// filesMinWorkspace is the least the workspace keeps beside the panel. A
	// window too narrow for both draws no panel rather than a workspace too
	// narrow to read.
	filesMinWorkspace = 60
	// filesTimeout bounds one listing, move or copy inside the discobox.
	filesTimeout = 30 * time.Second
	// filesUploadTimeout bounds copying a drop into the discobox. It is long,
	// because a dropped directory can be large, but not forever.
	filesUploadTimeout = 30 * time.Minute
)

// fileTree is one discobox's files as the panel shows them.
type fileTree struct {
	// root is the directory the panel is rooted at: the primary terminal's
	// working directory, which is the box's primary source directory, once the
	// exec listing has said what it is (workspaceExecs). Empty until then.
	root string
	// dirs is every directory read so far, by absolute path.
	dirs map[string]*fileDir
	// open is the directories drawn open, by absolute path.
	open map[string]bool
	// cursor is the row the keys act on; top is the first row drawn.
	cursor, top int
	// git is what git last said about the tree (filegit.go).
	git fileGit
}

// fileDir is one directory's listing.
type fileDir struct {
	entries []FileEntry
	loaded  bool
	loading bool
	err     error
}

// fileRow is one line of the panel: an entry at a depth, or, with note set, a
// line that says why a directory shows nothing — still loading, empty, or
// unreadable — whose entry is that directory.
type fileRow struct {
	entry FileEntry
	depth int
	note  string
}

// rows is the tree as drawn, top to bottom: each directory's entries, and an
// open one's own under it.
func (t *fileTree) rows() []fileRow {
	if t.root == "" {
		return nil
	}
	var out []fileRow
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		d := t.dirs[dir]
		switch {
		case d == nil || (!d.loaded && d.err == nil):
			out = append(out, fileRow{entry: FileEntry{Path: dir, Dir: true}, depth: depth, note: "loading…"})
			return
		case !d.loaded:
			out = append(out, fileRow{entry: FileEntry{Path: dir, Dir: true}, depth: depth, note: d.err.Error()})
			return
		case len(d.entries) == 0:
			out = append(out, fileRow{entry: FileEntry{Path: dir, Dir: true}, depth: depth, note: "empty"})
			return
		}
		for _, e := range d.entries {
			out = append(out, fileRow{entry: e, depth: depth})
			if e.Dir && t.open[e.Path] {
				walk(e.Path, depth+1)
			}
		}
	}
	walk(t.root, 0)
	return out
}

// current is the row under the cursor, if it is an entry.
func (t *fileTree) current() (FileEntry, bool) {
	rows := t.rows()
	if t.cursor < 0 || t.cursor >= len(rows) || rows[t.cursor].note != "" {
		return FileEntry{}, false
	}
	return rows[t.cursor].entry, true
}

// dirAt is the directory a row stands for as somewhere to put things: a
// directory row is itself, a file or a note is the directory it is in, and no
// row at all is the root.
func (t *fileTree) dirAt(i int) string {
	rows := t.rows()
	if i < 0 || i >= len(rows) {
		return t.root
	}
	r := rows[i]
	if r.note != "" || r.entry.Dir {
		return r.entry.Path
	}
	return path.Dir(r.entry.Path)
}

// move steps the cursor delta rows, stopping at the ends.
func (t *fileTree) move(delta int) {
	n := len(t.rows())
	t.cursor = min(max(t.cursor+delta, 0), max(n-1, 0))
}

// moveTo puts the cursor on the row for p, when it is drawn.
func (t *fileTree) moveTo(p string) {
	for i, r := range t.rows() {
		if r.note == "" && r.entry.Path == p {
			t.cursor = i
			return
		}
	}
}

// scrollTo keeps the cursor among the height rows drawn from top.
func (t *fileTree) scrollTo(height int) {
	n := len(t.rows())
	t.cursor = min(max(t.cursor, 0), max(n-1, 0))
	if t.cursor < t.top {
		t.top = t.cursor
	}
	if t.cursor >= t.top+height {
		t.top = t.cursor - height + 1
	}
	t.top = min(max(t.top, 0), max(n-height, 0))
}

// listed takes one directory's answer.
func (t *fileTree) listed(msg filesListedMsg) {
	if t.dirs == nil {
		t.dirs = map[string]*fileDir{}
	}
	dir := msg.dir
	// The root is asked for as it was given — `~` when the box named no
	// working directory — and filed under the path the discobox answers with.
	if msg.err == nil && msg.path != "" && msg.path != dir {
		if dir == t.root {
			t.root = msg.path
		}
		delete(t.dirs, dir)
		dir = msg.path
	}
	d := t.dirs[dir]
	if d == nil {
		d = &fileDir{}
		t.dirs[dir] = d
	}
	d.loading = false
	if msg.err != nil {
		d.err = msg.err
		// A directory that has gone is not drawn open over nothing.
		if d.loaded {
			d.loaded, d.entries = false, nil
		}
		return
	}
	entries := append([]FileEntry(nil), msg.entries...)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		a, b := strings.ToLower(entries[i].Name), strings.ToLower(entries[j].Name)
		if a != b {
			return a < b
		}
		return entries[i].Name < entries[j].Name
	})
	d.entries, d.loaded, d.err = entries, true, nil
}

// fileDrag is a press on a row of the panel, held: a click until the pointer
// moves, and from then a drag of that row's path.
type fileDrag struct {
	from FileEntry
	// x and y are where the press was, in the window's cells; atX and atY
	// where the pointer is now.
	x, y, atX, atY int
	clicks         int
	moved          bool
}

// filesListedMsg is one directory read for the panel. path is the directory
// as the discobox answered, absolute and cleaned.
type filesListedMsg struct {
	box     Sandbox
	dir     string
	path    string
	entries []FileEntry
	err     error
}

// filesChangedMsg is a move, copy or drop finished in the discobox: the
// directories it changed, to read again, and the one it went into, to open.
type filesChangedMsg struct {
	box  Sandbox
	dirs []string
	into string
	said string
	err  error
}

// filesAction is one of the panel's own controls.
type filesAction int

const (
	filesClose filesAction = iota
	filesFocus
	filesBack
	filesReload
)

// filesShown reports whether the panel is drawn: asked for, beside a
// workspace on screen, and in a window with room for both. A maximized column
// has the whole window, the panel's cells included, as it has the tree's.
func (m *Model) filesShown() bool {
	if !m.filesOpen || !m.workspaceShown() || m.maximized {
		return false
	}
	return m.screenWidth-m.treeCost()-filesWidth >= filesMinWorkspace
}

// filesCost is the cells the panel takes from the window's right.
func (m *Model) filesCost() int {
	if m.filesShown() {
		return filesWidth
	}
	return 0
}

// filesHasKeys reports whether the panel is what the keys go to.
func (m *Model) filesHasKeys() bool {
	return m.filesFront && m.filesShown() && m.inPanes() && m.audit == nil
}

// filesLeft is the window column the panel starts at.
func (m *Model) filesLeft() int { return m.screenWidth - filesWidth }

// fileTreeOf is the panel's state for one box: the live workspace's, or a
// parked one's. Nil for a box with no workspace.
func (m *Model) fileTreeOf(box Sandbox) *fileTree {
	ref := m.boxRef(box)
	if (m.hasWorkspace() || m.wsOpening) && m.boxRef(m.paneBox) == ref {
		return &m.fileTree
	}
	if ws := m.parked[ref]; ws != nil {
		return &ws.fileTree
	}
	return nil
}

// toggleFiles is the leader's F: open the panel with the keys, give an open
// one the keys, and put away one that has them.
func (m *Model) toggleFiles() tea.Cmd {
	if m.filesOpen && m.filesHasKeys() {
		m.filesOpen, m.filesFront = false, false
		return nil
	}
	if !m.workspaceShown() {
		return status("the files panel opens beside a workspace")
	}
	// A maximized column has the panel's cells; asking for the panel puts the
	// column back, as asking for the list does.
	if m.maximized {
		m.maximized = false
	}
	opening := !m.filesOpen
	m.filesOpen = true
	if !m.filesShown() {
		m.filesOpen = false
		return status("%s", m.filesTooNarrow())
	}
	m.focusFiles()
	m.syncWidth()
	m.layout()
	if opening {
		return m.reloadFiles()
	}
	return m.readGit()
}

// filesTooNarrow is why the panel cannot be drawn, or "" when the window has
// room for it beside the tree — the tree as it is drawn once a maximized
// column is put back, which opening the panel does.
func (m *Model) filesTooNarrow() string {
	need := filesWidth + filesMinWorkspace
	if m.treeMode() {
		need += treeWidth
	}
	if m.screenWidth >= need {
		return ""
	}
	return fmt.Sprintf("the files panel needs a window at least %d cells wide", need)
}

// filesMenuItem is the panel's entry in the box menu: open it, or put away
// the one that is up.
func (m *Model) filesMenuItem() action {
	if m.filesOpen && m.filesShown() {
		return action{key: filesPanelKey, press: filesPanelKey, label: "close file tree", detail: "put the files panel away", enabled: true}
	}
	why := m.filesTooNarrow()
	return action{key: filesPanelKey, press: filesPanelKey, label: "file tree",
		detail: "browse, move and drop files beside the workspace", enabled: why == "", why: why}
}

// filesFromMenu is the box menu's entry chosen: the panel put away when it is
// up, and opened with the keys otherwise.
func (m *Model) filesFromMenu() tea.Cmd {
	if m.filesOpen && m.filesShown() {
		m.filesOpen, m.filesFront = false, false
		return nil
	}
	return m.toggleFiles()
}

// focusFiles gives the panel the keys, from the panes or the list.
func (m *Model) focusFiles() {
	m.filesFront = true
	m.listFront, m.treeExpanded = false, false
	m.focus = focusPane
	m.prompt.Blur()
}

// filesRooted is the primary's working directory arriving with the exec
// listing: where the box's panel is rooted, read at once if the panel is up.
func (m *Model) filesRooted(workdir string) tea.Cmd {
	if m.fileTree.root != "" {
		return nil
	}
	if workdir == "" {
		// A sandbox that does not say where it starts its sessions is asked
		// for the home directory, which it answers with as a path.
		workdir = "~"
	}
	m.fileTree.root = workdir
	if !m.filesOpen {
		return nil
	}
	return tea.Batch(m.listFiles(m.paneBox, workdir), m.readGit())
}

// reloadFiles reads the root and every open directory of the live workspace
// again.
func (m *Model) reloadFiles() tea.Cmd {
	t := &m.fileTree
	if t.root == "" {
		return nil
	}
	cmds := []tea.Cmd{m.listFiles(m.paneBox, t.root), m.readGit()}
	for dir, open := range t.open {
		if open && dir != t.root {
			cmds = append(cmds, m.listFiles(m.paneBox, dir))
		}
	}
	return tea.Batch(cmds...)
}

// listFiles reads one directory of box for the panel.
func (m *Model) listFiles(box Sandbox, dir string) tea.Cmd {
	if t := m.fileTreeOf(box); t != nil {
		if t.dirs == nil {
			t.dirs = map[string]*fileDir{}
		}
		if d := t.dirs[dir]; d != nil {
			d.loading = true
		} else {
			t.dirs[dir] = &fileDir{loading: true}
		}
	}
	ds := m.ds.ForServer(box.Server)
	if ds == nil {
		return nil
	}
	parent := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, filesTimeout)
		defer cancel()
		listed, entries, err := ds.ListFiles(ctx, box.ID, dir)
		return filesListedMsg{box: box, dir: dir, path: listed, entries: entries, err: err}
	}
}

// filesListed files one directory's answer with the box it is from, on
// screen or parked.
func (m *Model) filesListed(msg filesListedMsg) tea.Cmd {
	t := m.fileTreeOf(msg.box)
	if t == nil {
		return nil
	}
	cur, had := t.current()
	t.listed(msg)
	// The cursor stays on the path it was on, wherever the answer moved it.
	if had {
		t.moveTo(cur.Path)
	}
	return nil
}

// setOpen opens or closes one directory, reading it as it opens.
func (m *Model) setOpen(e FileEntry, open bool) tea.Cmd {
	t := &m.fileTree
	if !e.Dir {
		return nil
	}
	if t.open == nil {
		t.open = map[string]bool{}
	}
	if !open {
		delete(t.open, e.Path)
		return nil
	}
	t.open[e.Path] = true
	return m.listFiles(m.paneBox, e.Path)
}

// filesKeyPress answers a key while the panel has the keys. The leader is not
// one of them (updateKey): it reaches the window as it does from the list.
func (m *Model) filesKeyPress(msg tea.KeyPressMsg) tea.Cmd {
	t := &m.fileTree
	cur, ok := t.current()
	switch keyName(msg) {
	case "esc":
		m.filesFront = false
	case "up", "k":
		t.move(-1)
	case "down", "j":
		t.move(1)
	case "pgup":
		t.move(-m.filesListHeight())
	case "pgdown":
		t.move(m.filesListHeight())
	case "home", "g":
		t.cursor = 0
	case "end", "G":
		t.move(len(t.rows()))
	case "enter":
		if !ok {
			return nil
		}
		if cur.Dir {
			return m.setOpen(cur, !t.open[cur.Path])
		}
		return m.pasteFilePath(m.focusedPane(), cur.Path)
	case "right", "l":
		if ok && cur.Dir {
			if t.open[cur.Path] {
				t.move(1)
				return nil
			}
			return m.setOpen(cur, true)
		}
	case "left", "h":
		if ok && cur.Dir && t.open[cur.Path] {
			return m.setOpen(cur, false)
		}
		// Up to the directory this row is in, the way a file manager's tree
		// goes.
		if rows := t.rows(); t.cursor >= 0 && t.cursor < len(rows) {
			r := rows[t.cursor]
			parent := r.entry.Path
			if r.note == "" {
				parent = path.Dir(parent)
			}
			if parent != t.root {
				t.moveTo(parent)
			}
		}
	case "p":
		if ok {
			return m.pasteFilePath(m.focusedPane(), cur.Path)
		}
	case "y":
		if ok {
			return m.copyText(cur.Path, fmt.Sprintf("copied %q", cur.Path))
		}
	case "d":
		if ok {
			return m.downloadFile(cur)
		}
	case "r":
		return m.reloadFiles()
	}
	return nil
}

// filesHints is the key line while the panel has the keys.
func (m *Model) filesHints() []hint {
	return []hint{
		pressing("↑↓ move", "down"),
		pressing("enter open or paste", "enter"),
		pressing("p paste path", "p"),
		pressing("y copy path", "y"),
		pressing("d download", "d"),
		pressing("r reload", "r"),
		pressing("esc back", "esc"),
		pressing(m.leader()+" "+filesPanelKey+" close", m.leader(), filesPanelKey),
	}
}

// pasteFilePath pastes a path in the discobox into a pane, quoted as a shell
// would need it — what a terminal pastes for a file dropped onto it.
func (m *Model) pasteFilePath(p *pane, filePath string) tea.Cmd {
	if p == nil {
		return status("no pane to paste into")
	}
	if p.issue != nil || p.exited || p.service != "" {
		return status("%s takes no input", p.name())
	}
	paste := tea.PasteMsg{Content: shellQuote(filePath)}
	if p.imageBusy {
		p.imageHeld = append(p.imageHeld, paste)
		return nil
	}
	return tea.Batch(m.toTerm(p, paste), status("pasted %q", filePath))
}

// filesAct is a press on one of the panel's own controls.
func (m *Model) filesAct(a filesAction) tea.Cmd {
	switch a {
	case filesClose:
		m.filesOpen, m.filesFront = false, false
	case filesFocus:
		m.focusFiles()
	case filesBack:
		m.filesFront = false
	case filesReload:
		return m.reloadFiles()
	}
	return nil
}

// filesMouse answers a mouse event while the panel is drawn, in the window's
// own cells, before the tree or the workspace sees it. A drag that started on
// a row is the panel's wherever the pointer goes until it is released; any
// other gesture the window holds — a drag in a pane, over its chrome — stays
// the window's over the panel too.
func (m *Model) filesMouse(msg tea.MouseMsg) (tea.Cmd, bool) {
	if m.filesDrag != nil {
		// A press while one is held means the release was lost — let go
		// outside the window — and the drag is over, unfinished: finishing it
		// here would move a file to wherever the next click happens to land.
		if _, ok := msg.(tea.MouseClickMsg); !ok {
			return m.dragFiles(msg), true
		}
		m.filesDrag = nil
	}
	if !m.filesShown() {
		m.filesHoverX, m.filesHoverY = -1, -1
		return nil, false
	}
	mouse := msg.Mouse()
	left := m.filesLeft()
	if mouse.X < left || m.mouseCapture != 0 || m.chromeCapture || m.promptCapture {
		if ev, ok := msg.(tea.MouseMotionMsg); ok && ev.Button == tea.MouseNone {
			m.filesHoverX, m.filesHoverY = -1, -1
		}
		// A press anywhere else hands the keys back to what it pressed.
		if _, ok := msg.(tea.MouseClickMsg); ok && mouse.X < left && !m.modalUp() {
			m.filesFront = false
		}
		return nil, false
	}
	x, y := mouse.X-left, mouse.Y
	if ev, ok := msg.(tea.MouseMotionMsg); ok && ev.Button == tea.MouseNone {
		m.filesHoverX, m.filesHoverY = x, y
		m.hoverX, m.hoverY = -1, -1
		m.treeHoverX, m.treeHoverY = -1, -1
		return nil, true
	}
	if m.treeBlocked() {
		return nil, true
	}
	where, ok := m.filesZones.at(x, y)
	switch ev := msg.(type) {
	case tea.MouseWheelMsg:
		m.fileTree.move(-wheelLines(ev))
	case tea.MouseClickMsg:
		if ev.Button != tea.MouseLeft {
			return nil, true
		}
		if ok && where.what.kind == hitFiles {
			return m.filesAct(filesAction(where.what.idx)), true
		}
		m.focusFiles()
		if !ok || where.what.kind != hitFileRow || where.what.idx < 0 {
			return nil, true
		}
		rows := m.fileTree.rows()
		i := where.what.idx
		if i >= len(rows) || rows[i].note != "" {
			return nil, true
		}
		m.fileTree.cursor = i
		m.filesDrag = &fileDrag{from: rows[i].entry, x: mouse.X, y: mouse.Y, atX: mouse.X, atY: mouse.Y, clicks: m.countClick(mouse.X, mouse.Y)}
	}
	return nil, true
}

// dragFiles carries a press held on a row: motion makes it a drag, and the
// release is a click where it was pressed and a drop anywhere else.
func (m *Model) dragFiles(msg tea.MouseMsg) tea.Cmd {
	d := m.filesDrag
	mouse := msg.Mouse()
	switch msg.(type) {
	case tea.MouseMotionMsg:
		// No button down is a release that never arrived.
		if mouse.Button == tea.MouseNone {
			m.filesDrag = nil
			return nil
		}
		d.atX, d.atY = mouse.X, mouse.Y
		if mouse.X != d.x || mouse.Y != d.y {
			d.moved = true
		}
		return nil
	case tea.MouseReleaseMsg:
		m.filesDrag = nil
		if !d.moved && mouse.X == d.x && mouse.Y == d.y {
			return m.fileClicked(d)
		}
		return m.fileDropped(d, mouse)
	}
	return nil
}

// fileClicked is a press and release on one row: a directory opens or
// closes, and a second click on a file pastes its path into the pane with the
// keys, as Enter does.
func (m *Model) fileClicked(d *fileDrag) tea.Cmd {
	if d.from.Dir {
		return m.setOpen(d.from, !m.fileTree.open[d.from.Path])
	}
	if d.clicks > 1 {
		return m.pasteFilePath(m.focusedPane(), d.from.Path)
	}
	return nil
}

// fileDropped is a row's path let go of somewhere: on the panel, a move (a
// copy with Ctrl or Alt) into the directory under the pointer; on a pane, the
// path pasted into it.
func (m *Model) fileDropped(d *fileDrag, at tea.Mouse) tea.Cmd {
	left := m.filesLeft()
	if at.X >= left {
		where, ok := m.filesZones.at(at.X-left, at.Y)
		if !ok || where.what.kind != hitFileRow {
			return nil
		}
		duplicate := at.Mod&(tea.ModCtrl|tea.ModAlt) != 0
		return m.transferFile(d.from, m.fileTree.dirAt(where.what.idx), duplicate)
	}
	x := at.X - m.treeCost()
	if x < 0 {
		return nil
	}
	p, _, _ := m.paneAt(x, at.Y)
	if p == nil {
		return nil
	}
	m.focusPane(p)
	m.filesFront = false
	return m.pasteFilePath(p, d.from.Path)
}

// transferFile moves or copies one path of the box on screen into dir.
func (m *Model) transferFile(from FileEntry, dir string, duplicate bool) tea.Cmd {
	if dir == path.Dir(from.Path) {
		return status("%q is already in %q", from.Name, dir)
	}
	if from.Dir && (dir == from.Path || strings.HasPrefix(dir, from.Path+"/")) {
		return status("%q cannot go inside itself", from.Name)
	}
	box := m.paneBox
	ds := m.ds.ForServer(box.Server)
	if ds == nil {
		return status("server %q is no longer available", box.Server)
	}
	verb, done := "moving", "moved"
	if duplicate {
		verb, done = "copying", "copied"
	}
	to := path.Join(dir, from.Name)
	parent := m.ctx
	work := func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, filesTimeout)
		defer cancel()
		err := ds.TransferFile(ctx, box.ID, from.Path, to, duplicate)
		return filesChangedMsg{
			box:  box,
			dirs: []string{path.Dir(from.Path), dir},
			into: dir,
			said: fmt.Sprintf("%s %q to %q", done, from.Name, dir),
			err:  err,
		}
	}
	// The progress goes up before the work starts, so a fast answer is not
	// overwritten by it (REVIEW.md).
	expires := m.update(status("%s %q to %q…", verb, from.Name, dir)())
	return tea.Batch(work, expires)
}

// filesDownloadedMsg is one path copied out of the box: where it landed on
// this machine, or why it did not.
type filesDownloadedMsg struct {
	name  string
	local string
	err   error
}

// downloadFile copies the row's path out of the box on screen into this
// machine's downloads directory (DataSource.DownloadFile). A terminal cannot
// drag a file out onto the desktop, so a key is how one leaves.
func (m *Model) downloadFile(e FileEntry) tea.Cmd {
	box := m.paneBox
	ds := m.ds.ForServer(box.Server)
	if ds == nil {
		return status("server %q is no longer available", box.Server)
	}
	name, remote, parent := fileName(e.Name), e.Path, m.ctx
	work := func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, filesUploadTimeout)
		defer cancel()
		local, err := ds.DownloadFile(ctx, box.ID, remote)
		return filesDownloadedMsg{name: name, local: local, err: err}
	}
	expires := m.update(status("downloading %q…", name)())
	return tea.Batch(work, expires)
}

// filesDownloaded says where a download landed.
func (m *Model) filesDownloaded(msg filesDownloadedMsg) tea.Cmd {
	if msg.err != nil {
		return m.report(true, "download %q: %v", msg.name, msg.err)
	}
	return status("downloaded %q to %s", msg.name, msg.local)
}

// filesDropMsg is a paste the panel looked at: the local paths it names when
// it is a drop, none when it is not.
type filesDropMsg struct {
	box   Sandbox
	dir   string
	paths []string
}

// filesPaste looks at a paste into the panel as files dropped onto it, landing
// in the directory under the cursor.
func (m *Model) filesPaste(msg tea.PasteMsg) tea.Cmd {
	t := &m.fileTree
	if t.root == "" {
		return status("the box has not said where its files are yet")
	}
	box, dir, text, parent := m.paneBox, t.dirAt(t.cursor), msg.Content, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, filesTimeout)
		defer cancel()
		return filesDropMsg{box: box, dir: dir, paths: clipimage.LocalPaths(ctx, text)}
	}
}

// filesDropped copies a drop into the box. A paste that was not one is
// nobody's: the panel has the keys, and it takes no text.
func (m *Model) filesDropped(msg filesDropMsg) tea.Cmd {
	if len(msg.paths) == 0 {
		return status("only files from this machine can be dropped on the files panel")
	}
	ds := m.ds.ForServer(msg.box.Server)
	if ds == nil {
		return status("server %q is no longer available", msg.box.Server)
	}
	what := plural(len(msg.paths), "item", "items")
	if len(msg.paths) == 1 {
		what = fmt.Sprintf("%q", path.Base(strings.ReplaceAll(msg.paths[0], "\\", "/")))
	}
	box, dir, paths, parent := msg.box, msg.dir, msg.paths, m.ctx
	work := func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, filesUploadTimeout)
		defer cancel()
		err := ds.UploadFiles(ctx, box.ID, paths, dir)
		return filesChangedMsg{box: box, dirs: []string{dir}, into: dir, said: fmt.Sprintf("copied %s into %q", what, dir), err: err}
	}
	expires := m.update(status("copying %s into %q…", what, dir)())
	return tea.Batch(work, expires)
}

// filesChanged reads again what a move, copy or drop changed, and opens the
// directory it went into.
func (m *Model) filesChanged(msg filesChangedMsg) tea.Cmd {
	t := m.fileTreeOf(msg.box)
	var cmds []tea.Cmd
	if msg.err != nil {
		cmds = append(cmds, m.report(true, "%s: %v", strings.TrimSuffix(msg.said, "…"), msg.err))
	} else {
		cmds = append(cmds, status("%s", msg.said))
	}
	if t == nil {
		return tea.Batch(cmds...)
	}
	if msg.err == nil && msg.into != "" && msg.into != t.root {
		if t.open == nil {
			t.open = map[string]bool{}
		}
		t.open[msg.into] = true
	}
	if m.boxRef(msg.box) == m.boxRef(m.paneBox) {
		cmds = append(cmds, m.readGit())
	}
	seen := map[string]bool{}
	for _, dir := range msg.dirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if dir == t.root || t.open[dir] {
			cmds = append(cmds, m.listFiles(msg.box, dir))
		}
	}
	return tea.Batch(cmds...)
}

// filesListHeight is the rows the panel's list has, from the same arithmetic
// viewFiles draws with: the window less the rows above and below the box, its
// two edges, the root's row and the keys' row.
func (m *Model) filesListHeight() int {
	return max(m.height-2-4, 1)
}

// gitName is a file's name in the color of what git says about it.
func (m *Model) gitName(e FileEntry) string {
	name := fileName(e.Name)
	if _, letter := m.gitMark(e); letter != 0 {
		return m.gitStyle(letter).Render(name)
	}
	return name
}

// fileName is a name the discobox wrote, made safe to draw: a control
// character in it would be an escape sequence in the window's frame.
func fileName(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return '?'
		}
		return r
	}, s)
}

// truncateStart shortens text to w cells from its start, keeping its end: a
// path is told apart by its last elements.
func truncateStart(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	return "…" + ansi.TruncateLeft(s, lipgloss.Width(s)-w+1, "")
}

// viewFiles draws the panel, m.height rows tall, against zones of its own:
// a blank row level with the workspace header, then a box level with its
// panes holding the root, the tree, and the keys.
func (m *Model) viewFiles() string {
	inner := filesWidth - 2
	saved := m.zones
	m.zones = m.filesZones
	m.zones.reset()
	m.zones.setHover(m.filesHoverX, m.filesHoverY)
	m.zones.push(0, 1)

	focused := m.filesHasKeys()
	edge := m.st.rule
	if focused {
		edge = m.st.frame
	}
	side := edge.Render("│")
	control := edge.Render("[") + "x" + edge.Render("]")
	// The [x] sits where titledEdge puts a control: a cell of rule and the
	// corner after it.
	m.zones.mark(hit{kind: hitFiles, idx: int(filesClose)}, filesWidth-5, 0, 3, 1)
	out := []string{titledEdge(m.st, edge, "files", control, inner)}

	t := &m.fileTree
	root := "waiting for the box…"
	if t.root != "" {
		root = fileName(t.root)
	}
	out = append(out, side+padANSI(" "+m.st.dimText.Render(truncateStart(root, inner-2)), inner)+side)

	listH := m.filesListHeight()
	rows := t.rows()
	t.scrollTo(listH)
	m.zones.mark(hit{kind: hitFileRow, idx: -1}, 1, len(out), inner, listH)
	target := -1
	if d := m.filesDrag; d != nil && d.moved && d.atX >= m.filesLeft() {
		if where, ok := m.filesZones.at(d.atX-m.filesLeft(), d.atY); ok && where.what.kind == hitFileRow {
			target = where.what.idx
		}
	}
	for i := t.top; i < len(rows) && i < t.top+listH; i++ {
		r := rows[i]
		y := len(out)
		indent := strings.Repeat("  ", r.depth)
		var line string
		switch {
		case r.note != "":
			line = " " + indent + m.st.dimText.Render(fileName(r.note))
		case r.entry.Dir:
			glyph := "▸ "
			if t.open[r.entry.Path] {
				glyph = "▾ "
			}
			line = " " + indent + m.st.dimText.Render(glyph) + m.st.name.Render(fileName(r.entry.Name)+"/")
		case r.entry.Link:
			line = " " + indent + "  " + m.gitName(r.entry) + m.st.dimText.Render(" → "+fileName(r.entry.Target))
		default:
			line = " " + indent + "  " + m.gitName(r.entry)
		}
		// What git says sits at the row's end, clear of a long name.
		if mark, _ := m.gitMark(r.entry); mark != "" && r.note == "" {
			line = padANSI(line, inner-2) + mark + " "
		}
		line = padANSI(line, inner)
		switch {
		case i == target:
			line = highlight(m.st, line, colHighlightBG)
		case i == t.cursor && focused && (m.filesDrag == nil || !m.filesDrag.moved):
			line = highlight(m.st, line, colHighlightBG)
		}
		m.zones.mark(hit{kind: hitFileRow, idx: i}, 1, y, inner, 1)
		out = append(out, side+line+side)
	}
	for len(out) < 2+listH {
		out = append(out, side+strings.Repeat(" ", inner)+side)
	}

	// The keys are buttons for themselves, as they are on the tree.
	type offer struct {
		text string
		act  filesAction
	}
	offers := []offer{{m.leader() + " " + filesPanelKey + " focus", filesFocus}}
	if focused {
		offers = []offer{{"esc back", filesBack}, {"r reload", filesReload}}
	}
	hintLine, x := " ", 1
	for i, o := range offers {
		if i > 0 {
			hintLine += m.st.dimText.Render(" · ")
			x += 3
		}
		w := lipgloss.Width(o.text)
		m.zones.mark(hit{kind: hitFiles, idx: int(o.act)}, x+1, len(out), w, 1)
		style := m.st.dimText
		if m.zones.hovering(x+1, len(out), w, 1) {
			style = m.st.hover
		}
		hintLine += style.Render(o.text)
		x += w
	}
	out = append(out, side+padANSI(hintLine, inner)+side)
	out = append(out, edge.Render("╰"+strings.Repeat("─", inner)+"╯"))

	m.zones.pop()
	m.filesZones = m.zones
	m.zones = saved
	out = append(append([]string{strings.Repeat(" ", filesWidth)}, out...), strings.Repeat(" ", filesWidth))
	if m.treeBlocked() {
		for i, line := range out {
			out[i] = m.st.greyedOut.Render(ansi.Strip(line))
		}
	}
	return lipgloss.NewStyle().Width(filesWidth).MaxHeight(m.height).Render(strings.Join(out, "\n"))
}
