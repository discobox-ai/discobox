package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Submitting a prompt does not leave the list on screen with a busy line under
// it while a pool comes up and gigabytes arrive. The window goes to the
// discobox being made instead, and reports there.
func TestWaitingDialogFollowsTheNarration(t *testing.T) {
	t.Parallel()
	m := &Model{st: newStyles(true), width: 80, height: 24}
	m.dialog = statusDialog("Starting nimble_swan", "creating the discobox")

	feed := narration{gen: m.busyGen, ch: make(chan narrationLine, 1)}
	m.narrated(narrationMsg{source: feed, text: "pulling the discobox image"})

	if m.dialog == nil {
		t.Fatal("the dialog went away while there was still something to report")
	}
	if m.dialog.body != "pulling the discobox image" {
		t.Fatalf("dialog body = %q, want the narration", m.dialog.body)
	}
	// The busy line keeps its own copy: the dialog is the same report larger,
	// not a different one.
	if !strings.Contains(m.busy, "pulling the discobox image") {
		t.Fatalf("busy = %q, want the narration too", m.busy)
	}
}

// It takes itself down when the attach it is covering can finish.
func TestWaitingDialogClosesWhenProvisioningEnds(t *testing.T) {
	t.Parallel()
	m := &Model{st: newStyles(true), width: 80, height: 24}
	m.dialog = statusDialog("Starting nimble_swan", "creating the discobox")

	m.update(provisioningDoneMsg{sandboxID: "sbx_1"})
	if m.dialog != nil {
		t.Fatal("the dialog outlived the wait it was reporting")
	}
}

// A dialog the user opened is theirs, and provisioning finishing must not close
// it out from under them.
func TestProvisioningDoneLeavesOtherDialogsAlone(t *testing.T) {
	t.Parallel()
	m := &Model{st: newStyles(true), width: 80, height: 24}
	m.dialog = textDialog("Keys", "…")

	m.update(provisioningDoneMsg{sandboxID: "sbx_1"})
	if m.dialog == nil {
		t.Fatal("an unrelated dialog was closed")
	}
}

// Nothing to answer, so Enter is not an answer: only the work ending, or Esc,
// takes it away. Enter closing it would drop the user onto a pane that has not
// attached yet.
func TestWaitingDialogIsNotDismissedByEnter(t *testing.T) {
	t.Parallel()
	d := statusDialog("Starting nimble_swan", "creating the discobox")
	if _, closed := d.update(tea.KeyPressMsg{Code: tea.KeyEnter}); closed {
		t.Fatal("Enter dismissed a dialog that has no answer to give")
	}
	if _, closed := d.update(tea.KeyPressMsg{Code: tea.KeyEscape}); !closed {
		t.Fatal("Esc did not stop the wait")
	}
}

// A name is better than an id, and an id is better than nothing: a freshly
// created discobox may be reported before its name comes back.
func TestSandboxLabelPrefersTheName(t *testing.T) {
	t.Parallel()
	if got := sandboxLabel(Sandbox{ID: "sbx_1", Name: "nimble_swan"}); got != "nimble_swan" {
		t.Fatalf("sandboxLabel() = %q", got)
	}
	if got := sandboxLabel(Sandbox{ID: "sbx_1"}); got != "sbx_1" {
		t.Fatalf("sandboxLabel() = %q", got)
	}
}

// The launcher's create is watched in the discobox's workspace, opening, not
// under a wait: and walking off that workspace is "I am not watching this
// one", so the attach that was opening goes with it — its answer, arriving
// once the discobox is up, must not put a pane over the screen somebody went
// to.
func TestLeavingTheOpeningWorkspaceStopsTheAttachUnderIt(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	m.created(createdMsg{sandbox: Sandbox{ID: "sbx_1", Name: "nimble_swan"}, create: &createWatch{}, accepted: true})
	if m.dialog != nil {
		t.Fatal("the launcher's create put a wait up over the workspace it opens")
	}
	if !m.wsOpening || m.paneBox.ID != "sbx_1" {
		t.Fatalf("opening %v, box %s; want the new box's workspace on its way up", m.wsOpening, m.paneBox.ID)
	}
	opening := m.wsGen

	m.closeWorkspace()

	// The listing the attach was waiting on, answered after the fact.
	m.update(workspaceExecsMsg{gen: opening})
	if m.inPanes() || m.connecting[ExecPrimary] {
		t.Fatal("the discobox took the screen after its workspace was left")
	}
}

// A create's two answers come from goroutines of their own, so "the server
// took it" can arrive after "done". Done and failed, there is nothing to go
// to: the late one must not park the screen onto a workspace no attach is
// coming to.
func TestALateAcceptAfterAFailedCreateGoesNowhere(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	box := Sandbox{ID: "sbx_new", Name: "nimble_swan"}
	watch := &createWatch{}
	m.created(createdMsg{sandbox: box, create: watch, accepted: true, err: errors.New("push failed")})
	m.update(promptSpentMsg{sandbox: box, create: watch})
	if m.wsOpening || m.paneBox.ID == box.ID {
		t.Fatalf("opening %v, box %s; want the screen left alone by an accept that came after the failure", m.wsOpening, m.paneBox.ID)
	}
}

// And done well, the attach is already the screen: the late accept leaves it
// as it is rather than parking it and starting over.
func TestALateAcceptAfterADoneCreateLeavesTheAttach(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	box := Sandbox{ID: "sbx_new", Name: "nimble_swan"}
	watch := &createWatch{}
	m.created(createdMsg{sandbox: box, create: watch, accepted: true})
	gen := m.wsGen
	m.update(promptSpentMsg{sandbox: box, create: watch})
	if m.wsGen != gen || !m.wsOpening || m.paneBox.ID != box.ID {
		t.Fatalf("gen %d→%d, opening %v, box %s; want the attach left running", gen, m.wsGen, m.wsOpening, m.paneBox.ID)
	}
}

// Two creates can be underway at once: the first finishing must not take the
// screen from the second, which is the one the window went to last.
func TestAFirstCreateDoesNotTakeTheScreenFromASecond(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	a, b := Sandbox{ID: "sbx_a", Name: "first"}, Sandbox{ID: "sbx_b", Name: "second"}
	wa, wb := &createWatch{}, &createWatch{}
	m.update(promptSpentMsg{sandbox: a, create: wa})
	m.update(promptSpentMsg{sandbox: b, create: wb})
	if !m.wsOpening || m.paneBox.ID != b.ID {
		t.Fatalf("opening %v, box %s; want the second create's workspace", m.wsOpening, m.paneBox.ID)
	}
	send(t, m, createdMsg{sandbox: a, create: wa, accepted: true})
	if m.paneBox.ID != b.ID || !m.wsOpening {
		t.Fatalf("box %s, opening %v; want the second create's workspace kept", m.paneBox.ID, m.wsOpening)
	}
	if !strings.Contains(m.status, "created first") {
		t.Fatalf("status %q; want the first create reported", m.status)
	}
	m.created(createdMsg{sandbox: b, create: wb, accepted: true})
	if m.paneBox.ID != b.ID || !m.wsOpening || m.connecting == nil {
		t.Fatalf("box %s, opening %v; want the second create attaching", m.paneBox.ID, m.wsOpening)
	}
}
