package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// The windows that are one command's own: `discobox run`, which is the window
// opened on one run request, and `discobox attach`, which is it opened on one
// discobox. Both open on the discobox they are about rather than on the list,
// and both close when that is left.

// openAttached is the window `discobox attach` opens: the launcher, opened on
// one discobox's workspace rather than on its list.
func openAttached(t *testing.T, ds *fakeSource, sandbox Sandbox) (*driver, *Model) {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
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

	// It is the workspace, not a bare stream: the discobox is named above it,
	// and the way out is the one key that leaves — quit does the same thing
	// here, so the header does not offer it as a second choice.
	frame := plainFrame(m)
	if !strings.Contains(frame, box.ID) {
		t.Fatalf("the header should name the discobox:\n%s", frame)
	}
	if !strings.Contains(frame, "ctrl+a d detach") {
		t.Fatalf("the header should say how to leave:\n%s", frame)
	}
	if strings.Contains(frame, "quit") {
		t.Fatalf("quit is detach here, so it should not be offered beside it:\n%s", frame)
	}
}

// Detaching from an attach leaves the window, because the window is the
// attach: there is no list behind it that anybody asked for. The sessions keep
// running, exactly as a detach always leaves them.
func TestDetachingAnAttachClosesTheWindow(t *testing.T) {
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

// The launcher's own detach is unchanged: it leaves the workspace and lands
// back on the list it was opened from.
func TestDetachingTheLauncherKeepsTheWindow(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

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
	t.Setenv("NO_COLOR", "1")
	m := New(t.Context(), ds, WithRun(req))
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
	ds := newFakeSource(testSandboxes()...)
	ds.workspace = SourceWorkspace{
		Directory: "/src/disco2", Repository: true, Carries: true,
		Changes: []string{"run.go", "tui.go", "model.go", "pane.go"},
	}
	d, m := openRun(t, ds, RunRequest{Prompt: []string{"fix", "the", "tests"}})
	d.wait("the question", func() bool { return m.dialog != nil && m.dialog.kind == dlgConfirm })

	if !strings.Contains(m.dialog.body, "4 uncommitted changes (run.go, tui.go, model.go and 1 more)") {
		t.Fatalf("dialog body = %q, want the changes it is about named", m.dialog.body)
	}
	// y is carry them in, and the create that follows is the window's own —
	// with the prompt as the words the command was given.
	d.key("y")
	d.wait("the create", func() bool { return len(ds.runs) == 1 })
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
	ds := newFakeSource(testSandboxes()...)
	ds.workspace = SourceWorkspace{Directory: "/src/disco2", Repository: true, Carries: true}
	d, m := openRun(t, ds, RunRequest{})
	d.wait("the question", func() bool { return m.dialog != nil && m.dialog.kind == dlgConfirm })

	d.key("enter")
	d.wait("the create", func() bool { return len(ds.runs) == 1 })
	if got := ds.runs[0].IncludeDirty; got != "false" {
		t.Fatalf("includeDirty = %q, want the default answer to carry nothing extra", got)
	}
}

// While the discobox is being made the window is on the discobox being made,
// never on the list of everything else — which is not what somebody who ran one
// command is looking at.
func TestARunWindowWaitsOnItsOwnScreen(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.runGate = make(chan struct{})
	d, m := openRun(t, ds, RunRequest{IncludeDirty: "false", Prompt: []string{"fix the tests"}})
	d.wait("the create", func() bool { return len(ds.runs) == 1 })

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
}

// A create that failed takes its wait down with it: the report belongs on a
// screen somebody can read it on.
func TestARunWindowsFailureIsReadable(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.runErr = errors.New("no pool is accepting discoboxes")
	d, m := openRun(t, ds, RunRequest{IncludeDirty: "false"})
	d.wait("the failure", func() bool { return m.statusE })

	if m.dialog != nil {
		t.Fatalf("dialog = %+v, want the wait taken down with the create", m.dialog)
	}
	if !strings.Contains(m.status, "no pool is accepting discoboxes") {
		t.Fatalf("status = %q, want the failure", m.status)
	}
}

// The composer's draft belongs to the launcher, and a window that never shows
// the composer must not write over it: a create started from the command line
// empties the field on its way past, and that is not a prompt anybody typed
// here.
func TestAOneShotWindowLeavesTheDraftAlone(t *testing.T) {
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
	close(ds.runGate)
}

// The launcher's own wait is unchanged: Esc stops watching and puts back the
// list, which is where the discobox it was waiting for turns up.
func TestTheLaunchersWaitOnlyStopsWatching(t *testing.T) {
	m := &Model{st: newStyles(false)}
	d := m.waitDialog("Starting nimble_swan", "creating the discobox")
	if d.onCancel != nil || d.footer != "" {
		t.Fatalf("the launcher's wait should offer nothing but stopping watching: %+v", d)
	}
}
