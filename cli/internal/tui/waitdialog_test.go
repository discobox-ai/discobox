package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Submitting a prompt used to leave the list on screen with a busy line under
// it while a pool came up and gigabytes arrived. The window goes to the
// discobox being made instead, and reports there.
func TestWaitingDialogFollowsTheNarration(t *testing.T) {
	m := &Model{st: newStyles(true), width: 80, height: 24}
	m.dialog = statusDialog("Starting nimble_swan", "creating the discobox")

	feed := narration{gen: m.busyGen, ch: make(chan string, 1)}
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
	if got := sandboxLabel(Sandbox{ID: "sbx_1", Name: "nimble_swan"}); got != "nimble_swan" {
		t.Fatalf("sandboxLabel() = %q", got)
	}
	if got := sandboxLabel(Sandbox{ID: "sbx_1"}); got != "sbx_1" {
		t.Fatalf("sandboxLabel() = %q", got)
	}
}
