package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/discobox-ai/discobox/tools"
)

// openTool opens the workspace and runs one tool from the picker, waiting until
// its window is the screen.
func openTool(t *testing.T, ds *fakeSource, key string) (*driver, *Model) {
	t.Helper()
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key(toolsKey)
	waitPicker(d, m)
	d.key(key)
	d.wait("the tool window", func() bool { return m.showingTool() != nil })
	return d, m
}

// waitPicker waits for the picker, and for the tools on it: they are the
// discobox's to say, and arrive after the card does.
func waitPicker(d *driver, m *Model) {
	d.wait("the picker", func() bool { return m.dialog != nil && m.dialog.title == toolsTitle })
	d.wait("the tools", func() bool {
		_, known := m.toolCatalogs[m.currentBox().ID]
		return known
	})
}

// openPicker opens the workspace and puts the tools picker on screen.
func openPicker(t *testing.T, ds *fakeSource) (*driver, *Model) {
	t.Helper()
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key(toolsKey)
	waitPicker(d, m)
	return d, m
}

// The picker prints the two ways into a discobox that are not this window, and
// a press takes the whole line: what makes them worth a row is reading them and
// seeing that there is nothing else to them.
func TestTheToolsPickerPrintsAndCopiesTheAddresses(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openPicker(t, ds)
	copies := make(chan string, 2)
	m.copyOS = func(text string) error { copies <- text; return nil }

	d.wait("the addresses", func() bool { return strings.Contains(frameText(m), "ssh sbx_one") })
	if frame := frameText(m); !strings.Contains(frame, "ssh://sbx_one/home/discobox/repo") {
		t.Fatalf("the picker should print the git URL too:\n%s", frame)
	}
	if got := ds.addressLookups(); len(got) != 1 || got[0] != "sbx_one" {
		t.Fatalf("address lookups = %v, want one for sbx_one", got)
	}

	// Clicking the row is the whole gesture: the point of printing an address
	// is that it can be taken without knowing a key for it.
	x, y := at(t, m, "ssh://sbx_one/home/discobox/repo")
	d.dispatch(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.wait("the copy", func() bool { return len(copies) > 0 })
	if got := <-copies; got != "ssh://sbx_one/home/discobox/repo" {
		t.Fatalf("copied %q, want the git URL", got)
	}

	// The card stays up and the row it came from says so, which is the whole
	// reason it stays: a receipt on a status line under a card that had just
	// been taken away is one nobody reads.
	if m.dialog == nil {
		t.Fatal("copying an address should leave the card up")
	}
	d.wait("the receipt", func() bool {
		return strings.Contains(frameText(m), "ssh://sbx_one/home/discobox/repo  copied")
	})
	if strings.Contains(frameText(m), "ssh sbx_one  copied") {
		t.Error("the receipt belongs to the row it was earned on")
	}

	// And the address beside it is still one press away, on the same card.
	d.key(addressSSHKey)
	d.wait("the second copy", func() bool { return len(copies) > 0 })
	if got := <-copies; got != "ssh sbx_one" {
		t.Fatalf("copied %q, want the ssh command", got)
	}
	d.key("esc")
	d.wait("the card to close", func() bool { return m.dialog == nil })

	// Resolved once and remembered: reopening the card does not go and write
	// the ssh config a second time, and it opens with no receipt on it.
	d.key("ctrl+a")
	d.key(toolsKey)
	d.wait("the picker again", func() bool { return m.dialog != nil })
	if strings.Contains(frameText(m), "copied") {
		t.Error("a reopened card should carry no receipt from the last one")
	}
	if got := ds.addressLookups(); len(got) != 1 {
		t.Errorf("address lookups = %v, want the first one reused", got)
	}
}

// A lookup that failed says so on the rows it could not fill, and is tried
// again the next time the card is opened — what it failed at is the kind of
// thing that stops being true.
func TestTheToolsPickerRetriesAFailedAddressLookup(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.addressErr = errors.New("no ssh_config to write")
	d, m := openPicker(t, ds)

	d.wait("the reason", func() bool { return strings.Contains(frameText(m), "no ssh_config to write") })
	if strings.Contains(frameText(m), "ssh sbx_one") {
		t.Error("a failed lookup should print no address")
	}

	ds.mu.Lock()
	ds.addressErr = nil
	ds.mu.Unlock()
	d.key("esc")
	d.wait("the card to close", func() bool { return m.dialog == nil })
	d.key("ctrl+a")
	d.key(toolsKey)
	d.wait("the retry", func() bool { return strings.Contains(frameText(m), "ssh sbx_one") })
	if got := ds.addressLookups(); len(got) != 2 {
		t.Fatalf("address lookups = %v, want the failure retried", got)
	}
}

// The picker runs the tool in the discobox, as an exec session in its primary
// source directory — nothing on this machine — and gives it the whole window.
func TestTheToolsPickerRunsDiffInTheBox(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openTool(t, ds, "d")

	if got := ds.toolRunsSeen(); len(got) != 1 || got[0] != tools.DiffID {
		t.Fatalf("tool runs = %v, want discobox-review as the diff tool", got)
	}
	p := m.showingTool()
	if p.tool != tools.DiffID {
		t.Fatalf("showing %q, want the diff tool", p.tool)
	}
	// The whole window, over the workspace that is still attached underneath.
	if got := m.paneWidthOf(p); got != m.width {
		t.Errorf("tool width = %d, want the window's %d", got, m.width)
	}
	if m.primary() == nil {
		t.Error("the workspace should still be attached under the tool")
	}
	ds.execTerm(p.execID).send("changed files")
	d.wait("the tool's output", func() bool { return strings.Contains(frameText(m), "changed files") })
	if frame := frameText(m); !strings.Contains(frame, "[-]") || !strings.Contains(frame, "[x]") {
		t.Errorf("the tool window should wear its buttons:\n%s", frame)
	}
}

// Minimizing puts the window away and leaves the session running: nothing is
// ended, and choosing the tool again shows the same pane rather than starting a
// second one.
func TestMinimizingAToolKeepsItsSessionAndReopensIt(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openTool(t, ds, "d")
	opened := m.showingTool()

	d.key("ctrl+a")
	d.key(paneDetachAlt)
	d.wait("the workspace back", func() bool { return m.showingTool() == nil })

	if !m.inPanes() {
		t.Fatal("putting a tool away should leave the workspace up")
	}
	if got := ds.endedExecs(); len(got) != 0 {
		t.Fatalf("ended = %v, want a put-away tool to keep running", got)
	}
	if m.toolPane(tools.DiffID) == nil {
		t.Fatal("the tool pane should still be attached while it is put away")
	}

	d.key("ctrl+a")
	d.key(toolsKey)
	waitPicker(d, m)
	d.key("d")
	d.wait("the tool window again", func() bool { return m.showingTool() != nil })

	if m.showingTool() != opened {
		t.Error("reopening a tool should show the pane it already had")
	}
	if got := ds.toolRunsSeen(); len(got) != 1 {
		t.Fatalf("tool runs = %v, want reopening to start nothing", got)
	}
}

// Closing is the other button: it ends the session in the discobox, which is
// the one thing the window does that a detach never does.
func TestClosingAToolEndsItsSession(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openTool(t, ds, "f")
	execID := m.showingTool().execID

	d.key("ctrl+a")
	d.key(toolCloseKey)
	d.wait("the session ended", func() bool { return len(ds.endedExecs()) == 1 })

	if got := ds.endedExecs(); got[0] != execID {
		t.Fatalf("ended %v, want the tool's own session %s", got, execID)
	}
	if m.toolPane("fresh") != nil {
		t.Error("a closed tool should leave no pane behind")
	}
	if m.showingTool() != nil {
		t.Error("closing a tool should give the window back to the workspace")
	}
	if !m.inPanes() {
		t.Fatal("closing a tool should leave the workspace up")
	}
}

// Quitting the tool itself is the third way out, and it is the ordinary one:
// the window goes when the program does, rather than holding a dead screen the
// reader has to dismiss before the workspace comes back.
func TestAToolThatExitsTakesItsWindowWithIt(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openTool(t, ds, "d")
	term := ds.execTerm(m.showingTool().execID)

	term.Close()
	d.wait("the workspace back", func() bool { return m.showingTool() == nil })

	if m.toolPane(tools.DiffID) != nil {
		t.Error("a tool that exited should leave no pane behind")
	}
	if !m.inPanes() {
		t.Fatal("a tool exiting should leave the workspace up")
	}
	if got := ds.endedExecs(); len(got) != 0 {
		t.Fatalf("ended = %v, want nothing ended for a session that ended itself", got)
	}
}

// The [-] and [x] on the border are the same two things, reachable with a
// mouse: a press on one has to mean that button and not the pane under it.
func TestTheToolWindowButtonsMinimizeAndClose(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openTool(t, ds, "d")

	minimize, closeAt := buttonColumns(t, m)
	clickAt(d, minimize, 1)
	d.settle()
	if m.showingTool() != nil {
		t.Fatal("[-] should put the tool away")
	}
	if got := ds.endedExecs(); len(got) != 0 {
		t.Fatalf("ended = %v, want [-] to end nothing", got)
	}

	m.showTool(m.toolPane(tools.DiffID))
	m.View()
	clickAt(d, closeAt, 1)
	d.wait("the session ended", func() bool { return len(ds.endedExecs()) == 1 })
	if m.toolPane(tools.DiffID) != nil {
		t.Error("[x] should close the tool as well as ending it")
	}
}

// buttonColumns is where the showing tool window drew its two buttons, read off
// the frame it just drew — the spans are recorded by the drawing pass.
func buttonColumns(t *testing.T, m *Model) (minimize, closeAt int) {
	t.Helper()
	m.View()
	if len(m.buttonSpans) != 2 {
		t.Fatalf("buttonSpans = %v, want the two the tool window draws", m.buttonSpans)
	}
	for _, span := range m.buttonSpans {
		if span.action == buttonMinimize {
			minimize = span.start + 1
		} else {
			closeAt = span.start + 1
		}
	}
	return minimize, closeAt
}

// A tool session outlives the window that opened it, so a window that attaches
// to a discobox with one already running picks it back up — put away, because
// attaching to a discobox should show you the discobox.
func TestARunningToolIsPickedUpOnAttach(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.execs = []Exec{{
		ID: "exec_diff", Command: []string{"discobox-review"}, Tool: tools.DiffID,
		Tty: true, Live: true, CreatedAt: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
	}}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the running tool", func() bool { return m.toolPane(tools.DiffID) != nil })

	if m.showingTool() != nil {
		t.Error("a tool picked up off the listing should arrive put away")
	}
	if m.shells.len() != 0 || m.terminals.len() != 1 {
		t.Errorf("a tool should be neither a shell nor a terminal: %d shells, %d terminals",
			m.shells.len(), m.terminals.len())
	}
	if got := m.toolPane(tools.DiffID).execID; got != "exec_diff" {
		t.Errorf("tool exec = %q, want the session already running", got)
	}

	d.key("ctrl+a")
	d.key(toolsKey)
	waitPicker(d, m)
	if body := m.dialog.view(m.st, &m.zones, m.width, m.height); !strings.Contains(body, "running") {
		t.Errorf("the picker should say which tools are up:\n%s", body)
	}
	d.key("d")
	d.wait("the tool window", func() bool { return m.showingTool() != nil })
	if got := ds.toolRunsSeen(); len(got) != 0 {
		t.Fatalf("tool runs = %v, want a picked-up tool to start nothing", got)
	}
}

// Detaching closes the window onto the tools the way it closes the window onto
// everything else: the streams go, and the sessions keep running.
func TestDetachingLeavesTheToolSessionsRunning(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openTool(t, ds, "d")

	d.key("ctrl+a")
	d.key(paneDetachAlt) // put the tool away
	d.wait("the workspace back", func() bool { return m.showingTool() == nil })
	d.key("ctrl+a")
	d.key(paneDetachAlt) // and leave the workspace
	d.wait("the list", func() bool { return !m.inPanes() })

	if m.tools.len() != 0 {
		t.Error("detaching should close this window's view of every tool")
	}
	if got := ds.endedExecs(); len(got) != 0 {
		t.Fatalf("ended = %v, want detaching to end nothing", got)
	}
}

// Launching a tool carries its files into the discobox, before the session
// starts: the tool reads its configuration when it comes up.
func TestLaunchingAToolCarriesItsConfigIn(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	_, m := openTool(t, ds, "f")

	want := []string{
		"fresh/config.jsonc → .config/fresh/config.json",
		"fresh/live_diff.json → .local/share/fresh/orchestrator/state/live_diff.json",
	}
	got := ds.installedFiles()
	if len(got) != len(want) {
		t.Fatalf("installed = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("installed[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if m.showingTool() == nil {
		t.Error("the tool should still open once its config is in place")
	}
}

// A tool that carries nothing carries nothing — the picker's second key is
// offered on every row, so the ones with no config have to say so rather than
// look broken.
func TestATooWithNoConfigSaysSo(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(toolsKey)
	waitPicker(d, m)
	d.key(toolFileKey) // the cursor is on diff, the first row
	d.wait("the report", func() bool { return m.status != "" })

	if !strings.Contains(m.status, "no config") {
		t.Fatalf("status = %q, want it to say diff carries no config", m.status)
	}
	if got := ds.editedToolFiles(); len(got) != 0 {
		t.Fatalf("edited = %v, want nothing opened", got)
	}
}

// The picker's second key opens the highlighted tool's config in $EDITOR. One
// file means no second menu: a list with a single row is a press to answer a
// question that has one answer.
func TestThePickerEditsTheHighlightedToolsConfig(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.editToolFile = func(ToolFile) string { return `{"theme":"mine"}` }
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(toolsKey)
	waitPicker(d, m)
	d.key("down") // onto fresh
	d.key(toolFileKey)
	// fresh carries more than one file, so the key opens the list first.
	d.wait("the file list", func() bool {
		return m.dialog != nil && strings.Contains(m.dialog.title, "Config")
	})
	d.key("1") // config.jsonc
	d.wait("the editor", func() bool { return len(ds.editedToolFiles()) == 1 })

	if got := ds.editedToolFiles(); got[0] != "fresh/config.jsonc" {
		t.Fatalf("edited %v, want fresh's config", got)
	}
	d.wait("the report", func() bool { return strings.Contains(m.status, "config.json") })
	// What it did not do is the surprising half, so the line says it.
	if !strings.Contains(m.status, "next discobox") {
		t.Errorf("status = %q, want it to say an open box keeps its own copy", m.status)
	}
	if !m.inPanes() {
		t.Error("editing a config should leave the workspace up")
	}
}

// An editor that saved nothing changed nothing, and the line says that rather
// than claiming a save.
func TestAnUneditedConfigReportsUnchanged(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.editToolFile = func(file ToolFile) string { return file.Default }
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(toolsKey)
	waitPicker(d, m)
	d.key("down")
	d.key(toolFileKey)
	d.wait("the file list", func() bool { return m.dialog != nil })
	d.key("1")
	d.wait("the report", func() bool { return strings.Contains(m.status, "unchanged") })
}

// A tool carrying several files gets a list, and each row has to say what the
// file is and where it lands — the local name and the delivered path differ, and
// a row that showed only one of them would be a row you cannot act on.
func TestTheFileListNamesEveryFileAndWhereItGoes(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(toolsKey)
	waitPicker(d, m)
	d.key("down") // onto fresh
	d.key(toolFileKey)
	d.wait("the file list", func() bool {
		return m.dialog != nil && strings.Contains(m.dialog.title, "Config")
	})

	body := m.dialog.view(m.st, &m.zones, m.width, m.height)
	for _, want := range []string{
		"config.jsonc", "~/.config/fresh/config.json",
		"live_diff.json", "~/.local/share/fresh/orchestrator/state/live_diff.json",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the list should mention %q:\n%s", want, body)
		}
	}
}

// The workspace's hints line is where the picker is advertised — it is the only
// place, so a window that never showed it is a picker nobody opens.
func TestTheWorkspaceHintsOfferTheToolsPicker(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	_, m, _ := openWorkspace(t, ds, "enter")

	want := "ctrl+a " + toolsKey + " tools"
	if got := hintLine(m.hints()); !strings.Contains(got, want) {
		t.Fatalf("hints = %q, want it to offer %q", got, want)
	}
	if !strings.Contains(frameText(m), want) {
		t.Error("the hint should be on the frame, not only in the string")
	}
}

// That row is one row and stays one row: it drops whole hints from the end
// rather than wrapping onto a second, which would come out of the panes. The
// dropping itself is statusKeys' (fitFields); what is asserted here is that the
// workspace's own line survives it — no hint cut mid-word, and the way out never
// among the casualties.
func TestTheHintsLineDropsRatherThanOverrunning(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openTool(t, ds, "d")
	d.key("ctrl+a")
	d.key(paneDetachAlt) // back to the workspace, which has the busiest line
	d.wait("the workspace", func() bool { return m.showingTool() == nil })
	d.key("ctrl+a")
	d.key("s")
	d.wait("a shell", func() bool { return m.shells.len() > 0 })

	var narrowest string
	for _, w := range []int{160, 120, 100, 80, 60} {
		m.width = w
		m.layout()
		// The room the workspace actually gives the row; see viewPaneWindow.
		room := max(max(w-2, 1)-2*boxPad, 1)
		line := m.statusKeys(room)
		if lipgloss.Width(line) > room {
			t.Fatalf("at w=%d the line is %d cells wide, over the %d it has:\n%s",
				w, lipgloss.Width(line), room, line)
		}
		// Whole fragments go, so no hint is ever left half-written.
		for _, part := range strings.Split(line, hintSep) {
			if strings.HasSuffix(part, "…") {
				t.Errorf("at w=%d a hint was cut mid-word: %q", w, part)
			}
		}
		narrowest = line
	}
	// The way out is the one thing that never goes.
	if !strings.Contains(narrowest, m.detachHint()) {
		t.Errorf("the narrowest row lost the way out: %q", narrowest)
	}
}

// The picker is the discobox's catalog, not a table of this package's: a tool
// the box declares is a row, one that cannot run is a row that says why, and a
// key two declarations ask for goes to the first of them.
func TestThePickerListsTheDiscoboxsCatalog(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.catalog = append(testTools(),
		Tool{ID: "review", Key: "d", Label: "review", Detail: "the repository's own reviewer"},
		Tool{ID: "open", Key: "w", Label: "open", Detail: "open it here", Problem: "a tool that runs on your machine cannot be declared by the discobox's source"},
	)
	d, m := openPicker(t, ds)

	frame := frameText(m)
	for _, want := range []string{"review", "the repository's own reviewer", "cannot be declared by the discobox's source"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the picker should show %q:\n%s", want, frame)
		}
	}
	var review, open action
	for _, it := range m.dialog.items {
		switch it.key {
		case toolRowKey("review"):
			review = it
		case toolRowKey("open"):
			open = it
		}
	}
	if review.press != "" {
		t.Errorf("review took d from diff: %+v", review)
	}
	if open.enabled {
		t.Errorf("a tool with a problem is offered: %+v", open)
	}

	// d is still diff's.
	d.key("d")
	d.wait("the diff", func() bool { return m.showingTool() != nil })
	if got := ds.toolRunsSeen(); len(got) != 1 || got[0] != tools.DiffID {
		t.Fatalf("tool runs = %v, want diff", got)
	}
}

// A repository that declares its own vscode is refused, and the refusal must
// not cost the real one: no second row under the id, and the key stays the
// host tool's even when the refused file sorts first.
func TestARefusedReplacementDoesNotTakeTheHostToolsRowOrKey(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.catalog = append([]Tool{
		{ID: "vscode", Key: "v", Label: "vscode", Problem: "id \"vscode\" is a tool that runs on your machine, declared by builtin; the discobox's source cannot replace it"},
		{ID: "lint", Key: "z", Label: "lint", Detail: "the repository's linter"},
	}, testTools()...)
	d, m := openPicker(t, ds)

	var rows int
	for _, it := range m.dialog.items {
		switch it.key {
		case toolRowKey("vscode"):
			rows++
			if !it.enabled || it.press != "v" {
				t.Errorf("vscode row = %+v, want the runnable one on v", it)
			}
		case toolRowKey("lint"):
			if it.press != "" {
				t.Errorf("the box's lint took %q from zed", it.press)
			}
		}
	}
	if rows != 1 {
		t.Fatalf("vscode has %d rows, want 1", rows)
	}
	d.key("v")
	d.wait("vscode", func() bool { return len(ds.openedEditors()) == 1 })
	if got := ds.openedEditors(); got[0] != (editorOpen{id: "sbx_one", tool: "vscode"}) {
		t.Fatalf("ran %v, want the host vscode", got)
	}
}

// A click on the git summary that waited for the tools lookup reports the
// lookup's own failure, not a tool it could not find in no catalog.
func TestAGitSummaryClickReportsAFailedToolLookup(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.toolsErr = errors.New("sandbox has no container on this pool: it is being rebuilt, or it needs repair")
	d, m, _ := openWorkspace(t, ds, "enter")

	clickAt(d, headerCol(t, m, "main@a3f9c21")+2, 0)
	d.wait("the report", func() bool { return strings.Contains(m.status, "needs repair") })
	if strings.Contains(m.status, "no such tool") {
		t.Fatalf("status = %q", m.status)
	}
	if got := ds.toolRunsSeen(); len(got) != 0 {
		t.Fatalf("tool runs = %v, want none", got)
	}
}

// A lookup that already failed is not a catalog to run against: the next click
// asks again, and reports why it still fails.
func TestAGitSummaryClickRetriesAFailedToolLookup(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.toolsErr = errors.New("sandbox has no container on this pool: it is being rebuilt, or it needs repair")
	d, m, _ := openWorkspace(t, ds, "enter")

	col := headerCol(t, m, "main@a3f9c21") + 2
	clickAt(d, col, 0)
	d.wait("the first report", func() bool { return strings.Contains(m.status, "needs repair") })
	m.status = ""
	clickAt(d, col, 0)
	d.wait("the second report", func() bool { return m.status != "" })
	if !strings.Contains(m.status, "needs repair") {
		t.Fatalf("second click status = %q, want the lookup's reason again", m.status)
	}
	ds.mu.Lock()
	lookups := len(ds.toolLookups)
	ds.mu.Unlock()
	if lookups < 2 {
		t.Fatalf("lookups = %d, want the second click to ask again", lookups)
	}
}
