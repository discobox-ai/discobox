package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// Submitting a prompt does not leave the list on screen with a busy line under
// it while a pool comes up and gigabytes arrive. The window goes to the
// discobox being made instead, and reports there.
func TestWaitingDialogFollowsTheNarration(t *testing.T) {
	t.Parallel()
	m := &Model{st: newStyles(true), width: 80, height: 24}
	m.dialog = statusDialog("Starting nimble_swan", "creating the discobox")
	m.dialog.windowWait = true

	feed := narration{gen: m.busyGen, Narration: console.NewNarration()}
	m.narrated(narrationMsg{source: feed, event: console.Event{Progress: &console.Progress{Text: "pulling the discobox image"}}})

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
	m.dialog.windowWait = true

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
	if got := sandboxLabel(console.Sandbox{ID: "sbx_1", Name: "nimble_swan"}); got != "nimble_swan" {
		t.Fatalf("sandboxLabel() = %q", got)
	}
	if got := sandboxLabel(console.Sandbox{ID: "sbx_1"}); got != "sbx_1" {
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
	m.created(createdMsg{sandbox: console.Sandbox{ID: "sbx_1", Name: "nimble_swan"}, create: &createWatch{}, accepted: true})
	if m.dialog != nil {
		t.Fatal("the launcher's create put a wait up over the workspace it opens")
	}
	if !m.wsOpening || m.Box.ID != "sbx_1" {
		t.Fatalf("opening %v, box %s; want the new box's workspace on its way up", m.wsOpening, m.Box.ID)
	}
	opening := m.wsGen

	m.closeWorkspace()

	// The listing the attach was waiting on, answered after the fact.
	m.update(workspaceExecsMsg{gen: opening})
	if m.inPanes() || m.connecting[console.ExecPrimary] {
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
	box := console.Sandbox{ID: "sbx_new", Name: "nimble_swan"}
	watch := &createWatch{}
	m.created(createdMsg{sandbox: box, create: watch, accepted: true, err: errors.New("push failed")})
	m.update(promptSpentMsg{sandbox: box, create: watch})
	if m.wsOpening || m.Box.ID == box.ID {
		t.Fatalf("opening %v, box %s; want the screen left alone by an accept that came after the failure", m.wsOpening, m.Box.ID)
	}
}

// And done well, the attach is already the screen: the late accept leaves it
// as it is rather than parking it and starting over.
func TestALateAcceptAfterADoneCreateLeavesTheAttach(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	box := console.Sandbox{ID: "sbx_new", Name: "nimble_swan"}
	watch := &createWatch{}
	m.created(createdMsg{sandbox: box, create: watch, accepted: true})
	gen := m.wsGen
	m.update(promptSpentMsg{sandbox: box, create: watch})
	if m.wsGen != gen || !m.wsOpening || m.Box.ID != box.ID {
		t.Fatalf("gen %d→%d, opening %v, box %s; want the attach left running", gen, m.wsGen, m.wsOpening, m.Box.ID)
	}
}

// Two creates can be underway at once: the first finishing must not take the
// screen from the second, which is the one the window went to last.
func TestAFirstCreateDoesNotTakeTheScreenFromASecond(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	a, b := console.Sandbox{ID: "sbx_a", Name: "first"}, console.Sandbox{ID: "sbx_b", Name: "second"}
	wa, wb := &createWatch{}, &createWatch{}
	m.update(promptSpentMsg{sandbox: a, create: wa})
	m.update(promptSpentMsg{sandbox: b, create: wb})
	if !m.wsOpening || m.Box.ID != b.ID {
		t.Fatalf("opening %v, box %s; want the second create's workspace", m.wsOpening, m.Box.ID)
	}
	send(t, m, createdMsg{sandbox: a, create: wa, accepted: true})
	if m.Box.ID != b.ID || !m.wsOpening {
		t.Fatalf("box %s, opening %v; want the second create's workspace kept", m.Box.ID, m.wsOpening)
	}
	if !strings.Contains(m.status, "created first") {
		t.Fatalf("status %q; want the first create reported", m.status)
	}
	m.created(createdMsg{sandbox: b, create: wb, accepted: true})
	if m.Box.ID != b.ID || !m.wsOpening || m.connecting == nil {
		t.Fatalf("box %s, opening %v; want the second create attaching", m.Box.ID, m.wsOpening)
	}
}

// Another operation's status card — the settings read, a secret being stored —
// is not the window's wait: a create's progress neither writes into it nor
// takes it down.
func TestWindowWaitLeavesOtherStatusCardsAlone(t *testing.T) {
	t.Parallel()
	m := &Model{st: newStyles(true), width: 80, height: 24}
	card := statusDialog("Settings", "saving…")
	m.dialog = card

	feed := narration{gen: m.busyGen, Narration: console.NewNarration()}
	m.narrated(narrationMsg{source: feed, event: console.Event{Progress: &console.Progress{Text: "pulling the discobox image"}}})
	if card.body != "saving…" {
		t.Fatalf("body = %q, want the card's own", card.body)
	}
	m.update(provisioningDoneMsg{sandboxID: "sbx_1"})
	if m.dialog != card {
		t.Fatal("provisioning finishing took another operation's card down")
	}
}

// The create's answer can overtake its acceptance on the feed. Then the
// answer gives the composer back, and the late acceptance touches nothing —
// not the next create's prompt, nor its hold on the composer.
func TestALateAcceptLeavesTheNextCreatesComposer(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	a := console.Sandbox{ID: "sbx_a", Name: "first"}
	reqA := console.RunRequest{Prompt: []string{"first prompt"}}
	m.prompt.SetValue("first prompt")
	m.setSubmitting(true)
	wa := &createWatch{}
	m.created(createdMsg{sandbox: a, req: reqA, create: wa, accepted: true})
	if m.submitting || m.prompt.Value() != "" {
		t.Fatalf("submitting %v, prompt %q; want the answer to give the composer back", m.submitting, m.prompt.Value())
	}
	// The person submits a second create before the first's acceptance lands.
	m.prompt.SetValue("first prompt")
	m.setSubmitting(true)
	m.update(promptSpentMsg{req: reqA, sandbox: a, create: wa})
	if !m.submitting || m.prompt.Value() != "first prompt" {
		t.Fatalf("submitting %v, prompt %q; want the second create's composer left alone", m.submitting, m.prompt.Value())
	}
}

// And in the usual order, the acceptance gives the composer back and the
// answer after it leaves the composer as it now is.
func TestAnAnswerAfterTheAcceptLeavesTheComposer(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	a := console.Sandbox{ID: "sbx_a", Name: "first"}
	reqA := console.RunRequest{Prompt: []string{"first prompt"}, Detach: true}
	m.prompt.SetValue("first prompt")
	m.setSubmitting(true)
	wa := &createWatch{}
	m.update(promptSpentMsg{req: reqA, sandbox: a, create: wa})
	if m.submitting || m.prompt.Value() != "" {
		t.Fatalf("submitting %v, prompt %q; want the acceptance to give the composer back", m.submitting, m.prompt.Value())
	}
	m.prompt.SetValue("first prompt")
	m.setSubmitting(true)
	m.created(createdMsg{sandbox: a, req: reqA, create: wa, accepted: true})
	if !m.submitting || m.prompt.Value() != "first prompt" {
		t.Fatalf("submitting %v, prompt %q; want the next create's composer left alone", m.submitting, m.prompt.Value())
	}
}
