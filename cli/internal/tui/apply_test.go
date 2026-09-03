package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// readySandboxes is the fixture with its first discobox holding committed work
// that no apply has landed: a clean tree whose head has moved off the commit it
// was spawned from, which is the state the list spells "ready".
func readySandboxes() []Sandbox {
	all := testSandboxes()
	all[0].Dirty = false
	all[0].Git = GitState{Known: true, Branch: "main", Commit: "b7d0f11"}
	all[0].Diff = DiffStat{Known: true, Added: 142, Deleted: 38, Files: 7}
	return all
}

// The other half of the band the credential request gets: work that is finished
// and could come home, said in green rather than red, because this one is an
// offer rather than a person waiting on you.
func TestTheWorkspaceOffersToApplyWorkThatIsReady(t *testing.T) {
	ds := newFakeSource(readySandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the band", func() bool { return m.bannerTop() == 1 })
	d.wait("the band drawn", func() bool { return m.banner.live })

	if m.banner.kind != bannerApply {
		t.Fatalf("banner kind = %v, want the apply offer", m.banner.kind)
	}
	frame := plainFrame(m)
	for _, want := range []string{
		"ready to apply",
		"+142 −38 in 7 files", // how much is waiting, in the list's own spelling
		m.leader() + " " + applyKey,
		"click to apply",
	} {
		if !strings.Contains(frame, want) {
			t.Fatalf("the band does not carry %q:\n%s", want, frame)
		}
	}
}

// And nothing at all otherwise: the band is the exception on a screen that is
// otherwise all terminal, so a box with nothing to bring back does not get one.
func TestNoApplyBandWithoutCommittedWork(t *testing.T) {
	ds := newFakeSource(testSandboxes()...) // sbx_one is dirty, and has never committed
	_, m, _ := openWorkspace(t, ds, "enter")

	if m.bannerTop() != 0 {
		t.Fatalf("a band is up for a discobox with nothing applied:\n%s", plainFrame(m))
	}
	if strings.Contains(plainFrame(m), "ready to apply") {
		t.Fatalf("the offer is drawn on a dirty discobox:\n%s", plainFrame(m))
	}
}

// One band at a time, and the request wins: an agent blocked on a person
// outranks an offer that will still be there in a minute.
func TestACredentialRequestOutranksTheApplyOffer(t *testing.T) {
	ds := newFakeSource(readySandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the offer", func() bool { return m.bannerShowing() == bannerApply })

	ds.mu.Lock()
	ds.requests = []CredentialRequest{waitingRequest()}
	ds.mu.Unlock()
	d.dispatch(tickMsg{})
	d.wait("the request", func() bool { return m.bannerShowing() == bannerCredential })

	// Still one band, at one row's cost: two exception bars on one screen is a
	// header rather than an exception.
	if m.bannerCost() != 2 {
		t.Fatalf("bannerCost = %d, want the one band's two rows", m.bannerCost())
	}
	frame := plainFrame(m)
	if strings.Contains(frame, "ready to apply") {
		t.Fatalf("both bands are drawn:\n%s", frame)
	}
	if !strings.Contains(frame, "credential request") {
		t.Fatalf("the request's band is not up:\n%s", frame)
	}
}

// The key is deliberate — a leader chord, typed by somebody who read the bar
// that names it — so it runs the apply, the same as it does from the list.
func TestTheApplyKeyGoesStraightToTheApply(t *testing.T) {
	ds := newFakeSource(readySandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the band", func() bool { return m.bannerTop() == 1 })

	d.key("ctrl+a")
	d.key(applyKey)
	d.wait("the command", func() bool { return m.overlay != nil })

	if m.dialog != nil {
		t.Fatalf("the key asked a question: %q", dialogText(m))
	}
	if m.overlay.action != InteractApply {
		t.Fatalf("overlay = %s, want apply", m.overlay.action)
	}
	if len(ds.opens) != 1 || !strings.HasPrefix(ds.opens[0], "apply sbx_one ") {
		t.Fatalf("opens = %v, want apply on the discobox the band was about", ds.opens)
	}
	// The offer is not repeated over the top of the thing it offered.
	if m.bannerTop() != 0 {
		t.Fatalf("the band is still up over the running apply:\n%s", plainFrame(m))
	}
}

// A click is a press on a bar the width of the window, a row under the header,
// where a mistimed press on a tab lands. So it says what it is about to do to a
// repository outside the discobox before it does it.
func TestClickingTheApplyBandAsksFirst(t *testing.T) {
	ds := newFakeSource(readySandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the band drawn", func() bool { return m.banner.live })

	span := m.banner
	x := span.end - 2
	d.dispatch(tea.MouseClickMsg{X: x, Y: span.rows[1], Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: x, Y: span.rows[1], Button: tea.MouseLeft})
	d.wait("the question", func() bool { return m.dialog != nil && m.dialog.kind == dlgConfirm })

	if len(ds.opens) != 0 {
		t.Fatalf("opens = %v, want nothing applied before the question was answered", ds.opens)
	}
	text := dialogText(m)
	for _, want := range []string{"cherry-pick", "/src/disco2", "uncommitted"} {
		if !strings.Contains(text, want) {
			t.Fatalf("dialog = %q, want it to say %q", text, want)
		}
	}
	// The press belongs to the band: it must not also have started a selection
	// drag across the chrome.
	if m.chromeCapture {
		t.Fatal("the click also started a chrome selection")
	}

	d.key("y")
	d.wait("the command", func() bool { return m.overlay != nil })
	if len(ds.opens) != 1 || !strings.HasPrefix(ds.opens[0], "apply sbx_one ") {
		t.Fatalf("opens = %v, want the apply the question was about", ds.opens)
	}
}

// Saying no leaves the discobox exactly as it was, offer and all.
func TestDecliningTheApplyQuestionAppliesNothing(t *testing.T) {
	ds := newFakeSource(readySandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the band drawn", func() bool { return m.banner.live })

	span := m.banner
	d.dispatch(tea.MouseClickMsg{X: span.end - 2, Y: span.rows[0], Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: span.end - 2, Y: span.rows[0], Button: tea.MouseLeft})
	d.wait("the question", func() bool { return m.dialog != nil && m.dialog.kind == dlgConfirm })

	d.key("n")
	d.wait("the question to go", func() bool { return m.dialog == nil })
	if len(ds.opens) != 0 {
		t.Fatalf("opens = %v, want nothing applied", ds.opens)
	}
	if m.bannerShowing() != bannerApply {
		t.Fatal("the offer went away with the question")
	}
}
