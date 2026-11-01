package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/termpane"
)

// The windows that are one command's own: `discobox run`, which is the window
// opened on one run request, and `discobox attach`, which is it opened on one
// discobox. Both open on the discobox they are about rather than on the list,
// and both close when that is left.

// openAttached is the window `discobox attach` opens: the launcher, opened on
// one discobox's workspace rather than on its list.
func openAttached(t *testing.T, ds *fakeSource, sandbox Sandbox) (*driver, *Model) {
	t.Helper()
	m := New(t.Context(), ds, WithAttach(sandbox))
	m.logo = logo{}
	m.copyOS = func(string) error { return nil }
	d := newDriver(t, m)
	d.start()
	return d, m
}

// The window opens on the discobox it was given, with nothing pressed: no
// list, no prompt, no row to find. The attach starts from the row the command
// already has, so it does not wait for the listing to come back first.
func TestAnAttachOpensOnItsDiscobox(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	box := testSandboxes()[1]
	d, m := openAttached(t, ds, box)
	d.wait("the workspace", func() bool { return m.focus == focusPane })
	d.wait("the primary terminal", func() bool { return m.primary() != nil })

	if m.paneBox.ID != box.ID {
		t.Fatalf("the window opened on %q, want the discobox it was given (%q)", m.paneBox.ID, box.ID)
	}
	term := ds.execTerm(ExecPrimary)
	term.send("hello from the discobox")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "hello from the discobox") })

	// With nothing but the primary in it the window is bare (bare.go): the
	// terminal, and under it one line saying where it was cut from and the
	// way out — the one key that leaves. Quit does the same thing here, so
	// the line does not offer it as a second choice.
	frame := plainFrame(m)
	lines := strings.Split(frame, "\n")
	status := lines[len(lines)-1]
	if !strings.Contains(status, "discobox") || !strings.Contains(status, box.Source) {
		t.Fatalf("the status line should say where the discobox was cut from: %q", status)
	}
	if !strings.Contains(status, "ctrl+a d detach") {
		t.Fatalf("the status line should say how to leave: %q", status)
	}
	if strings.Contains(frame, "quit") {
		t.Fatalf("quit is detach here, so it should not be offered beside it:\n%s", frame)
	}
}

// Detaching from an attach leaves the window, because the window is the
// attach: there is no list behind it that anybody asked for. The sessions keep
// running, exactly as a detach always leaves them.
func TestDetachingAnAttachClosesTheWindow(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openAttached(t, ds, testSandboxes()[0])
	// Past the wait screen the window opens on, which has the keys while it is
	// up.
	d.wait("the primary terminal", func() bool { return m.primary() != nil && m.dialog == nil })

	d.key("ctrl+a")
	d.key(paneDetachAlt)
	d.wait("the window to close", func() bool { return m.quit })

	if m.exitErr != nil {
		t.Fatalf("exitErr = %v, want a detach to be an ordinary exit", m.exitErr)
	}
	if m.primary() != nil {
		t.Fatal("the workspace should be closed behind the window")
	}
}

// The launcher's own detach is unchanged on a console too narrow for the tree
// beside the workspace: it leaves the workspace and lands back on the list it
// was opened from.
func TestDetachingTheLauncherKeepsTheWindow(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openNarrowWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(paneDetachAlt)
	d.wait("the list", func() bool { return m.primary() == nil })

	if m.quit {
		t.Fatal("detaching from the launcher should not close the window")
	}
}

// An attach is over when its session is, the way a plain `discobox attach` is:
// the harness exits, and the window goes with it rather than falling back to a
// list.
func TestAnAttachEndsWithItsSession(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openAttached(t, ds, testSandboxes()[0])
	d.wait("the primary terminal", func() bool { return m.primary() != nil })

	ds.execTerm(ExecPrimary).Close()
	d.wait("the window to close", func() bool { return m.quit })

	if m.exitErr != nil {
		t.Fatalf("exitErr = %v, want a session that simply ended to be an ordinary exit", m.exitErr)
	}
}

// An attach that never comes up ends with the failure, which Run hands back to
// the command that opened the window: a status line on a screen nobody is left
// to read is not a report.
func TestAnAttachThatCannotOpenEndsWithTheError(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.openExecErrFor = map[string]error{ExecPrimary: errors.New("session is sealed")}
	d, m := openAttached(t, ds, testSandboxes()[0])
	d.wait("the window to close", func() bool { return m.quit })

	if m.exitErr == nil || !strings.Contains(m.exitErr.Error(), "session is sealed") {
		t.Fatalf("exitErr = %v, want the attach's own failure", m.exitErr)
	}
}

// openRun is the window `discobox run` opens: the launcher, opened on the
// request the command built from its flags rather than on a prompt somebody
// types.
func openRun(t *testing.T, ds *fakeSource, req RunRequest) (*driver, *Model) {
	t.Helper()
	return openRunWith(t, ds, req, false)
}

// openRunWith is that window for a command that may have left its source
// unsaid, which the window then asks.
func openRunWith(t *testing.T, ds *fakeSource, req RunRequest, askSource bool) (*driver, *Model) {
	t.Helper()
	m := New(t.Context(), ds, WithRun(req, askSource))
	m.logo = logo{}
	m.copyOS = func(string) error { return nil }
	d := newDriver(t, m)
	d.start()
	return d, m
}

// The question about uncommitted work is the window's own dialog, the one Enter
// in the prompt puts up: `discobox run` hands its request over and the window
// makes the discobox, rather than the command asking on the terminal first.
func TestARunWindowAsksTheWindowsOwnQuestion(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.workspace = SourceWorkspace{
		Directory: "/src/disco2", Repository: true, Carries: true,
		Changes: []string{"run.go", "tui.go", "model.go", "pane.go"},
	}
	d, m := openRun(t, ds, RunRequest{Prompt: []string{"fix", "the", "tests"}})
	d.wait("the question", func() bool { return m.dialog != nil && m.dialog.kind == dlgActions })

	if !strings.Contains(m.dialog.body, "4 uncommitted changes (run.go, tui.go, model.go and 1 more)") {
		t.Fatalf("dialog body = %q, want the changes it is about named", m.dialog.body)
	}
	// y is carry them in, and the create that follows is the window's own —
	// with the prompt as the words the command was given.
	d.key("y")
	d.wait("the create", func() bool { return len(ds.runRequests()) == 1 })
	if got := ds.runs[0].IncludeDirty; got != "true" {
		t.Fatalf("includeDirty = %q, want the answer the dialog took", got)
	}
	if got := ds.runs[0].Prompt; !slices.Equal(got, []string{"fix", "the", "tests"}) {
		t.Fatalf("prompt = %q, want the words the command was given", got)
	}
}

// Enter means no there, the same way it does in the launcher: carrying local
// content in is what has to be asked for.
func TestARunWindowsQuestionLeadsWithCarryingNothing(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.workspace = SourceWorkspace{Directory: "/src/disco2", Repository: true, Carries: true}
	d, m := openRun(t, ds, RunRequest{})
	d.wait("the question", func() bool { return m.dialog != nil && m.dialog.kind == dlgActions })

	d.key("enter")
	d.wait("the create", func() bool { return len(ds.runRequests()) == 1 })
	if got := ds.runs[0].IncludeDirty; got != "false" {
		t.Fatalf("includeDirty = %q, want the default answer to carry nothing extra", got)
	}
}

// `discobox run` has already answered --include-dirty, so a dirty repository
// is not asked about again; a directory in no repository still is, because
// that question is the copy's, and Enter copies it.
func TestARunWindowAsksOnlyTheQuestionTheCommandLeftOpen(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.workspace = SourceWorkspace{Directory: "/src/disco2", Repository: true, Carries: true}
	d, m := openRun(t, ds, RunRequest{IncludeDirty: "false"})
	d.wait("the create", func() bool { return len(ds.runRequests()) == 1 })
	if got := ds.runs[0].IncludeDirty; got != "false" {
		t.Fatalf("includeDirty = %q, want the command's answer", got)
	}

	ds = newFakeSource(testSandboxes()...)
	ds.workspace = SourceWorkspace{Directory: "/home/ada/notes", Carries: true}
	d, m = openRun(t, ds, RunRequest{IncludeDirty: "false"})
	d.wait("the copy question", func() bool { return m.dialog != nil && m.dialog.kind == dlgActions })
	if m.dialog.subject != "/home/ada/notes" {
		t.Fatalf("dialog subject = %q, want the directory", m.dialog.subject)
	}
	d.key("enter")
	d.wait("the create", func() bool { return len(ds.runRequests()) == 1 })
	if got := ds.runs[0].CopyDirectory; got != "true" {
		t.Fatalf("copyDirectory = %q, want Enter to copy", got)
	}
}

// While the discobox is being made the window is on the discobox being made,
// never on the list of everything else — which is not what somebody who ran one
// command is looking at.
func TestARunWindowWaitsOnItsOwnScreen(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.runGate = make(chan struct{})
	d, m := openRun(t, ds, RunRequest{IncludeDirty: "false", Prompt: []string{"fix the tests"}})
	d.wait("the create", func() bool { return len(ds.runRequests()) == 1 })

	if m.dialog == nil || m.dialog.kind != dlgStatus {
		t.Fatalf("dialog = %+v, want the wait to have the screen", m.dialog)
	}
	frame := plainFrame(m)
	for _, row := range testSandboxes() {
		if strings.Contains(frame, row.Name) {
			t.Fatalf("the window sat on the list while the discobox was being made:\n%s", frame)
		}
	}
	close(ds.runGate)
	d.wait("the workspace", func() bool { return m.primary() != nil })
}

// The window is the run, so it is the attach on what it made: leaving the
// workspace leaves the window, exactly as `discobox attach`'s does.
func TestARunWindowBecomesTheAttachOnWhatItMade(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openRun(t, ds, RunRequest{IncludeDirty: "false"})
	// Past the wait screen, which has the keys while it is up.
	d.wait("the workspace", func() bool { return m.primary() != nil && m.dialog == nil })

	if m.attach == nil || m.attach.ID != ds.createdID {
		t.Fatalf("attach = %+v, want the discobox this window made", m.attach)
	}
	d.key("ctrl+a")
	d.key(paneDetachAlt)
	d.wait("the window to close", func() bool { return m.quit })
	// And it is what Run hands back, so the command can say which discobox
	// is still running once the window has taken the screen away with it.
	if made := m.made(); made == nil || made.ID != ds.createdID {
		t.Fatalf("made = %+v, want the discobox this window made", made)
	}
}

// A create that failed ends the window with the failure, the way an attach
// that never came up does: the window has no list behind it, and the list is
// a screen only the box menu asks for. The command reports the failure on the
// terminal once the window has gone.
func TestARunWindowsFailedCreateEndsWithTheError(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.runErr = errors.New("no pool is accepting discoboxes")
	d, m := openRun(t, ds, RunRequest{IncludeDirty: "false"})
	d.wait("the window to close", func() bool { return m.quit })

	if m.exitErr == nil || !strings.Contains(m.exitErr.Error(), "cannot create the discobox: no pool is accepting discoboxes") {
		t.Fatalf("exitErr = %v, want the create's failure", m.exitErr)
	}
}

// So does a create the server took and the window could not finish: the
// failure says it was created, so nobody runs it again for a second one.
func TestARunWindowsUnfinishedCreateEndsWithTheError(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.deliverErr = errors.New("source push refused")
	d, m := openRun(t, ds, RunRequest{IncludeDirty: "false"})
	d.wait("the window to close", func() bool { return m.quit })

	if m.exitErr == nil || !strings.Contains(m.exitErr.Error(), "created the discobox, but could not finish setting it up: source push refused") {
		t.Fatalf("exitErr = %v, want the unfinished create's failure", m.exitErr)
	}
	// The discobox is there whatever went wrong after the server took it, and
	// the command names it once the window has gone.
	if made := m.made(); made == nil || made.ID != ds.createdID {
		t.Fatalf("made = %+v, want the discobox the create made", made)
	}
}

// Turning down a question the run stopped to ask is turning down the run, as
// turning down its create card is: nothing was made, and the window goes
// rather than uncovering the list.
func TestEscOnARunsHarnessQuestionEndsTheCommand(t *testing.T) {
	t.Parallel()
	ds := noDefaultSource(t)
	d, m := openRun(t, ds, RunRequest{IncludeDirty: "false"})
	d.wait("the harness question", func() bool { return m.dialog != nil && m.dialog.kind == dlgActions })

	d.key("esc")
	d.wait("the window to close", func() bool { return m.quit })
	if !errors.Is(m.exitErr, errNothingCreated) || len(ds.runRequests()) != 0 {
		t.Fatalf("exitErr = %v runs = %+v, want nothing created", m.exitErr, ds.runRequests())
	}
}

// A harness setup the run stopped for that fails ends the run, and with it the
// window.
func TestARunWindowsFailedSetupEndsWithTheError(t *testing.T) {
	t.Parallel()
	ds := noDefaultSource(t)
	ds.configureErr = errors.New("the setup exited before it finished")
	d, m := openRun(t, ds, RunRequest{IncludeDirty: "false"})
	d.wait("the harness question", func() bool { return m.dialog != nil && m.dialog.kind == dlgActions })

	for i, item := range m.dialog.items {
		if item.label == "Custom" {
			d.key(itoa(i + 1))
			break
		}
	}
	d.wait("the window to close", func() bool { return m.quit })
	if m.exitErr == nil || !strings.Contains(m.exitErr.Error(), "the setup exited before it finished") {
		t.Fatalf("exitErr = %v, want the setup's failure", m.exitErr)
	}
	if len(ds.runRequests()) != 0 {
		t.Fatalf("runs = %+v, want nothing created", ds.runRequests())
	}
}

// The composer's draft belongs to the launcher, and a window that never shows
// the composer must not write over it: a create started from the command line
// empties the field on its way past, and that is not a prompt anybody typed
// here.
func TestAOneShotWindowLeavesTheDraftAlone(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.session.Draft = "the sentence I was in the middle of"
	d, m := openRun(t, ds, RunRequest{IncludeDirty: "false"})
	d.wait("the workspace", func() bool { return m.primary() != nil && m.dialog == nil })

	d.key("ctrl+a")
	d.key(paneQuitKey)
	d.wait("the window to close", func() bool { return m.quit })
	if len(ds.drafts) != 0 {
		t.Fatalf("drafts written = %v, want the launcher's draft left alone", ds.drafts)
	}
}

// Esc on the wait leaves, rather than dropping onto a list nobody asked for.
// The discobox carries on coming up in the background and nothing attaches to
// it: what was left is this window.
func TestEscOnAOneShotWaitLeavesTheDiscoboxStarting(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.runGate = make(chan struct{})
	d, m := openRun(t, ds, RunRequest{IncludeDirty: "false"})
	d.wait("the wait", func() bool { return m.dialog != nil && m.dialog.kind == dlgStatus })

	d.key("esc")
	d.wait("the window to close", func() bool { return m.quit })
	if m.exitErr != nil {
		t.Fatalf("exitErr = %v, want leaving to be an ordinary exit", m.exitErr)
	}
	if m.primary() != nil {
		t.Fatal("leaving the wait should not attach to anything")
	}
	if made := m.made(); made != nil {
		t.Fatalf("made = %+v, want nothing named before the create came back", made)
	}
	close(ds.runGate)
}

// A project that has not been welcomed still gets the introduction on a run
// window: it is not one of the two things New drops it for, and it stands in
// front of the wait for the discobox this run is making, exactly as it would
// stand in front of the list on the launcher's own first run.
func TestARunWindowStillShowsTheIntroductionWhenUnwelcomed(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	m := New(t.Context(), ds, WithRun(RunRequest{Prompt: []string{"fix", "the", "tests"}}, false), WithWelcome())
	m.logo = logo{}
	m.copyOS = func(string) error { return nil }
	d := newDriver(t, m)
	d.start()

	if !m.welcoming {
		t.Fatal("the introduction should be showing")
	}
	// The run it was opened for proceeds behind the introduction the same way
	// everything else loads behind it: no dirty workspace and a default
	// harness mean nothing here for it to ask about.
	d.wait("the create", func() bool { return len(ds.runRequests()) == 1 })

	d.key("enter")
	if m.welcoming {
		t.Fatal("Enter did not dismiss the introduction")
	}
	if ds.welcomed != 1 {
		t.Fatalf("the welcome was recorded %d times, want once", ds.welcomed)
	}
}

// `discobox attach` never shows it, welcomed or not: there is no screen behind
// the workspace it opens directly on for the introduction to hand over to.
func TestAnAttachNeverShowsTheIntroduction(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	box := testSandboxes()[1]
	m := New(t.Context(), ds, WithAttach(box), WithWelcome())
	if m.welcoming {
		t.Fatal("the introduction should not be showing over an attach")
	}
}

// `discobox run` with no SOURCE asks where to cut from rather than taking the
// directory it ran in: the window opens on the create card, holding every flag
// the command was given, and creating from it carries the ones the card has no
// row for as well.
func TestARunWithNoSourceAsksOnTheCreateCard(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	req := RunRequest{
		Prompt:     []string{"fix", "the", "tests"},
		Harness:    "codex",
		UserSkills: new(true),
		SkillDirs:  []string{"./team-skills"},
		Include:    []string{"/src/obot"},
		Env:        []string{"A=1"},
		Bind:       []string{"github:GH_TOKEN"},
		Pool:       "pool-a",
	}
	d, m := openRunWith(t, ds, req, true)
	d.wait("the create card", func() bool { return m.dialog != nil && m.dialog.kind == dlgCreate })
	if len(ds.runRequests()) != 0 {
		t.Fatalf("runs = %+v, want nothing created before the card is answered", ds.runRequests())
	}
	opts := m.opts.opts
	if got := opts[optHarness].selected(); got != "codex" {
		t.Fatalf("harness = %q, want the one the command named", got)
	}
	if !opts[optUserSkills].changed() || !slices.Equal(opts[optSkills].items, req.SkillDirs) || !slices.Equal(opts[optInclude].items, req.Include) {
		t.Fatal("the card should open holding the command's skills and sources")
	}

	// The sources come off the listing, which may land after the card.
	d.wait("the listing's sources", func() bool {
		return slices.Contains(m.opts.opts[optSource].values, "/src/obot")
	})
	d.key(itoa(slices.Index(m.opts.opts[optSource].values, "/src/obot") + 1))
	d.wait("the create", func() bool { return len(ds.runRequests()) == 1 })
	run := ds.runRequests()[0]
	if run.Source != "/src/obot" {
		t.Fatalf("source = %q, want the one chosen on the card", run.Source)
	}
	if run.Harness != "codex" || run.UserSkills == nil || !*run.UserSkills || !slices.Equal(run.Include, req.Include) || !slices.Equal(run.Env, req.Env) {
		t.Fatalf("run = %+v, want the command's flags carried", run)
	}
	if !slices.Equal(run.Prompt, req.Prompt) || !slices.Equal(run.Bind, req.Bind) || run.Pool != "pool-a" {
		t.Fatalf("run = %+v, want the prompt, bindings and pool as given", run)
	}
}

// Turning the card down is turning the command down: the window closes and
// says nothing was made, rather than leaving a list nobody opened.
func TestEscOnARunsCreateCardEndsTheCommand(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openRunWith(t, ds, RunRequest{Prompt: []string{"fix"}}, true)
	d.wait("the create card", func() bool { return m.dialog != nil && m.dialog.kind == dlgCreate })
	d.key("esc")
	d.wait("the window to close", func() bool { return m.quit })
	if !errors.Is(m.exitErr, errNothingCreated) || len(ds.runRequests()) != 0 {
		t.Fatalf("exitErr = %v runs = %+v, want nothing created", m.exitErr, ds.runRequests())
	}
}

// The card is the command's question however it is left: Esc from a field it
// opened lands back on it still meaning that, q closes it as it closes the
// other cards, and Ctrl-C is leaving before anything was asked for.
func TestARunsCreateCardEndsTheCommandHoweverItIsLeft(t *testing.T) {
	t.Parallel()
	for name, keys := range map[string][]string{
		"esc from the source field": {"e", "esc", "esc"},
		"q":                         {"q"},
		"ctrl+c":                    {"ctrl+c"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ds := newFakeSource(testSandboxes()...)
			d, m := openRunWith(t, ds, RunRequest{Prompt: []string{"fix"}}, true)
			d.wait("the create card", func() bool { return m.dialog != nil && m.dialog.kind == dlgCreate })
			for _, key := range keys {
				d.key(key)
			}
			d.wait("the window to close", func() bool { return m.quit })
			if !errors.Is(m.exitErr, errNothingCreated) || len(ds.runRequests()) != 0 {
				t.Fatalf("exitErr = %v runs = %+v, want nothing created", m.exitErr, ds.runRequests())
			}
		})
	}
}

// `discobox run` with no SOURCE used to be the directory it ran in, and Enter on
// its card still is, whatever folder the last window here was left on.
func TestARunsCreateCardOpensOnTheDirectoryItRanIn(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.session.View = ListView{FolderKey: testKey("/src/obot"), FolderLabel: "/src/obot", FolderSource: "/src/obot"}
	d, m := openRunWith(t, ds, RunRequest{Prompt: []string{"fix"}}, true)
	d.wait("the create card", func() bool { return m.dialog != nil && m.dialog.kind == dlgCreate })
	d.key("enter")
	d.wait("the create", func() bool { return len(ds.runRequests()) == 1 })
	// The directory the window runs in goes as an empty source, which is ".".
	if got := ds.runRequests()[0].Source; got != "" {
		t.Fatalf("source = %q, want the directory the command ran in", got)
	}
	if m.exitErr != nil {
		t.Fatalf("exitErr = %v, want none once something was asked for", m.exitErr)
	}
}

// Answering the card puts the window's wait screen back while the working tree
// is checked, rather than the list nobody opened the window for.
func TestAnsweringARunsCreateCardGoesBackToTheWaitScreen(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.workspaceGate = make(chan struct{})
	defer close(ds.workspaceGate)
	d, m := openRunWith(t, ds, RunRequest{Prompt: []string{"fix"}}, true)
	d.wait("the create card", func() bool { return m.dialog != nil && m.dialog.kind == dlgCreate })
	d.key("enter")
	if m.dialog == nil || m.dialog.title != creatingTitle {
		t.Fatalf("dialog = %v, want the wait screen while the working tree is checked", m.dialog)
	}
}

// The warning a create reports — uncommitted changes `discobox run` left out —
// is still on screen when the window has become the workspace on the discobox
// it made: that is the screen the person is looking at by then.
func TestARunWindowsWarningIsOnTheWorkspace(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.runWarnings = []string{"/src/disco2: 2 uncommitted changes left out (--include-dirty carries them); the discobox starts from the last commit"}
	d, m := openRun(t, ds, RunRequest{IncludeDirty: "false"})
	// A long message is cut from its middle to fit the row, so its two ends
	// are what is certain to be on screen: what it is about, and what came of
	// it.
	d.wait("the workspace showing the warning", func() bool {
		frame := plainFrame(m)
		return m.primary() != nil && m.dialog == nil && strings.Contains(frame, "! /src/disco2:") && strings.Contains(frame, "from the last commit")
	})
}

// Archiving the discobox from a run window's workspace takes the workspace
// away, and the window with it: there is no list behind it to report on, so
// the archive is waited for on a screen of its own, and Esc does not take that
// down to show the list.
func TestArchivingFromARunWindowClosesTheWindow(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openRun(t, ds, RunRequest{IncludeDirty: "false"})
	d.wait("the workspace", func() bool { return m.primary() != nil && m.dialog == nil })

	ds.doGate = make(chan struct{})
	d.key("ctrl+a")
	d.key("x")
	d.wait("the archive's wait", func() bool { return m.dialog != nil && m.dialog.kind == dlgStatus })
	d.key("esc")
	if m.dialog == nil || m.dialog.kind != dlgStatus {
		t.Fatalf("dialog = %+v, want the wait to stay up through Esc", m.dialog)
	}
	frame := plainFrame(m)
	for _, row := range testSandboxes() {
		if strings.Contains(frame, row.Name) {
			t.Fatalf("the window showed the list while the archive was out:\n%s", frame)
		}
	}
	// Esc does nothing here, so the wait offers no key for it.
	if strings.Contains(frame, "Esc") || !strings.Contains(frame, "closes when the archive is answered") {
		t.Fatalf("the archive's wait should say what happens and offer no Esc:\n%s", frame)
	}
	close(ds.doGate)
	d.wait("the window to close", func() bool { return m.quit })

	if got, want := ds.verbs(), []string{"archive " + ds.createdID}; !slices.Equal(got, want) {
		t.Fatalf("verbs = %q, want %q", got, want)
	}
	if m.exitErr != nil {
		t.Fatalf("exitErr = %v, want an archive that worked to be an ordinary exit", m.exitErr)
	}
}

// An archive that fails ends the window with the failure, which the command
// reports: the status line it would have gone on is not there to be read.
func TestAFailedArchiveFromAnAttachEndsWithTheError(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.doErr = errors.New("server said no")
	d, m := openAttached(t, ds, testSandboxes()[0])
	d.wait("the primary terminal", func() bool { return m.primary() != nil && m.dialog == nil })

	d.key("ctrl+a")
	d.key("x")
	d.wait("the window to close", func() bool { return m.quit })

	if m.exitErr == nil || !strings.Contains(m.exitErr.Error(), "archive: server said no") {
		t.Fatalf("exitErr = %v, want the archive's failure", m.exitErr)
	}
}

// The launcher's archive, on a console with no tree, is unchanged: the
// workspace goes and the window lands on the list, which reports it.
func TestArchivingFromTheLauncherKeepsTheWindow(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openNarrowWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("x")
	d.wait("the archive", func() bool { return len(ds.verbs()) == 1 && m.busy == "" })

	if m.quit {
		t.Fatal("archiving from the launcher should not close the window")
	}
	if m.primary() != nil {
		t.Fatal("the workspace should be closed")
	}
}

// Only the archive that left the workspace closes the window: a stop's answer
// is reported like any other verb's, and the window stays on the workspace.
func TestStoppingFromAnAttachKeepsTheWindow(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openAttached(t, ds, testSandboxes()[0])
	d.wait("the primary terminal", func() bool { return m.primary() != nil && m.dialog == nil })

	d.key("ctrl+a")
	d.key("t")
	d.wait("the stop", func() bool { return len(ds.verbs()) == 1 && m.busy == "" })

	if m.quit {
		t.Fatal("a stop's answer should not close the window")
	}
	if m.primary() == nil {
		t.Fatal("the workspace should still be open")
	}
}

// Leaving a setup the run stopped for, before it has finished, is turning the
// run down: the window goes with nothing made rather than landing on the list
// with the run dropped.
func TestDetachingARunsSetupEndsTheCommand(t *testing.T) {
	t.Parallel()
	ds := noDefaultSource(t)
	d, m := openRun(t, ds, RunRequest{IncludeDirty: "false"})
	d.wait("the harness question", func() bool { return m.dialog != nil && m.dialog.kind == dlgActions })

	for i, item := range m.dialog.items {
		if item.label == "Custom" {
			d.key(itoa(i + 1))
			break
		}
	}
	d.wait("the setup", func() bool { return m.overlay != nil && m.overlay.configure != nil })
	d.dispatch(paneMsg{id: m.overlay.id, msg: termpane.DetachMsg{}})
	d.wait("the window to close", func() bool { return m.quit })
	if !errors.Is(m.exitErr, errNothingCreated) || len(ds.runRequests()) != 0 {
		t.Fatalf("exitErr = %v runs = %+v, want nothing created", m.exitErr, ds.runRequests())
	}
}
