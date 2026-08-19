package tui

import (
	"slices"
	"testing"
)

// A narrated operation replaces the busy line as it goes, so a long wait says
// what it is spending its time on instead of repeating one word for minutes.
func TestNarrationReplacesTheBusyLine(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	m.busy = "creating the discobox…"

	feed, _ := m.narrate()
	m.narrated(narrationMsg{source: feed, text: "pushing source"})

	if m.busy != "pushing source…" {
		t.Fatalf("busy = %q, want the reported step", m.busy)
	}
}

// A report from an operation the window has moved on from is dropped. Reports
// are in flight when an operation ends — the work and its feed are separate
// goroutines — and one landing late would overwrite whatever replaced it.
func TestNarrationFromAFinishedOperationIsDropped(t *testing.T) {
	m := newTestModel(t, newFakeSource())

	stale, _ := m.narrate()
	m.endNarration()
	m.busy = "attach…"
	m.narrated(narrationMsg{source: stale, text: "pushing source"})

	if m.busy != "attach…" {
		t.Fatalf("busy = %q, want the stale report ignored", m.busy)
	}
}

// Starting a narration ends the one before it, for the same reason: only one
// operation owns the line, so the previous one's reports are stale by
// definition rather than by timing.
func TestStartingANarrationEndsThePreviousOne(t *testing.T) {
	m := newTestModel(t, newFakeSource())

	first, _ := m.narrate()
	second, _ := m.narrate()
	m.busy = "attach…"

	m.narrated(narrationMsg{source: first, text: "preparing source"})
	if m.busy != "attach…" {
		t.Fatalf("busy = %q, want the superseded feed ignored", m.busy)
	}
	m.narrated(narrationMsg{source: second, text: "creating the container"})
	if m.busy != "creating the container…" {
		t.Fatalf("busy = %q, want the current feed reported", m.busy)
	}
}

// Opening a workspace asks what the discobox is doing while the attach waits
// for it. The attach can block for minutes behind an image pull, and this is
// the only thing that can say so.
func TestOpeningAWorkspaceWatchesProvisioning(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	_, _, _ = openWorkspace(t, ds, "enter")

	ds.mu.Lock()
	watched := slices.Clone(ds.watched)
	ds.mu.Unlock()
	if len(watched) == 0 {
		t.Fatal("the workspace never asked what the discobox was doing")
	}
	if watched[0] != ds.sandboxes[0].ID {
		t.Fatalf("watched %q, want the discobox being attached to (%q)", watched[0], ds.sandboxes[0].ID)
	}
}

// Connecting ends the narration: the discobox agent accepts an attach only once
// the terminal is launched and installed, so a connected session means there is
// nothing left to say about getting there.
func TestConnectingEndsTheProvisioningNarration(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	_, m, _ := openWorkspace(t, ds, "enter")

	if m.busy != "" {
		t.Fatalf("busy = %q, want the line released once the terminal is up", m.busy)
	}
	if m.stopNarrating != nil {
		t.Fatal("the provisioning watch outlived the wait it was narrating")
	}
}
