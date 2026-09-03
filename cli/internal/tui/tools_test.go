package tui

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// openTool opens the workspace and runs one tool from the picker, waiting until
// its window is the screen.
func openTool(t *testing.T, ds *fakeSource, key string) (*driver, *Model) {
	t.Helper()
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key(toolsKey)
	d.wait("the picker", func() bool { return m.dialog != nil })
	d.key(key)
	d.wait("the tool window", func() bool { return m.showingTool() != nil })
	return d, m
}

// openPicker opens the workspace and puts the tools picker on screen.
func openPicker(t *testing.T, ds *fakeSource) (*driver, *Model) {
	t.Helper()
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key(toolsKey)
	d.wait("the picker", func() bool { return m.dialog != nil })
	return d, m
}

// The picker prints the two ways into a discobox that are not this window, and
// a press takes the whole line: what makes them worth a row is reading them and
// seeing that there is nothing else to them.
func TestTheToolsPickerPrintsAndCopiesTheAddresses(t *testing.T) {
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
	ds := newFakeSource(testSandboxes()...)
	d, m := openTool(t, ds, "d")

	if got := ds.toolRunsSeen(); len(got) != 1 || got[0] != "diff discobox-review" {
		t.Fatalf("tool runs = %v, want discobox-review as the diff tool", got)
	}
	p := m.showingTool()
	if p.tool != "diff" {
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
	if m.toolPane("diff") == nil {
		t.Fatal("the tool pane should still be attached while it is put away")
	}

	d.key("ctrl+a")
	d.key(toolsKey)
	d.wait("the picker", func() bool { return m.dialog != nil })
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
	ds := newFakeSource(testSandboxes()...)
	d, m := openTool(t, ds, "d")
	term := ds.execTerm(m.showingTool().execID)

	term.Close()
	d.wait("the workspace back", func() bool { return m.showingTool() == nil })

	if m.toolPane("diff") != nil {
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

	m.showTool(m.toolPane("diff"))
	m.View()
	clickAt(d, closeAt, 1)
	d.wait("the session ended", func() bool { return len(ds.endedExecs()) == 1 })
	if m.toolPane("diff") != nil {
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
	ds := newFakeSource(testSandboxes()...)
	ds.execs = []Exec{{
		ID: "exec_diff", Command: []string{"discobox-review"}, Tool: "diff",
		Tty: true, Live: true, CreatedAt: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
	}}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the running tool", func() bool { return m.toolPane("diff") != nil })

	if m.showingTool() != nil {
		t.Error("a tool picked up off the listing should arrive put away")
	}
	if m.shells.len() != 0 || m.terminals.len() != 1 {
		t.Errorf("a tool should be neither a shell nor a terminal: %d shells, %d terminals",
			m.shells.len(), m.terminals.len())
	}
	if got := m.toolPane("diff").execID; got != "exec_diff" {
		t.Errorf("tool exec = %q, want the session already running", got)
	}

	d.key("ctrl+a")
	d.key(toolsKey)
	d.wait("the picker", func() bool { return m.dialog != nil })
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

// The tools that run in the discobox are the ones the picker lists with a
// command; vscode is the odd one and is listed anyway, on the key it has in the
// list, so there is one place to ask "open this box in X".
func TestEveryToolIsReachableByItsKey(t *testing.T) {
	seen := map[string]bool{}
	for _, tool := range tools {
		if seen[tool.key] {
			t.Fatalf("two tools answer to %q", tool.key)
		}
		seen[tool.key] = true
		if tool.id == "" || tool.label == "" || tool.detail == "" {
			t.Errorf("tool %q is not fully described: %+v", tool.key, tool)
		}
	}
	if _, ok := toolByID("vscode"); !ok {
		t.Error("vscode should be one of the tools")
	}
	if t2, _ := toolByID("vscode"); t2.session() {
		t.Error("vscode is a request that returns, not a session in the box")
	}
}

// Launching a tool carries its files into the discobox, before the session
// starts: the tool reads its configuration when it comes up.
func TestLaunchingAToolCarriesItsConfigIn(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	_, m := openTool(t, ds, "f")

	want := []string{
		"fresh/config.jsonc → .config/fresh/config.json",
		"fresh/live_diff.json → .local/share/fresh/orchestrator/state/live_diff.json",
		"fresh/trust.json → .local/share/fresh/workspaces/{workspace}/trust.json",
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
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(toolsKey)
	d.wait("the picker", func() bool { return m.dialog != nil })
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
	ds := newFakeSource(testSandboxes()...)
	ds.editToolFile = func(ToolFile) string { return `{"theme":"mine"}` }
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(toolsKey)
	d.wait("the picker", func() bool { return m.dialog != nil })
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
	ds := newFakeSource(testSandboxes()...)
	ds.editToolFile = func(file ToolFile) string { return file.Default }
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(toolsKey)
	d.wait("the picker", func() bool { return m.dialog != nil })
	d.key("down")
	d.key(toolFileKey)
	d.wait("the file list", func() bool { return m.dialog != nil })
	d.key("1")
	d.wait("the report", func() bool { return strings.Contains(m.status, "unchanged") })
}

// A config file has to tell an editor what it is, twice over: the copy on this
// machine says it in its extension, and the copy inside the discobox — whose
// name fresh dictates — says it in a modeline, because a .json file full of
// comments is a screenful of syntax errors otherwise.
func TestTheFreshConfigDeclaresItsFormat(t *testing.T) {
	// The name fresh reads is not negotiable; the local one is ours to pick.
	if freshConfig.Home != ".config/fresh/config.json" {
		t.Errorf("Home = %q, want the path fresh actually reads", freshConfig.Home)
	}
	if !strings.HasSuffix(freshConfig.Name, ".jsonc") {
		t.Errorf("Name = %q, want a .jsonc extension so editors color it", freshConfig.Name)
	}

	lines := strings.Split(freshConfig.Default, "\n")
	// Vim reads modelines from the first five lines, so a modeline further down
	// is a modeline that does nothing.
	var found bool
	for i, line := range lines {
		if i >= 5 {
			break
		}
		if strings.Contains(line, "vim:") {
			found = true
			if !strings.Contains(line, "ft=jsonc") {
				t.Errorf("line %d sets no jsonc filetype: %q", i+1, line)
			}
			// The set form's options end at a colon; without it vim reads the
			// rest of the line as more options and gives up on the lot.
			if !strings.Contains(line, "set ") || !strings.HasSuffix(strings.TrimSpace(line), ":") {
				t.Errorf("line %d is not a well-formed set modeline: %q", i+1, line)
			}
		}
	}
	if !found {
		t.Errorf("no modeline in the first five lines:\n%s", strings.Join(lines[:min(5, len(lines))], "\n"))
	}
	// It has to still be JSONC: a comment before the top-level value, which is
	// what every JSONC parser allows.
	if body := strings.TrimSpace(freshConfig.Default); !strings.Contains(body, "{") {
		t.Error("the default carries no object at all")
	}
	if strings.Index(freshConfig.Default, "{") < strings.Index(freshConfig.Default, "vim:") {
		t.Error("the modeline must come before the object, where a comment is legal")
	}
}

// A tool carrying several files gets a list, and each row has to say what the
// file is and where it lands — the local name and the delivered path differ, and
// a row that showed only one of them would be a row you cannot act on.
func TestTheFileListNamesEveryFileAndWhereItGoes(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(toolsKey)
	d.wait("the picker", func() bool { return m.dialog != nil })
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

// fresh has to be handed the directory. Given no argument it opens an empty
// buffer with no file tree — the one thing you want from an editor pointed at a
// project — so the argument is the feature, and it regresses silently.
func TestFreshIsLaunchedOnItsDirectory(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	openTool(t, ds, "f")

	if got := ds.toolRunsSeen(); len(got) != 1 || got[0] != "fresh fresh ." {
		t.Fatalf("tool runs = %v, want fresh given a directory to open", got)
	}
}

// The live-diff seed is plugin state, not config: it is parsed with a strict
// JSON reader, so a comment in it would take the whole file — and the enable
// with it — silently.
func TestTheLiveDiffSeedIsStrictJSON(t *testing.T) {
	if strings.Contains(freshLiveDiff.Default, "//") {
		t.Fatalf("a comment would make this unparseable:\n%s", freshLiveDiff.Default)
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(freshLiveDiff.Default), &state); err != nil {
		t.Fatalf("does not parse as strict JSON: %v", err)
	}
	// Flat map of key to value, which is the shape fresh reads it back as.
	if got := state["live_diff.global_enabled"]; got != true {
		t.Errorf("global_enabled = %v, want true — the plugin is opt-in", got)
	}
	mode, ok := state["live_diff.default_mode"].(map[string]any)
	if !ok || mode["kind"] != "head" {
		t.Errorf("default_mode = %v, want the HEAD revision", state["live_diff.default_mode"])
	}
	// It lands under the data dir, beside the plugin's own name, not in config.
	if !strings.HasSuffix(freshLiveDiff.Home, "/orchestrator/state/live_diff.json") {
		t.Errorf("Home = %q, want the per-plugin state path fresh reads", freshLiveDiff.Home)
	}
}

// Every declared file has to be complete enough to deliver and to name.
func TestEveryToolFileIsDeliverable(t *testing.T) {
	for _, tool := range tools {
		for _, file := range tool.files {
			if file.Tool != tool.id {
				t.Errorf("%s carries a file owned by %q", tool.id, file.Tool)
			}
			if file.Name == "" || file.Home == "" {
				t.Errorf("%s/%s needs both a name and a home path: %+v", tool.id, file.Name, file)
			}
			if strings.HasPrefix(file.Home, "/") || strings.HasPrefix(file.Home, "~") {
				t.Errorf("%s/%s: Home is relative to the run user's home, got %q",
					tool.id, file.Name, file.Home)
			}
			if strings.TrimSpace(file.Default) == "" {
				t.Errorf("%s/%s has no default to start from", tool.id, file.Name)
			}
		}
	}
}

// The workspace's hints line is where the picker is advertised — it is the only
// place, so a window that never showed it is a picker nobody opens.
func TestTheWorkspaceHintsOfferTheToolsPicker(t *testing.T) {
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
// dropping itself is statusLine's (fitFields); what is asserted here is that the
// workspace's own line survives it — no hint cut mid-word, and the way out never
// among the casualties.
func TestTheHintsLineDropsRatherThanOverrunning(t *testing.T) {
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
		line := m.statusLine(room)
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
