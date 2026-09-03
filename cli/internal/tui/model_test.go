package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// The window opens ready to type: no key is needed before the first character
// of a prompt, and nothing in the list is picked out while the prompt has it.
func TestOpensInThePrompt(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	if m.focus != focusPrompt {
		t.Fatal("the window should open with the prompt focused")
	}
	send(t, m, typeString("fix the reaper")...)
	if got := m.prompt.Value(); got != "fix the reaper" {
		t.Fatalf("prompt = %q", got)
	}
	if strings.Contains(frameText(m), "❯ fix flaky") {
		t.Fatal("no row should wear the cursor while the prompt has focus")
	}
}

// Shift-Backspace is Backspace. A terminal on the Kitty keyboard protocol
// reports it as its own key, and a prompt that ignored it would be a prompt
// where deleting a character depends on where your left hand happens to be.
func TestShiftBackspaceDeletesInThePrompt(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	send(t, m, typeString("fix the reaperx")...)
	send(t, m, keyPress("shift+backspace"))
	if got := m.prompt.Value(); got != "fix the reaper" {
		t.Fatalf("prompt = %q, want shift+backspace to delete a character", got)
	}
	send(t, m, keyPress("backspace"))
	if got := m.prompt.Value(); got != "fix the reape" {
		t.Fatalf("prompt = %q, want backspace to delete a character", got)
	}
}

// Enter with a prompt is the whole point of the window: it creates the sandbox
// and attaches to it, and the prompt is spent.
func TestEnterRunsThePromptAndAttaches(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, typeString("fix the reaper")...)
	send(t, m, keyPress("enter"))

	if len(ds.runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(ds.runs))
	}
	if promptText(ds.runs[0]) != "fix the reaper" {
		t.Fatalf("prompt = %q", ds.runs[0].Prompt)
	}
	// Attaching is a terminal the window draws itself, not a command it steps
	// aside for.
	opened := ds.execOpened()
	if len(opened) != 1 || !strings.HasPrefix(opened[0], "sbx_created primary ") {
		t.Fatalf("execOpens = %v, want an attach on the new sandbox", opened)
	}
	if len(ds.opens) != 0 {
		t.Fatalf("opens = %v, want no command drawn over it", ds.opens)
	}
	if m.prompt.Value() != "" {
		t.Fatalf("the prompt should be spent, got %q", m.prompt.Value())
	}
}

// An empty prompt is not an error. It is the other thing you come here for: a
// sandbox with nothing given to the harness.
func TestEnterOnAnEmptyPromptStillCreates(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, keyPress("enter"))

	if len(ds.runs) != 1 || promptText(ds.runs[0]) != "" {
		t.Fatalf("runs = %+v, want one empty prompt", ds.runs)
	}
}

// --include-dirty=auto asks, and both answers are answers: the sandbox is
// created either way, from the working tree or from the last commit.
func TestDirtyWorkspaceIsAskedAboutAndBothAnswersCreate(t *testing.T) {
	for _, tc := range []struct {
		answer string
		want   string
	}{
		{"y", "true"},
		{"n", "false"},
		// Enter is no: the working tree is carried in only when asked for.
		{"enter", "false"},
	} {
		ds := newFakeSource()
		ds.workspace = SourceWorkspace{Directory: "/home/ada/src/web", Repository: true, Carries: true}
		m := newTestModel(t, ds)
		send(t, m, keyPress("enter"))
		if m.dialog == nil {
			t.Fatalf("%s: a dirty working tree should be asked about", tc.answer)
		}
		// The repository asked about is the one the run is cut from, which the
		// source option can point somewhere other than this window's directory.
		if !strings.Contains(m.dialog.body, "/home/ada/src/web") {
			t.Fatalf("%s: dialog body = %q, want the repository named", tc.answer, m.dialog.body)
		}
		send(t, m, keyPress(tc.answer))
		if len(ds.runs) != 1 {
			t.Fatalf("%s: runs = %d, want 1", tc.answer, len(ds.runs))
		}
		if ds.runs[0].IncludeDirty != tc.want {
			t.Fatalf("%s: includeDirty = %q, want %q", tc.answer, ds.runs[0].IncludeDirty, tc.want)
		}
	}
}

// A source directory in no Git repository is the same question with more at
// stake — the whole directory is what would be carried — so it is asked before
// anything is copied, and "no" still creates the discobox.
func TestDirectoryWithNoRepositoryIsAskedAboutBeforeItIsCopied(t *testing.T) {
	for _, tc := range []struct {
		answer string
		want   string
	}{
		{"y", "true"},
		{"n", "false"},
		// Enter is no: copying a home directory into a discobox is not
		// something to do by pressing Enter twice.
		{"enter", "false"},
	} {
		ds := newFakeSource()
		ds.workspace = SourceWorkspace{Directory: "/home/ada/notes", Carries: true}
		m := newTestModel(t, ds)
		send(t, m, keyPress("enter"))
		if m.dialog == nil {
			t.Fatalf("%s: a directory with no repository should be asked about", tc.answer)
		}
		if !strings.Contains(m.dialog.body, "/home/ada/notes") || !strings.Contains(m.dialog.body, "not a Git repository") {
			t.Fatalf("%s: dialog body = %q, want the directory named", tc.answer, m.dialog.body)
		}
		if m.dialog.emphasis == "" {
			t.Fatalf("%s: the question should lead with what copying would cost", tc.answer)
		}
		if got := ds.measuredDirs(); len(got) != 1 || got[0] != "/home/ada/notes" {
			t.Fatalf("%s: measured %v, want the directory being asked about", tc.answer, got)
		}
		send(t, m, keyPress(tc.answer))
		if len(ds.runs) != 1 {
			t.Fatalf("%s: runs = %d, want 1", tc.answer, len(ds.runs))
		}
		if ds.runs[0].IncludeDirty != tc.want {
			t.Fatalf("%s: includeDirty = %q, want %q", tc.answer, ds.runs[0].IncludeDirty, tc.want)
		}
		// The answer is in; nothing should still be reading the disk for it.
		send(t, m, directorySizeMsg{})
		if ds.stoppedCount() != 1 {
			t.Fatalf("%s: the walk was stopped %d times, want once", tc.answer, ds.stoppedCount())
		}
	}
}

// The size arrives while the question is up: it is asked immediately, with a
// number that climbs, and only the final one is stated as fact.
func TestTheDirectoryQuestionCountsWhileItIsUp(t *testing.T) {
	ds := newFakeSource()
	ds.workspace = SourceWorkspace{Directory: "/home/ada/notes", Carries: true}
	m := newTestModel(t, ds)
	send(t, m, keyPress("enter"))
	if m.dialog.emphasis != "calculating…" {
		t.Fatalf("emphasis = %q, want the walk to say it has nothing counted yet", m.dialog.emphasis)
	}

	ds.setTotal(DirectoryTotal{Bytes: 5 << 20, Files: 3})
	send(t, m, directorySizeMsg{})
	if m.dialog.emphasis != "5.0 MiB in 3 files, still counting…" {
		t.Fatalf("emphasis = %q, want the running total", m.dialog.emphasis)
	}
	if ds.stoppedCount() != 0 {
		t.Fatal("the walk was stopped while the question was still up")
	}

	ds.setTotal(DirectoryTotal{Bytes: 7 << 20, Files: 4, Done: true})
	send(t, m, directorySizeMsg{})
	if m.dialog.emphasis != "7.0 MiB in 4 files" {
		t.Fatalf("emphasis = %q, want the final total stated as one", m.dialog.emphasis)
	}
	if ds.stoppedCount() != 1 {
		t.Fatalf("the walk was stopped %d times once it had finished, want once", ds.stoppedCount())
	}
}

// An empty directory carries nothing, so both answers make the same discobox
// and there is nothing to ask.
func TestAnEmptyDirectoryWithNoRepositoryIsNotAskedAbout(t *testing.T) {
	ds := newFakeSource()
	ds.workspace = SourceWorkspace{Directory: "/home/ada/empty"}
	m := newTestModel(t, ds)
	send(t, m, keyPress("enter"))

	if m.dialog != nil {
		t.Fatal("an empty directory should not be asked about")
	}
	if len(ds.runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(ds.runs))
	}
	if got := ds.measuredDirs(); len(got) != 0 {
		t.Fatalf("measured %v, want nothing walked for a question nobody asked", got)
	}
}

// A clean working tree has nothing to ask about, so Enter goes straight through.
func TestCleanWorkspaceIsNotAskedAbout(t *testing.T) {
	ds := newFakeSource()
	m := newTestModel(t, ds)
	send(t, m, keyPress("enter"))
	if m.dialog != nil {
		t.Fatal("a clean working tree should not be asked about")
	}
	if len(ds.runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(ds.runs))
	}
}

// The window is a ladder — the folder filter above the list above the prompt —
// and the arrows climb it. Each end stops rather than wrapping: the prompt is
// the bottom, the filter is the top, and a key that jumped from one to the
// other would be moving the opposite way to what it says.
func TestTheArrowsClimbTheWindowAndStopAtItsEnds(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	send(t, m, keyPress("tab"))
	if m.focus != focusList {
		t.Fatal("tab should move to the list")
	}
	send(t, m, keyPress("up"))
	if m.focus != focusFolder {
		t.Fatal("up off the top of the list should reach the folder filter")
	}
	send(t, m, keyPress("up"))
	if m.focus != focusFolder {
		t.Fatal("up off the folder filter should stay: it is the top")
	}
	// Tab and Esc are still the way back from there.
	send(t, m, keyPress("esc"))
	if m.focus != focusPrompt {
		t.Fatal("esc from the folder filter should return to the prompt")
	}

	// Down at the prompt stays in it, however many times it is pressed.
	for range 3 {
		send(t, m, keyPress("down"))
	}
	if m.focus != focusPrompt {
		t.Fatal("down at the prompt should stay in the prompt")
	}

	// And down out of the bottom of the list still returns to it.
	send(t, m, keyPress("tab"))
	for range len(m.list.rows()) {
		send(t, m, keyPress("down"))
	}
	if m.focus != focusPrompt {
		t.Fatal("down past the end of the list should return to the prompt")
	}
}

// An empty list is exactly when the folder filter is the thing you want: the
// folder you are standing in has nothing in it and the sandboxes are elsewhere.
// Refusing to move would leave no way to reach the one control that helps.
func TestAnEmptyListLandsOnTheFolderFilter(t *testing.T) {
	m := newTestModel(t, newFakeSource(Sandbox{ID: "sbx_one", Name: "one", State: StateRunning, Folder: "/src/elsewhere"}))
	if len(m.list.rows()) != 0 {
		t.Fatalf("expected an empty list, got %d rows", len(m.list.rows()))
	}

	send(t, m, keyPress("tab"))
	if m.focus != focusFolder {
		t.Fatalf("focus = %v, want the folder filter", m.focus)
	}
	// And from there the other folder is one press away.
	send(t, m, keyPress("right"))
	if m.list.folder != "/src/elsewhere" {
		t.Fatalf("folder = %q, want the one with something in it", m.list.folder)
	}
	if len(m.list.rows()) != 1 {
		t.Fatalf("rows = %d, want the sandbox that was there all along", len(m.list.rows()))
	}
}

// A letter in the list is a command, and it acts on the row under the cursor.
func TestListLettersRunActions(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"))

	send(t, m, keyPress("s"))
	shelled := false
	for _, open := range ds.execOpened() {
		if strings.HasPrefix(open, "sbx_one exec_shell") {
			shelled = true
		}
	}
	if !shelled {
		t.Fatalf("execOpens = %v, want a shell in the box under the cursor", ds.execOpened())
	}
	// A shell keeps ctrl+c for itself, so the leader carries detach there.
	send(t, m, keyPress("ctrl+a"), keyPress("d"))
	send(t, m, keyPress("t"))
	if len(ds.did) != 1 || ds.did[0] != "stop sbx_one" {
		t.Fatalf("did = %v", ds.did)
	}
}

// Selection is what a command acts on, and it outlives the cursor moving.
func TestSelectionIsWhatCommandsActOn(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress(" "), keyPress("down"), keyPress(" "))

	if n := m.list.selectionCount(); n != 2 {
		t.Fatalf("selected %d, want 2", n)
	}
	send(t, m, keyPress("t"))
	if len(ds.did) != 2 || ds.did[0] != "stop sbx_one" || ds.did[1] != "stop sbx_two" {
		t.Fatalf("did = %v", ds.did)
	}
	// A verb that ran is what the marks were for, so they go with it.
	if n := m.list.selectionCount(); n != 0 {
		t.Fatalf("selection should be cleared after a verb, got %d", n)
	}
}

// V draws a range discobox-review-style, and a command acts on the whole of it.
func TestVisualRangeActsOnTheWholeRange(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	showAllFolders(t, m)
	send(t, m, keyPress("V"), keyPress("down"), keyPress("down"))

	if !m.list.visual {
		t.Fatal("V should start a visual range")
	}
	send(t, m, keyPress("x"))
	want := []string{"archive sbx_one", "archive sbx_two", "archive sbx_three"}
	if len(ds.did) != len(want) {
		t.Fatalf("did = %v, want %v", ds.did, want)
	}
	for i, w := range want {
		if ds.did[i] != w {
			t.Fatalf("did = %v, want %v", ds.did, want)
		}
	}
	if m.list.visual {
		t.Fatal("a command should end visual mode")
	}
}

// Purge destroys the disk, so it asks first — and archiving, which is
// reversible, does not.
func TestPurgeConfirmsAndArchiveDoesNot(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("A"), keyPress("G")) // the archived row is last

	if got := m.list.current(); got == nil || got.State != StateArchived {
		t.Fatalf("expected the cursor on an archived sandbox, got %+v", got)
	}
	send(t, m, keyPress("P"))
	if m.dialog == nil {
		t.Fatal("purge should ask first")
	}
	if len(ds.did) != 0 {
		t.Fatalf("nothing should have run before the answer, got %v", ds.did)
	}
	send(t, m, keyPress("y"))
	if len(ds.did) != 1 || ds.did[0] != "purge sbx_four" {
		t.Fatalf("did = %v", ds.did)
	}
}

// e opens the name for editing, and Enter sends the edited one.
func TestRenameEditsTheNameInPlace(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("e"))

	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatal("e should open an input dialog")
	}
	// The name it already has is what is being edited, not a blank line.
	if got := m.dialog.input.Value(); got != "fix flaky pool reaper tests" {
		t.Fatalf("input = %q, want the current name", got)
	}
	send(t, m, typeString(" again")...)
	send(t, m, keyPress("enter"))
	if len(ds.renames) != 1 || ds.renames[0] != "sbx_one fix flaky pool reaper tests again" {
		t.Fatalf("renames = %v", ds.renames)
	}
	if m.dialog != nil {
		t.Fatal("accepting the name should close the dialog")
	}
}

// Esc leaves the name alone, and so does Enter on the name it already had.
func TestRenameCancelsWithoutCalling(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("e"), keyPress("esc"))
	if len(ds.renames) != 0 {
		t.Fatalf("esc should rename nothing, got %v", ds.renames)
	}
	send(t, m, keyPress("e"), keyPress("enter"))
	if len(ds.renames) != 0 {
		t.Fatalf("an unchanged name should rename nothing, got %v", ds.renames)
	}
}

// Rename takes one box: a name is a name, and several rows cannot share one.
func TestRenameTakesExactlyOneBox(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress(" "), keyPress("down"), keyPress(" "), keyPress("e"))

	if m.dialog == nil || m.dialog.kind != dlgMessage {
		t.Fatalf("rename on a selection should say why, got %+v", m.dialog)
	}
	if len(ds.renames) != 0 {
		t.Fatalf("renames = %v", ds.renames)
	}
}

// A row named by its terminal's title refuses rename: the configured name is
// not the one on screen, so accepting a new one would visibly change nothing.
func TestRenameRefusedWhenNameIsTerminalTitle(t *testing.T) {
	boxes := testSandboxes()
	boxes[0].ConfigName = "brave-otter"
	ds := newFakeSource(boxes...)
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("e"))

	if m.dialog == nil || m.dialog.kind != dlgMessage {
		t.Fatalf("rename on a title-named row should say why, got %+v", m.dialog)
	}
	if len(ds.renames) != 0 {
		t.Fatalf("renames = %v", ds.renames)
	}
}

// An action that does not apply says why rather than doing nothing.
func TestUnavailableActionExplainsItself(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"))

	// The row under the cursor is running, so there is no upgrade on it.
	send(t, m, keyPress("."))
	if m.dialog == nil {
		t.Fatal(". should open the action menu")
	}
	if !strings.Contains(m.dialog.view(m.st, &m.zones, 120, 40), "already on the current image") {
		t.Fatal("the menu should keep upgrade, with the reason it is unavailable")
	}
}

// Diff, apply and status run git inside the sandbox, so an archived one cannot
// take them however much it changed — there is no container to run it in.
func TestArchivedSandboxesCannotBeDiffed(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("A"), keyPress("G"))

	for _, a := range m.actions(m.list.targets()) {
		switch a.key {
		case "d", "y", "i":
			if a.enabled {
				t.Errorf("%s should not be offered on an archived sandbox", a.label)
			}
			if !strings.Contains(a.why, "no working tree") {
				t.Errorf("%s should say why: %q", a.label, a.why)
			}
		}
	}
	send(t, m, keyPress("y"))
	if len(ds.opens) != 0 {
		t.Fatalf("nothing should have run, got %v", ds.opens)
	}
}

// Archived sandboxes are out of the way until A asks for them.
func TestArchivedSandboxesAreHiddenUntilAskedFor(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	showAllFolders(t, m)
	if got := len(m.list.rows()); got != 3 {
		t.Fatalf("rows = %d, want 3 with the archived one hidden", got)
	}
	send(t, m, keyPress("A"))
	if got := len(m.list.rows()); got != 4 {
		t.Fatalf("rows = %d, want 4 with archived shown", got)
	}
}

// The window opens on the folder it is running in, which is what `discobox ls`
// shows, and the header's dropdown is how you reach the others.
func TestTheFolderFilterOpensOnThisDirectory(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	if m.list.folder != "/src/disco2" {
		t.Fatalf("folder = %q, want the directory the window is running in", m.list.folder)
	}
	for _, s := range m.list.rows() {
		if s.Folder != "/src/disco2" {
			t.Fatalf("row from %q should be filtered out", s.Folder)
		}
	}

	// Left off the first choice wraps to "every folder", which is the one
	// choice that is not a path.
	send(t, m, keyPress("tab"), keyPress("up"), keyPress("left"))
	if m.list.folder != "" {
		t.Fatalf("folder = %q, want every folder", m.list.folder)
	}
	if got := len(m.list.rows()); got != 3 {
		t.Fatalf("rows = %d, want every unarchived sandbox", got)
	}

	// And on round to the other folder something was started from.
	send(t, m, keyPress("left"))
	if m.list.folder != "/src/obot" {
		t.Fatalf("folder = %q, want the other folder", m.list.folder)
	}
	if got := len(m.list.rows()); got != 1 {
		t.Fatalf("rows = %d, want just the one started there", got)
	}
}

// The dropdown lists every folder something was started from, plus the choice
// to drop the filter, and choosing one applies it.
func TestTheFolderDropdownListsTheKnownFolders(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	send(t, m, keyPress("tab"), keyPress("up"), keyPress("enter"))

	if m.dialog == nil {
		t.Fatal("enter on the folder filter should open the dropdown")
	}
	view := m.dialog.view(m.st, &m.zones, 120, 40)
	for _, want := range []string{"/src/disco2", "/src/obot", allFolders, "where this window is running"} {
		if !strings.Contains(view, want) {
			t.Errorf("the dropdown is missing %q:\n%s", want, view)
		}
	}

	// The second choice is the other folder; picking it filters to it.
	send(t, m, keyPress("down"), keyPress("enter"))
	if m.list.folder != "/src/obot" {
		t.Fatalf("folder = %q, want the choice that was made", m.list.folder)
	}
	// And the cursor is back at the top: the rows under it are a different set
	// of sandboxes now.
	if m.list.cursor != 0 {
		t.Fatalf("cursor = %d, want the top of the new list", m.list.cursor)
	}
}

// A folder with nothing in it is still worth offering when it is the one the
// window is running in — it is where a new sandbox would be created.
func TestTheFolderFilterAlwaysOffersThisDirectory(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	choices := m.folderChoices()
	if len(choices) != 2 || choices[0] != "/src/disco2" || choices[1] != allFolders {
		t.Fatalf("choices = %v", choices)
	}
}

// A refresh keeps the cursor on the sandbox it was on, not on the row number:
// a list that reorders under you must not move the cursor onto a different
// sandbox between the key press and the action.
func TestRefreshKeepsTheCursorOnItsSandbox(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("down"))
	if m.list.current().ID != "sbx_two" {
		t.Fatalf("cursor on %s", m.list.current().ID)
	}

	reordered := testSandboxes()
	reordered[0], reordered[1] = reordered[1], reordered[0]
	ds.sandboxes = reordered
	send(t, m, tickMsg{})

	if got := m.list.current().ID; got != "sbx_two" {
		t.Fatalf("cursor moved to %s after a refresh", got)
	}
}

// The diffstat arrives on the row with the listing itself — the agent
// reports it with the rest of its status — so a refresh carries it like any
// other field and nothing is fetched on the list's behalf.
func TestDiffstatArrivesWithTheListing(t *testing.T) {
	ds := newFakeSource(Sandbox{
		ID: "sbx_one", Name: "one", State: StateRunning, Folder: "/src/disco2",
		Diff: DiffStat{Known: true, Added: 3, Deleted: 1, Files: 2},
	})
	m := newTestModel(t, ds)

	if got := m.list.all[0].Diff; !got.Known || got.Added != 3 {
		t.Fatalf("diffstat = %+v, want the listed one", got)
	}
	send(t, m, tickMsg{})
	if got := m.list.all[0].Diff; !got.Known || got.Added != 3 {
		t.Fatalf("diffstat = %+v after refresh, want the listed one kept", got)
	}
}

// A listing that fails says so, and does not take the window down with it.
func TestListFailureIsReported(t *testing.T) {
	ds := newFakeSource()
	ds.listErr = errors.New("no server")
	m := newTestModel(t, ds)
	send(t, m, tickMsg{})

	if !m.statusE || !strings.Contains(m.status, "no server") {
		t.Fatalf("status = %q (error %v), want the listing failure", m.status, m.statusE)
	}
}

// Ctrl-D on an empty prompt quits the way a shell does, and does not when there
// is something in the buffer to delete forward over.
func TestCtrlDQuitsOnlyOnAnEmptyPrompt(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	send(t, m, typeString("something")...)
	send(t, m, keyPress("ctrl+d"))
	if m.quit {
		t.Fatal("ctrl+d with text in the prompt should not quit")
	}
	m.prompt.SetValue("")
	send(t, m, keyPress("ctrl+d"))
	if !m.quit {
		t.Fatal("ctrl+d on an empty prompt should quit")
	}
}

// The window takes the whole terminal, so what was on screen before it started
// is left exactly as it was and comes back when it exits.
func TestTheWindowIsFullScreen(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	if !m.View().AltScreen {
		t.Fatal("the launcher should take the alternate screen")
	}
	// Including the layers that stand in place of it.
	send(t, m, keyPress("f1"))
	if !m.View().AltScreen {
		t.Fatal("a dialog should stay on the alternate screen")
	}
}

var _ tea.Model = (*Model)(nil)

// The header is one control: the folder it is on is both which sandboxes are
// listed and where a new one is created. Switching it switches both.
func TestSwitchingFolderSwitchesWhereTheRunHappens(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)

	// On the window's own directory, the run names no source at all: that is
	// already what `discobox run` would use, and passing it would only repeat it.
	if req := m.opts.request(""); req.Source != "" {
		t.Fatalf("source = %q, want the CLI's own default", req.Source)
	}

	send(t, m, keyPress("tab"), keyPress("up"), keyPress("right")) // on to the other folder
	if m.list.folder != "/src/obot" {
		t.Fatalf("folder = %q", m.list.folder)
	}
	if req := m.opts.request(""); req.Source != "/src/obot" {
		t.Fatalf("source = %q, want the folder the header moved to", req.Source)
	}
	if !strings.Contains(m.opts.command(""), "-C /src/obot") {
		t.Fatalf("command should carry the folder: %q", m.opts.command(""))
	}

	// And it is what Enter actually asks for.
	send(t, m, keyPress("esc"))
	send(t, m, keyPress("enter"))
	if len(ds.runs) != 1 || ds.runs[0].Source != "/src/obot" {
		t.Fatalf("runs = %+v, want the folder the header is on", ds.runs)
	}
}

// The chip strip says what Enter will do that the rest of the window does not
// already say. The header names the folder, so the source is a chip only when
// it differs from it.
func TestTheSourceChipOnlyShowsWhenItDiffers(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	if chips := m.opts.chips(m.st); strings.Contains(chips, "/src/disco2") {
		t.Errorf("the strip repeats the header: %q", chips)
	}

	// Moving the header moves the source with it, so it still says nothing.
	send(t, m, keyPress("tab"), keyPress("up"), keyPress("right"), keyPress("esc"))
	if chips := m.opts.chips(m.st); strings.Contains(chips, "/src/obot") {
		t.Errorf("the strip repeats the header after a switch: %q", chips)
	}

	// An override is the one case worth a chip: the window is listing one
	// folder and about to create in another.
	m.opts.chooseSource("/src/elsewhere@main")
	if chips := m.opts.chips(m.st); !strings.Contains(chips, "/src/elsewhere@main") {
		t.Errorf("an override should be on the strip: %q", chips)
	}
}

// The list sits above the prompt, so Up out of the prompt is a cursor moving
// into the row directly above it: the last one. Tab is not a direction, so it
// lands at the top.
func TestUpFromThePromptLandsOnTheLastRow(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	send(t, m, keyPress("up"))
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
	if want := len(m.list.rows()) - 1; m.list.cursor != want {
		t.Fatalf("cursor = %d, want the last row %d", m.list.cursor, want)
	}
	if want := "sbx_two"; m.list.current().ID != want {
		t.Fatalf("cursor on %s, want %s", m.list.current().ID, want)
	}
}

// ↑ leaves the prompt only while the prompt is empty. With anything typed it
// is a cursor key in the text you are writing: holding it to walk back up a
// paragraph should stop at the top, not open the list up behind your words.
// Tab is the way out, and it is a key you press once and mean.
func TestUpStaysInThePromptWithTextInIt(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	send(t, m, typeString("one")...)
	send(t, m, keyPress("ctrl+j"))
	send(t, m, typeString("two")...)
	// Up walks the text: the second line to the first, then to its start.
	for range 4 {
		send(t, m, keyPress("up"))
		if m.focus != focusPrompt {
			t.Fatalf("focus = %v, want the prompt: it has text in it", m.focus)
		}
	}
	if got := m.prompt.Value(); got != "one\ntwo" {
		t.Fatalf("prompt = %q, want the text untouched", got)
	}
	// Tab still leaves, and the text goes with it.
	send(t, m, keyPress("tab"))
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list: tab is the way out", m.focus)
	}

	// Emptied again, ↑ is a way out once more.
	send(t, m, keyPress("esc"))
	send(t, m, keyPress("down"), keyPress("down")) // back to the end of the text
	for range len("one\ntwo") {
		send(t, m, keyPress("backspace"))
	}
	if got := m.prompt.Value(); got != "" {
		t.Fatalf("prompt = %q, want it emptied", got)
	}
	send(t, m, keyPress("up"))
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list: an empty prompt still lets ↑ out", m.focus)
	}
}

// ↓ ends at the prompt from wherever it is pressed. A folder with nothing in it
// is nothing to move through, not a dead end: the empty list says to type a
// prompt, so ↓ goes there rather than refusing.
func TestDownReachesThePromptThroughAnEmptyList(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	send(t, m, keyPress("tab"))
	if m.focus != focusFolder {
		t.Fatalf("focus = %v, want the folder filter: there are no rows to land on", m.focus)
	}

	send(t, m, keyPress("down"))
	if m.focus != focusPrompt {
		t.Fatalf("focus = %v, want the prompt", m.focus)
	}
}

func TestTabFromThePromptLandsOnTheFirstRow(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	send(t, m, keyPress("tab"))
	if m.list.cursor != 0 {
		t.Fatalf("cursor = %d, want the first row", m.list.cursor)
	}
}

// Leaving the list to type something and coming back is not the same as
// arriving at it: the cursor goes back to the sandbox it was left on, whichever
// key brings it back.
func TestComingBackToTheListKeepsTheCursor(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	showAllFolders(t, m)
	send(t, m, keyPress("down")) // row 1, chosen deliberately
	on := m.list.current().ID

	for _, back := range []string{"up", "tab"} {
		send(t, m, keyPress("esc")) // out to the prompt
		if m.focus != focusPrompt {
			t.Fatalf("%s: expected to be in the prompt", back)
		}
		send(t, m, keyPress(back))
		if m.focus != focusList {
			t.Fatalf("%s: focus = %v, want the list", back, m.focus)
		}
		if got := m.list.current().ID; got != on {
			t.Fatalf("%s: cursor on %s, want %s", back, got, on)
		}
	}
}

// A different set of sandboxes is a list nobody has chosen a row in, so Up
// lands at its end again rather than on whatever row number was last used.
func TestSwitchingFolderForgetsTheCursor(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	showAllFolders(t, m)
	send(t, m, keyPress("down"))
	if !m.list.visited {
		t.Fatal("moving the cursor should count as having been in the list")
	}

	send(t, m, keyPress("up"), keyPress("up"), keyPress("right")) // to the folder, on to the next
	if m.list.visited {
		t.Fatal("a new set of sandboxes is a list nobody has chosen a row in")
	}
}

// The leader is configurable — see the keys package for what a leader may be —
// and it is the key the pane actually reserves, and the one the key lists name.
func TestLeaderReachesThePaneAndTheKeyLists(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	t.Setenv("NO_COLOR", "1")
	m := New(t.Context(), ds, WithLeader("ctrl+b"))
	m.logo = logo{}
	d := newDriver(t, m)
	d.start()
	d.wait("the listing", func() bool { return len(m.list.rows()) > 0 })
	d.key("tab")
	d.key("enter")
	d.wait("the pane", func() bool { return m.focus == focusPane })

	if !strings.Contains(hintLine(m.hints()), "ctrl+b d detach") {
		t.Errorf("the key list should name the leader: %q", hintLine(m.hints()))
	}
	if !strings.Contains(m.helpText(), "ctrl+b d") {
		t.Error("the help should name the leader")
	}

	// The configured leader arms the pane; the default one no longer does.
	term := ds.terminals[0]
	d.key("ctrl+a")
	if got := term.typed("\x01"); !strings.Contains(got, "\x01") {
		t.Fatalf("typed %q, want the default leader to reach the sandbox", got)
	}
	d.key("ctrl+b")
	d.key("d")
	d.wait("the workspace to close", func() bool { return !m.inPanes() })
}

// Tab goes round the window in the order it is drawn, bottom to top: the
// prompt, the discoboxes, the folder they are filtered to, and back. Esc is the
// way straight out from anywhere.
func TestTabGoesRoundTheWindow(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	for _, want := range []focusArea{focusList, focusFolder, focusPrompt, focusList} {
		send(t, m, keyPress("tab"))
		if m.focus != want {
			t.Fatalf("tab landed on %v, want %v", m.focus, want)
		}
	}

	// And Esc is the short way back, from either stop.
	send(t, m, keyPress("esc"))
	if m.focus != focusPrompt {
		t.Fatalf("esc from the list landed on %v", m.focus)
	}
	send(t, m, keyPress("tab"), keyPress("tab")) // prompt -> list -> folder
	if m.focus != focusFolder {
		t.Fatalf("focus = %v, want the folder filter", m.focus)
	}
	send(t, m, keyPress("esc"))
	if m.focus != focusPrompt {
		t.Fatalf("esc from the folder filter landed on %v", m.focus)
	}
}

// ---------------------------------------------------------------------------
// vscode

// TestVSCodeOpensTheBoxUnderTheCursor: v is a request that returns, so the
// window stays where it was and says what happened on its status line.
func TestVSCodeOpensTheBoxUnderTheCursor(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("v"))

	if len(ds.editors) != 1 || ds.editors[0] != "sbx_one" {
		t.Fatalf("editors = %v", ds.editors)
	}
	if m.dialog != nil {
		t.Fatalf("v should open nothing over the window, got %+v", m.dialog)
	}
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list it was pressed from", m.focus)
	}
	if !strings.Contains(m.status, "VS Code") {
		t.Fatalf("status = %q, want it to report the window that opened", m.status)
	}
}

// A failure to reach the editor is the status line's, not a dialog's: nothing
// about the box changed, and there is nothing to answer.
func TestVSCodeReportsFailureOnTheStatusLine(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.editorErr = errors.New("no VS Code command found on PATH")
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("v"))

	if !m.statusE || !strings.Contains(m.status, "no VS Code command") {
		t.Fatalf("status = %q (error %v), want the failure reported", m.status, m.statusE)
	}
}

// An archived box has no container to run an editor server in, so v is refused
// with the reason rather than sent and failed.
func TestVSCodeRefusedOnAnArchivedBox(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	// The archived row is the last one, and only shown once A asks for it.
	send(t, m, keyPress("tab"), keyPress("A"), keyPress("G"), keyPress("v"))

	if len(ds.editors) != 0 {
		t.Fatalf("an archived box should not reach the editor, got %v", ds.editors)
	}
	if m.dialog == nil || m.dialog.kind != dlgMessage {
		t.Fatalf("v on an archived box should say why, got %+v", m.dialog)
	}
}

// wedgedSandbox is what repair is for: a settled failure the reconciler will
// never retry on its own (ADR 0017 §4).
func wedgedSandbox() Sandbox {
	return Sandbox{
		ID: "sbx_wedged", Name: "wedged", State: StateError, HasRuntime: true,
		Harness: "claude", Folder: "/src/disco2", Branch: "main", Commit: "a3f9c21",
		Message: "create failed: no such file or directory",
	}
}

// R repairs the discobox under the cursor, so recovering a wedged one never
// means leaving the window for `discobox admin sandbox repair`.
func TestRepairRunsOnAWedgedBox(t *testing.T) {
	ds := newFakeSource(wedgedSandbox())
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("R"))

	if len(ds.did) != 1 || ds.did[0] != "repair sbx_wedged" {
		t.Fatalf("did = %v, want a repair on the box under the cursor", ds.did)
	}
}

// Repair rebuilds, so it is offered on the two shapes that need rebuilding and
// refused — with the reason, on the menu — on a box that is working.
func TestRepairIsRefusedOnAHealthyBox(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"))

	send(t, m, keyPress("R"))
	if len(ds.did) != 0 {
		t.Fatalf("did = %v, want nothing: the box under the cursor is running", ds.did)
	}
	if m.dialog == nil || !strings.Contains(m.dialog.view(m.st, &m.zones, 120, 40), "nothing is wrong with it") {
		t.Fatal("a refused repair should say why")
	}
}

// An archived discobox is unarchived, not repaired — the server refuses it for
// the same reason (ADR 0035), so the reason names the action that does work.
func TestRepairPointsAnArchivedBoxAtUnarchive(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("A"), keyPress("G"))

	for _, a := range m.actions(m.list.targets()) {
		if a.key != repairKey {
			continue
		}
		if a.enabled {
			t.Fatal("repair should be refused on an archived box")
		}
		if !strings.Contains(a.why, "unarchive") {
			t.Fatalf("why = %q, want it to point at unarchive", a.why)
		}
		return
	}
	t.Fatal("repair should stay on the menu with its reason")
}

// The repaint is the window's own key, not a pane's: it redraws from whatever
// screen it is pressed on, and costs that screen nothing.
func TestRepaintWorksOffThePanes(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	send(t, m, typeString("fix the reaper")...)

	_, cmd := m.Update(keyPress("ctrl+l"))
	if !repaints(cmd) {
		t.Fatal("ctrl+l in the prompt should redraw the window")
	}
	if got := m.prompt.Value(); got != "fix the reaper" {
		t.Fatalf("prompt = %q, want the repaint to leave it alone", got)
	}
}
