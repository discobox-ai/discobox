package tui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// filesConsole is a console wide enough for the tree, the files panel and a
// workspace between them.
const filesConsole = treeWidth + filesWidth + 100

// filesSource is a fake whose primary starts in /src, holding a directory and
// a file, the directory holding a file of its own.
func filesSource() *fakeSource {
	ds := newFakeSource(testSandboxes()...)
	ds.execs = []Exec{{ID: "exec_pri", Harness: "claude", Primary: true, Tty: true, Live: true, Workdir: "/src"}}
	ds.files = map[string][]FileEntry{
		"/src": {
			{Name: "README.md", Path: "/src/README.md"},
			{Name: "cmd", Path: "/src/cmd", Dir: true},
		},
		"/src/cmd": {
			{Name: "main.go", Path: "/src/cmd/main.go"},
		},
	}
	return ds
}

// openFiles opens a workspace on filesSource and the files panel beside it.
func openFiles(t *testing.T) (*driver, *Model, *fakeSource, *fakeTerminal) {
	t.Helper()
	ds := filesSource()
	d, m, term := openWorkspaceAt(t, ds, "enter", filesConsole)
	d.wait("the files root", func() bool { return m.fileTree.root == "/src" })
	d.leader(filesPanelKey)
	d.wait("the files panel", func() bool { return filesRowAt(m, "README.md") >= 0 })
	return d, m, ds, term
}

// filesRowAt is the screen row the panel draws text on, or -1.
func filesRowAt(m *Model, text string) int {
	for y, line := range strings.Split(plainFrame(m), "\n") {
		if strings.Contains(ansi.TruncateLeft(line, m.filesLeft(), ""), text) {
			return y
		}
	}
	return -1
}

// pressFilesRow presses the left button on the panel's row showing text.
func pressFilesRow(t *testing.T, d *driver, m *Model, text string) (x, y int) {
	t.Helper()
	y = filesRowAt(m, text)
	if y < 0 {
		t.Fatalf("%q is not in the files panel:\n%s", text, plainFrame(m))
	}
	x = m.filesLeft() + 4
	d.dispatch(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	return x, y
}

func TestTheFilesPanelOpensBesideTheWorkspaceAtTheSource(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		_, m, _, _ := openFiles(t)
		if !m.filesShown() || !m.filesHasKeys() {
			t.Fatal("the leader's F should open the files panel with the keys")
		}
		if want := filesConsole - treeWidth - filesWidth; m.width != want {
			t.Fatalf("window %d cells, want %d: the panel's cells come off it", m.width, want)
		}
		lines := strings.Split(plainFrame(m), "\n")
		if top := ansi.TruncateLeft(lines[1], m.filesLeft(), ""); !strings.HasPrefix(top, "╭") || !strings.Contains(top, "files") {
			t.Fatalf("the panel's box should start level with the workspace's: %q", top)
		}
		if y := filesRowAt(m, "/src"); y < 0 {
			t.Fatal("the panel should name its root")
		}
		// Directories come first.
		if filesRowAt(m, "cmd/") > filesRowAt(m, "README.md") {
			t.Fatalf("directories should be listed before files:\n%s", plainFrame(m))
		}
		for i, line := range lines {
			if w := ansi.StringWidth(line); w != filesConsole {
				t.Fatalf("frame line %d is %d cells, want %d", i, w, filesConsole)
			}
		}
	})
}

func TestTheFilesPanelsKeysOpenADirectoryAndPasteAPath(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d, m, _, term := openFiles(t)
		// cmd/ is the first row; Enter opens it.
		d.key("enter")
		d.wait("cmd opened", func() bool { return filesRowAt(m, "main.go") >= 0 })
		d.key("down")
		d.key("enter")
		if got := term.typed("/src/cmd/main.go"); !strings.Contains(got, "/src/cmd/main.go") {
			t.Fatalf("Enter on a file should paste its path into the pane; typed %q", got)
		}
		// Esc gives the keys back, and typing reaches the pane again.
		d.key("esc")
		if m.filesHasKeys() {
			t.Fatal("Esc should give the keys back to the panes")
		}
		d.key("z")
		if got := term.typed("z"); !strings.HasSuffix(got, "z") {
			t.Fatalf("a key after Esc should reach the pane; typed %q", got)
		}
	})
}

func TestTheLeaderFromTheFilesPanelIsTheWorkspaces(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d, m, _, _ := openFiles(t)
		d.leader(filesPanelKey)
		if m.filesOpen || m.filesShown() {
			t.Fatal("the leader's F from the panel should put it away")
		}
		if want := filesConsole - treeWidth; m.width != want {
			t.Fatalf("window %d cells, want %d once the panel is away", m.width, want)
		}
		d.leader(filesPanelKey)
		d.wait("the panel back", func() bool { return m.filesHasKeys() })
		// Another leader key is the focused pane's chord: z maximizes, which
		// has nothing beside it to maximize over, and says so.
		d.leader(paneZoomKey)
		if m.filesFront {
			t.Fatal("a workspace chord from the panel should hand the keys back")
		}
	})
}

func TestDraggingAFileOntoADirectoryMovesIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d, m, ds, _ := openFiles(t)
		x, y := pressFilesRow(t, d, m, "README.md")
		to := filesRowAt(m, "cmd/")
		d.dispatch(tea.MouseMotionMsg{X: x, Y: to, Button: tea.MouseLeft})
		d.dispatch(tea.MouseReleaseMsg{X: x, Y: to, Button: tea.MouseLeft})
		d.wait("the move", func() bool { return len(ds.transfersSoFar()) == 1 })
		got := ds.transfersSoFar()[0]
		if got.from != "/src/README.md" || got.to != "/src/cmd/README.md" || got.duplicate {
			t.Fatalf("transfer = %+v, want README.md moved into cmd", got)
		}
		d.wait("cmd opened", func() bool { return filesRowAt(m, "main.go") >= 0 })

		// With Ctrl held it is a copy.
		pressFilesRow(t, d, m, "main.go")
		d.dispatch(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft})
		d.dispatch(tea.MouseReleaseMsg{X: x, Y: filesRowAt(m, "README.md"), Button: tea.MouseLeft, Mod: tea.ModCtrl})
		d.wait("the copy", func() bool { return len(ds.transfersSoFar()) == 2 })
		if got := ds.transfersSoFar()[1]; got.from != "/src/cmd/main.go" || got.to != "/src/main.go" || !got.duplicate {
			t.Fatalf("transfer = %+v, want main.go copied up to /src", got)
		}
	})
}

func TestDraggingAFileOntoAPanePastesItsPath(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d, m, _, term := openFiles(t)
		pressFilesRow(t, d, m, "README.md")
		x, y := screenOrigin(m, m.primary())
		d.dispatch(tea.MouseMotionMsg{X: x + 3, Y: y + 2, Button: tea.MouseLeft})
		d.dispatch(tea.MouseReleaseMsg{X: x + 3, Y: y + 2, Button: tea.MouseLeft})
		if got := term.typed("/src/README.md"); !strings.Contains(got, "/src/README.md") {
			t.Fatalf("a row dropped on the pane should paste its path; typed %q", got)
		}
		if m.filesHasKeys() {
			t.Fatal("the pane dropped on should have the keys")
		}
	})
}

func TestFilesDroppedFromThisMachineAreCopiedIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	local := filepath.Join(dir, "my notes.txt")
	if err := os.WriteFile(local, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		d, m, ds, term := openFiles(t)
		// The cursor on cmd/ is where a drop with the keys lands. Anything in
		// a box can put paths on the clipboard, so a drop lists what it would
		// copy and copies nothing until Enter; any other key drops it.
		d.dispatch(tea.PasteMsg{Content: "'" + local + "'"})
		d.wait("the drop to wait for enter", func() bool { return m.filesDrop != nil })
		if view := ansi.Strip(m.viewFiles()); !strings.Contains(view, "my notes.txt") || !strings.Contains(view, "enter copy") {
			t.Fatalf("a waiting drop should show what it copies and how to confirm; panel:\n%s", view)
		}
		d.key("j")
		d.settle()
		if m.filesDrop != nil || len(ds.uploadsSoFar()) != 0 {
			t.Fatalf("a key other than enter should copy nothing; uploads %+v", ds.uploadsSoFar())
		}
		d.dispatch(tea.PasteMsg{Content: "'" + local + "'"})
		d.wait("the drop to wait for enter", func() bool { return m.filesDrop != nil })
		// The keys leaving the panel let the drop go too: coming back finds
		// the tree, not a list one Enter would still send.
		d.key("ctrl+a")
		d.key("h")
		d.settle()
		if m.filesDrop != nil {
			t.Fatal("a drop should not outlive the panel's keys")
		}
		d.key("ctrl+a")
		d.key(filesPanelKey)
		d.settle()
		if view := ansi.Strip(m.viewFiles()); strings.Contains(view, "copy into") || len(ds.uploadsSoFar()) != 0 {
			t.Fatalf("back in the panel, the drop should be gone; uploads %+v, panel:\n%s", ds.uploadsSoFar(), view)
		}
		// A click on the tree takes the keys by a path of its own through
		// Update; the drop goes with them, and its offer copies nothing.
		if !m.treeShown() {
			t.Fatal("the workspace should be drawn beside its tree")
		}
		d.dispatch(tea.PasteMsg{Content: "'" + local + "'"})
		d.wait("the drop to wait for enter", func() bool { return m.filesDrop != nil })
		d.dispatch(tea.MouseClickMsg{X: 1, Y: 3, Button: tea.MouseLeft})
		d.dispatch(tea.MouseReleaseMsg{X: 1, Y: 3, Button: tea.MouseLeft})
		d.settle()
		if m.filesDrop != nil || m.filesHasKeys() {
			t.Fatalf("a click on the tree should take the keys and the drop; keys %v", m.filesHasKeys())
		}
		m.filesAct(filesDropCopy)
		// Back in the panel by a click on it, with the cursor where it was.
		x, y := m.filesLeft()+3, filesRowAt(m, "cmd/")
		d.dispatch(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		d.dispatch(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
		d.settle()
		if !m.filesHasKeys() || len(ds.uploadsSoFar()) != 0 {
			t.Fatalf("a drop left behind should copy nothing; uploads %+v", ds.uploadsSoFar())
		}
		d.dispatch(tea.PasteMsg{Content: "'" + local + "'"})
		d.wait("the drop to wait for enter", func() bool { return m.filesDrop != nil })
		d.key("enter")
		d.wait("the upload", func() bool { return len(ds.uploadsSoFar()) == 1 })
		if got := ds.uploadsSoFar()[0]; !slices.Equal(got.local, []string{local}) || got.dir != "/src/cmd" {
			t.Fatalf("upload = %+v, want %q into /src/cmd", got, local)
		}
		if got := term.typed(local); strings.Contains(got, local) {
			t.Fatalf("a drop should not also reach the pane; typed %q", got)
		}
		// Text pasted into the panel is not a drop, and is not typed anywhere.
		d.dispatch(tea.PasteMsg{Content: "hello there"})
		d.settle()
		if len(ds.uploadsSoFar()) != 1 {
			t.Fatal("text is not a drop")
		}
		// With the keys back in the pane, the same path is the pane's paste,
		// wherever the pointer was.
		d.dispatch(tea.MouseMotionMsg{X: m.filesLeft() + 3, Y: filesRowAt(m, "README.md")})
		d.key("esc")
		d.dispatch(tea.PasteMsg{Content: local})
		if got := term.typed(local); !strings.Contains(got, local) {
			t.Fatalf("a paste with the pane's keys should reach the pane; typed %q", got)
		}
		if len(ds.uploadsSoFar()) != 1 {
			t.Fatal("a paste into the pane is not a drop")
		}
	})
}

// A drag let go of outside the window never sees its release; the next press
// starts afresh rather than dropping the file wherever it lands.
func TestADragWhoseReleaseWasLostMovesNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d, m, ds, _ := openFiles(t)
		x, y := pressFilesRow(t, d, m, "README.md")
		d.dispatch(tea.MouseMotionMsg{X: x, Y: y + 1, Button: tea.MouseLeft})
		// The pointer comes back with the button up.
		to := filesRowAt(m, "cmd/")
		d.dispatch(tea.MouseMotionMsg{X: x, Y: to})
		d.dispatch(tea.MouseClickMsg{X: x, Y: to, Button: tea.MouseLeft})
		d.dispatch(tea.MouseReleaseMsg{X: x, Y: to, Button: tea.MouseLeft})
		d.settle()
		if got := ds.transfersSoFar(); len(got) != 0 {
			t.Fatalf("a lost release should move nothing; transfers %+v", got)
		}
		// The click was a click: it opened cmd.
		d.wait("cmd opened", func() bool { return m.fileTree.open["/src/cmd"] })
	})
}

// The box menu offers the panel beside services, tools and audit: choosing it
// opens the panel with the keys, and with the panel up the same entry puts it
// away.
func TestTheBoxMenuOpensAndClosesTheFilesPanel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := filesSource()
		d, m, _ := openWorkspaceAt(t, ds, "enter", filesConsole)
		d.wait("the files root", func() bool { return m.fileTree.root == "/src" })

		d.leader(boxMenuKey)
		if !slices.Contains(boxMenuLabels(t, m), "file tree") {
			t.Fatalf("actions = %q, want the file tree", boxMenuLabels(t, m))
		}
		d.key(filesPanelKey)
		d.wait("the files panel", func() bool { return filesRowAt(m, "README.md") >= 0 })
		if !m.filesHasKeys() {
			t.Fatal("the menu's file tree should open the panel with the keys")
		}

		d.key("esc")
		d.leader(boxMenuKey)
		if !slices.Contains(boxMenuLabels(t, m), "close file tree") {
			t.Fatalf("actions = %q, want the panel's entry to close it", boxMenuLabels(t, m))
		}
		d.key(filesPanelKey)
		d.settle()
		if m.filesOpen || m.filesShown() {
			t.Fatal("the menu's close file tree should put the panel away")
		}
	})
}

// On a window with no room for it the entry is there, but says why not.
func TestTheBoxMenusFileTreeSaysWhenTheWindowIsTooNarrow(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := filesSource()
		d, m, _ := openWorkspaceAt(t, ds, "enter", 120)
		d.leader(boxMenuKey)
		if !slices.Contains(boxMenuLabels(t, m), "file tree") {
			t.Fatalf("actions = %q, want the file tree listed", boxMenuLabels(t, m))
		}
		d.key(filesPanelKey)
		d.settle()
		if m.filesOpen {
			t.Fatal("a disabled entry should not open the panel")
		}
	})
}

const (
	gitTop    = "git rev-parse --show-toplevel"
	gitStatus = "git status --porcelain=v1 -z --untracked-files=all"
)

// gitFilesSource is filesSource in a repository whose README is modified and
// with a new file under cmd.
func gitFilesSource() *fakeSource {
	ds := filesSource()
	ds.files["/src/cmd"] = append(ds.files["/src/cmd"], FileEntry{Name: "new.go", Path: "/src/cmd/new.go"})
	ds.commands = map[string]CommandResult{
		gitTop:    {Stdout: "/src\n"},
		gitStatus: {Stdout: " M README.md\x00?? cmd/new.go\x00"},
	}
	return ds
}

// rowEnd is the panel's row showing text, trimmed of its border and the
// space before it.
func rowEnd(t *testing.T, m *Model, text string) string {
	t.Helper()
	y := filesRowAt(m, text)
	if y < 0 {
		t.Fatalf("%q is not in the files panel:\n%s", text, plainFrame(m))
	}
	line := ansi.TruncateLeft(strings.Split(plainFrame(m), "\n")[y], m.filesLeft(), "")
	return strings.TrimRight(strings.TrimSuffix(strings.TrimRight(line, " "), "│"), " ")
}

func TestParsePorcelainMarksFilesAndTheDirectoriesAboveThem(t *testing.T) {
	t.Parallel()
	// The git that answers is the box's: a byte that is no letter of git's
	// (an ESC, a C1 control) draws nothing.
	out := " M a/b/c.go\x00A  a/new.go\x00R  a/moved.go\x00a/old.go\x00?? d/e.txt\x00UU a/b/x.go\x00!! ignored\x00\x1b\x1b esc.go\x00 \x9b csi.go\x00"
	files, dirs := parsePorcelain("/repo", out)
	want := map[string]byte{"/repo/a/b/c.go": 'M', "/repo/a/new.go": 'A', "/repo/a/moved.go": 'R', "/repo/d/e.txt": '?', "/repo/a/b/x.go": 'U'}
	if len(files) != len(want) {
		t.Fatalf("files = %q, want %q", files, want)
	}
	for p, l := range want {
		if files[p] != l {
			t.Errorf("files[%s] = %q, want %q", p, files[p], l)
		}
	}
	if _, ok := files["/repo/a/old.go"]; ok {
		t.Error("a rename's old path is gone from the tree and is not drawn")
	}
	// The strongest change beneath a directory wins: the conflict.
	if dirs["/repo/a"] != 'U' || dirs["/repo/a/b"] != 'U' || dirs["/repo/d"] != '?' || dirs["/repo"] != 'U' {
		t.Fatalf("dirs = %q", dirs)
	}
	if f, d := parsePorcelain("", out); len(f) != 0 || len(d) != 0 {
		t.Fatal("no repository draws nothing")
	}
}

func TestTheFilesPanelDrawsWhatGitSays(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := gitFilesSource()
		d, m, _ := openWorkspaceAt(t, ds, "enter", filesConsole)
		d.wait("the files root", func() bool { return m.fileTree.root == "/src" })
		d.leader(filesPanelKey)
		d.wait("git's status", func() bool { return m.fileTree.git.files["/src/README.md"] == 'M' })
		if end := rowEnd(t, m, "README.md"); !strings.HasSuffix(end, "M") {
			t.Fatalf("README.md's row should end with git's M: %q", end)
		}
		if end := rowEnd(t, m, "cmd/"); !strings.HasSuffix(end, "•") {
			t.Fatalf("cmd/ has a change beneath it and should carry a dot: %q", end)
		}
		d.key("enter")
		d.wait("cmd opened", func() bool { return filesRowAt(m, "new.go") >= 0 })
		if end := rowEnd(t, m, "new.go"); !strings.HasSuffix(end, "?") {
			t.Fatalf("an untracked file should end with git's ?: %q", end)
		}
		if end := rowEnd(t, m, "main.go"); strings.HasSuffix(end, "M") || strings.HasSuffix(end, "?") {
			t.Fatalf("an unchanged file carries no mark: %q", end)
		}
	})
}

// Git is asked again when the listing says the box's git state moved, and not
// on every poll while it has not.
func TestTheFilesPanelAsksGitAgainOnlyWhenTheBoxReportsAChange(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := gitFilesSource()
		d, m, _ := openWorkspaceAt(t, ds, "enter", filesConsole)
		d.wait("the files root", func() bool { return m.fileTree.root == "/src" })
		d.leader(filesPanelKey)
		d.wait("git's status", func() bool { return len(m.fileTree.git.files) == 2 })
		asked := len(ds.ranSoFar())

		d.dispatch(workspaceTickMsg{gen: m.wsGen})
		d.settle()
		if got := len(ds.ranSoFar()); got != asked {
			t.Fatalf("an unchanged box was asked again: %d commands, want %d", got, asked)
		}

		for i := range m.list.all {
			if m.boxRef(m.list.all[i]) == m.boxRef(m.paneBox) {
				m.list.all[i].Diff = DiffStat{Known: true, Files: 3, Added: 7}
			}
		}
		ds.commands[gitStatus] = CommandResult{Stdout: " M README.md\x00 M cmd/main.go\x00"}
		d.dispatch(workspaceTickMsg{gen: m.wsGen})
		d.wait("the new status", func() bool { return m.fileTree.git.files["/src/cmd/main.go"] == 'M' })
		if _, ok := m.fileTree.git.files["/src/cmd/new.go"]; ok {
			t.Fatal("what git no longer says should be gone")
		}
	})
}

func TestARootOutsideARepositoryDrawsNoGit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := filesSource()
		ds.commands = map[string]CommandResult{gitTop: {ExitCode: 128, Stderr: "fatal: not a git repository"}}
		d, m, _ := openWorkspaceAt(t, ds, "enter", filesConsole)
		d.wait("the files root", func() bool { return m.fileTree.root == "/src" })
		d.leader(filesPanelKey)
		d.wait("git asked", func() bool { return slices.Contains(ds.ranSoFar(), gitTop) && !m.fileTree.git.reading })
		if slices.Contains(ds.ranSoFar(), gitStatus) {
			t.Fatal("no repository means no status to ask for")
		}
		if end := rowEnd(t, m, "README.md"); strings.HasSuffix(end, "M") {
			t.Fatalf("no repository, no marks: %q", end)
		}
	})
}

func TestDKeyDownloadsTheRow(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d, m, ds, _ := openFiles(t)
		d.key("down") // README.md, under cmd/
		d.key("d")
		d.wait("the download", func() bool { return len(ds.downloadsSoFar()) == 1 })
		if got := ds.downloadsSoFar()[0]; got != "/src/README.md" {
			t.Fatalf("downloaded %q, want /src/README.md", got)
		}
		d.wait("where it landed", func() bool { return strings.Contains(m.status, "/home/me/Downloads/README.md") })
	})
}

// Taking the keys by a click asks git again, as `leader F` does: what it says
// may have moved while the keys were elsewhere.
func TestClickingTheFilesPanelAsksGitAgain(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := gitFilesSource()
		d, m, _ := openWorkspaceAt(t, ds, "enter", filesConsole)
		d.wait("the files root", func() bool { return m.fileTree.root == "/src" })
		d.leader(filesPanelKey)
		d.wait("git's status", func() bool { return len(m.fileTree.git.files) == 2 && !m.fileTree.git.reading })
		d.key("esc")
		if m.filesFront {
			t.Fatal("esc should hand the keys back")
		}
		asked := len(ds.ranSoFar())
		pressFilesRow(t, d, m, "README.md")
		d.dispatch(tea.MouseReleaseMsg{X: m.filesLeft() + 4, Y: filesRowAt(m, "README.md"), Button: tea.MouseLeft})
		d.wait("git asked again", func() bool { return len(ds.ranSoFar()) > asked && !m.fileTree.git.reading })

		// A click while the panel already has the keys asks nothing more.
		asked = len(ds.ranSoFar())
		pressFilesRow(t, d, m, "README.md")
		d.dispatch(tea.MouseReleaseMsg{X: m.filesLeft() + 4, Y: filesRowAt(m, "README.md"), Button: tea.MouseLeft})
		d.settle()
		if got := len(ds.ranSoFar()); got != asked {
			t.Fatalf("a click with the keys asked git again: %d commands, want %d", got, asked)
		}
	})
}

// A change the panel makes while a read is in flight is read after it: that
// read may have been answered before the change landed, and an untracked file
// moved moves nothing the poll would notice.
func TestAChangeDuringAGitReadIsReadAfterIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := gitFilesSource()
		d, m, _ := openWorkspaceAt(t, ds, "enter", filesConsole)
		d.wait("the files root", func() bool { return m.fileTree.root == "/src" })
		d.leader(filesPanelKey)
		d.wait("git's status", func() bool { return len(m.fileTree.git.files) == 2 && !m.fileTree.git.reading })

		// A read in flight, answered with what git said before the change.
		m.fileTree.git.reading = true
		if cmd := m.rereadGit(); cmd != nil {
			t.Fatal("a read is in flight: the change waits for it")
		}
		ds.commands[gitStatus] = CommandResult{Stdout: " M README.md\x00?? new.go\x00"}
		asked := len(ds.ranSoFar())
		d.dispatch(filesGitMsg{box: m.paneBox, root: "/src", top: "/src", out: " M README.md\x00?? cmd/new.go\x00"})
		d.wait("the read after it", func() bool { return m.fileTree.git.files["/src/new.go"] == '?' })
		if len(ds.ranSoFar()) <= asked {
			t.Fatal("git should have been asked again")
		}
		if m.fileTree.git.again {
			t.Fatal("the read after it is the only one owed")
		}
	})
}
