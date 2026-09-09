package tui

import (
	"errors"
	"strings"
	"testing"
)

// pushableSandboxes is the fixture with its first discobox delivered by
// pushing it from this machine, which is the one state the window pushes for.
func pushableSandboxes() []Sandbox {
	all := testSandboxes()
	all[0].Pushable = true
	return all
}

// The whole point: opening a discobox sends what has been committed here since
// it was created, with nobody asking for it.
func TestTheWorkspacePushesWithoutBeingAsked(t *testing.T) {
	ds := newFakeSource(pushableSandboxes()...)
	ds.pushes = []SourcePush{{Slug: "primary", Branch: "main", Commit: "b7d0f1145aa2", Pushed: true}}

	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the push", func() bool { return len(ds.pushedCalls()) > 0 })

	if got := ds.pushedCalls()[0]; got != "sbx_one" {
		t.Fatalf("pushed %q, want the discobox the workspace is open on, holding nothing", got)
	}
	// What went, and the branch to rebase onto inside the discobox.
	d.wait("the report", func() bool { return strings.Contains(m.status, "pushed b7d0f1145aa2") })
	if !strings.Contains(m.status, "origin/main") {
		t.Fatalf("status = %q, want the branch the discobox sees it on", m.status)
	}
}

// A source with nothing to send is the ordinary case, and it is silent: a
// window that said something every five seconds about having done nothing is a
// window whose key hints are never on screen.
func TestNothingIsSaidWhenThereIsNothingToPush(t *testing.T) {
	ds := newFakeSource(pushableSandboxes()...)
	ds.pushes = []SourcePush{{Slug: "primary", Branch: "main", Commit: "a3f9c2179bbf"}}

	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the look", func() bool { return len(ds.pushedCalls()) > 0 })
	d.settle()

	if strings.Contains(m.status, "push") {
		t.Fatalf("status = %q, want nothing said about a push that sent nothing", m.status)
	}
}

// The row's own gate: a discobox this machine did not push, or one whose source
// it reads live, is never asked about at all.
func TestADiscoboxThisMachineCannotPushIsNeverAsked(t *testing.T) {
	ds := newFakeSource(testSandboxes()...) // nothing is Pushable
	d, _, _ := openWorkspace(t, ds, "enter")
	d.settle()

	if calls := ds.pushedCalls(); len(calls) > 0 {
		t.Fatalf("pushed %v, want nothing for a discobox that is not this window's to push", calls)
	}
}

// A refused push is reported once and held at the commit it failed on: the
// reasons a push is refused are decisions rather than transients, so retrying
// one every five seconds would send the same rejected pack behind a permanent
// error line.
func TestARefusedPushIsHeldUntilTheCommitMoves(t *testing.T) {
	ds := newFakeSource(pushableSandboxes()...)
	ds.pushes = []SourcePush{{
		Slug:   "primary",
		Branch: "main",
		Commit: "b7d0f1145aa2",
		Err:    errors.New("the discobox's origin has moved since your last push"),
	}}

	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the refusal", func() bool { return strings.Contains(m.status, "origin has moved") })
	if !m.statusE {
		t.Fatal("a refused push should read as an error")
	}
	m.status, m.statusE = "", false

	// The next look carries what is being held, and says nothing further about
	// it — the fake holds it back exactly as the data source does.
	d.dispatch(autoPushTickMsg{gen: m.wsGen})
	d.wait("the second look", func() bool { return len(ds.pushedCalls()) > 1 })
	d.settle()

	if got := ds.pushedCalls()[1]; got != "sbx_one primary=b7d0f1145aa2" {
		t.Fatalf("second look = %q, want the refused commit held", got)
	}
	if m.status != "" {
		t.Fatalf("status = %q, want a refusal reported once and not again", m.status)
	}
}

// A commit made after a refusal is a new tip, and a new attempt: the hold is
// released by the branch moving, not by anything the window is told.
func TestANewCommitReleasesAHeldPush(t *testing.T) {
	ds := newFakeSource(pushableSandboxes()...)
	ds.pushes = []SourcePush{{Slug: "primary", Branch: "main", Commit: "b7d0f1145aa2", Err: errors.New("refused")}}

	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the refusal", func() bool { return strings.Contains(m.status, "refused") })
	m.status, m.statusE = "", false

	// Somebody commits: the branch names something else now, and the hold on
	// the old commit no longer matches it.
	ds.mu.Lock()
	ds.pushes = []SourcePush{{Slug: "primary", Branch: "main", Commit: "c81e2ab30f45", Pushed: true}}
	ds.mu.Unlock()

	d.dispatch(autoPushTickMsg{gen: m.wsGen})
	d.wait("the push", func() bool { return strings.Contains(m.status, "pushed c81e2ab30f45") })
}

// The loop belongs to the workspace: leaving it ends the pushing, and a tick
// still in flight from the one that was left does nothing.
func TestLeavingTheWorkspaceStopsPushing(t *testing.T) {
	ds := newFakeSource(pushableSandboxes()...)
	ds.pushes = []SourcePush{{Slug: "primary", Branch: "main", Commit: "b7d0f1145aa2"}}

	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the first look", func() bool { return len(ds.pushedCalls()) > 0 })
	stale := m.wsGen

	d.key("ctrl+a")
	d.key("d")
	d.wait("the workspace to close", func() bool { return !m.inPanes() })
	before := len(ds.pushedCalls())

	d.dispatch(autoPushTickMsg{gen: stale})
	d.settle()

	if after := len(ds.pushedCalls()); after != before {
		t.Fatalf("looks = %d, want the loop to have ended with the workspace (%d)", after, before)
	}
}
