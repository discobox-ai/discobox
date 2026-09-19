package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Dismissing a band shows what was queued behind it, not an empty header: the
// refusal outranks the request, and taking it down is how the request is seen.
func TestDismissingABandShowsTheOneBehindIt(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithRejection(t, harnessRejection())
	m.setCredentialRequests([]CredentialRequest{waitingRequest()})

	m.dismissBanner()
	if got := m.bannerShowing(); got != bannerCredential {
		t.Fatalf("banner = %v, want the request once the refusal is dismissed", got)
	}
	m.dismissBanner()
	if got := m.bannerShowing(); got != bannerNone {
		t.Fatalf("banner = %v, want none once both are dismissed", got)
	}
}

// The same refusal read again stays dismissed — the poll re-reads it every few
// seconds, and a band that came back on each read was never dismissed. A
// refusal that was cleared and happened again is a new one, and shows.
func TestADismissedRefusalComesBackOnlyWhenItHappensAgain(t *testing.T) {
	t.Parallel()
	rejection := harnessRejection()
	m, _ := workspaceWithRejection(t, rejection)
	m.dismissBanner()

	m.setSecretRejections([]SecretRejection{rejection})
	if got := m.bannerShowing(); got != bannerNone {
		t.Fatalf("banner = %v, want the same refusal to stay dismissed across a poll", got)
	}

	again := rejection
	again.FirstSeen = time.Now()
	m.setSecretRejections([]SecretRejection{again})
	if got := m.bannerShowing(); got != bannerRejected {
		t.Fatalf("banner = %v, want a refusal first seen since to show", got)
	}
}

// The case the whole design turns on. The harness is configured while its band
// is dismissed; the read that says so must forget the dismissal, so that the
// harness signed out again — with nothing about it to tell the two apart — is
// shown rather than taken for the one somebody already dismissed.
func TestADismissalEndsWhenWhatItWasAboutIsOver(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithUncredentialedHarness(t)
	m.dismissBanner()
	if got := m.bannerShowing(); got != bannerNone {
		t.Fatalf("banner = %v, want the signed-out harness dismissed", got)
	}

	// Still signed out on the next poll: still dismissed.
	boxes := m.list.all
	m.list.setAll(boxes)
	m.pruneDismissed()
	if got := m.bannerShowing(); got != bannerNone {
		t.Fatalf("banner = %v, want it to stay dismissed while nothing changed", got)
	}

	// Configured, then signed out again: its name for itself never changed,
	// and it is shown anyway.
	boxes[0].HarnessUncredentialed = false
	m.list.setAll(boxes)
	m.pruneDismissed()
	boxes[0].HarnessUncredentialed = true
	m.list.setAll(boxes)
	m.pruneDismissed()
	if got := m.bannerShowing(); got != bannerUncredentialed {
		t.Fatalf("banner = %v, want the band back once the harness is signed out again", got)
	}
}

// A dismissal covers the requests it saw. Answering one of them leaves the
// rest dismissed; a request that arrives after brings the band back.
func TestADismissedRequestBandReturnsForANewRequest(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithUncredentialedHarness(t)
	m.list.all[0].HarnessUncredentialed = false
	first, second := waitingRequest(), waitingRequest()
	first.ID, second.ID = "req_1", "req_2"
	for _, req := range []*CredentialRequest{&first, &second} {
		req.SandboxID = m.paneBox.ID
	}
	m.setCredentialRequests([]CredentialRequest{first, second})
	m.dismissBanner()

	m.setCredentialRequests([]CredentialRequest{second})
	if got := m.bannerShowing(); got != bannerNone {
		t.Fatalf("banner = %v, want a request already dismissed to stay dismissed", got)
	}

	third := second
	third.ID = "req_3"
	m.setCredentialRequests([]CredentialRequest{second, third})
	if got := m.bannerShowing(); got != bannerCredential {
		t.Fatalf("banner = %v, want a new request to bring the band back", got)
	}
}

// A dismissed offer to apply is about the work it would apply: more work is a
// new offer.
func TestADismissedApplyOfferReturnsForNewWork(t *testing.T) {
	t.Parallel()
	ready := readySandboxes()
	m := newTestModel(t, newFakeSource(ready...))
	m.list.setAll(ready)
	m.paneBox = ready[0]
	m.toolShown = &pane{tool: "diff"}
	if got := m.bannerShowing(); got != bannerApply {
		t.Fatalf("banner = %v, want the offer", got)
	}
	m.dismissBanner()

	ready[0].Git.Commit = "c0ffee1"
	m.list.setAll(ready)
	m.pruneDismissed()
	if got := m.bannerShowing(); got != bannerApply {
		t.Fatalf("banner = %v, want the offer back for a new commit", got)
	}
}

// A band is about one discobox. Dismissing a signed-out harness in one box
// leaves it up in another box on the same harness.
func TestADismissalIsForOneDiscobox(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithUncredentialedHarness(t)
	boxes := m.list.all
	boxes[1].HarnessID, boxes[1].HarnessName, boxes[1].HarnessUncredentialed = "harness_claude", "Claude Code", true
	m.list.setAll(boxes)
	m.dismissBanner()

	m.paneBox = boxes[1]
	if got := m.bannerShowing(); got != bannerUncredentialed {
		t.Fatalf("banner = %v, want the other box's band still up", got)
	}
}

// The ✕ is on the band and pressing it dismisses; pressing anywhere else on
// the band still does what the band says.
func TestTheBandsCloseButtonDismissesIt(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.requests = []CredentialRequest{waitingRequest()}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the banner drawn", func() bool { return m.banner.live })

	top := frame(m)[m.banner.rows[0]]
	if !strings.Contains(ansi.Strip(top), "✕") {
		t.Fatalf("band = %q, want a dismiss button on it", ansi.Strip(top))
	}
	x, y := m.banner.closeStart+1, m.banner.rows[0]
	// The hit test is where the glyph was drawn, not where it ought to be.
	if got := ansi.Strip(ansi.Cut(top, x, x+1)); got != "✕" {
		t.Fatalf("cell %d of the band is %q, want the dismiss button", x, got)
	}
	if !m.bannerCloseAt(x, y) || m.bannerCloseAt(m.banner.start+1, y) {
		t.Fatal("the dismiss button's hit test is not the button")
	}
	d.dispatch(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.wait("the band gone", func() bool { return m.bannerShowing() == bannerNone })
	if m.dialog != nil {
		t.Fatalf("dialog = %s, want the dismiss to open nothing", describe(m.dialog))
	}
}

// The leader key is the button from the keyboard.
func TestTheLeaderKeyDismissesTheBand(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithUncredentialedHarness(t)
	m.Update(dismissBannerMsg{})
	if got := m.bannerShowing(); got != bannerNone {
		t.Fatalf("banner = %v, want it dismissed", got)
	}
}

// The key both credential bands print answers the band that is drawn. With a
// refusal dismissed from under it, the signed-out harness's band is up, and its
// key opens the harness's setup — not the dismissed refusal's card, which is
// what precedence alone would pick.
func TestTheCredentialKeyAnswersTheBandOnScreen(t *testing.T) {
	t.Parallel()
	m, ds := workspaceWithUncredentialedHarness(t)
	ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "GitHub token", Type: "token"}}
	m.setSecretRejections([]SecretRejection{secretRejection()})
	m.dismissBanner()
	if got := m.bannerShowing(); got != bannerUncredentialed {
		t.Fatalf("banner = %v, want the signed-out harness once the refusal is dismissed", got)
	}

	_, cmd := m.Update(openRejectedMsg{})
	drain(t, m, cmd, 0)
	if got := ds.configuredHarnesses(); len(got) != 1 || got[0] != "harness_claude" {
		t.Fatalf("configure opened for %v, want the harness whose band is drawn", got)
	}
	if m.dialog != nil && strings.Contains(m.dialog.title, "GitHub token") {
		t.Fatal("the key opened the dismissed refusal's card")
	}
}

// A dismissal covers the refusals it saw. A second refusal on the same box,
// while the first is still outstanding, is a new occurrence and shows.
func TestASecondRefusalShowsPastADismissedFirst(t *testing.T) {
	t.Parallel()
	first := secretRejection()
	m, _ := workspaceWithRejection(t, first)
	m.dismissBanner()

	second := secretRejection()
	second.SecretID, second.SecretName, second.FirstSeen = "sec_npm", "npm token", time.Now()
	m.setSecretRejections([]SecretRejection{first, second})
	if got := m.bannerShowing(); got != bannerRejected {
		t.Fatalf("banner = %v, want the second refusal shown past the dismissed first", got)
	}
	// About the second: what it says, and what pressing it opens. The first
	// is the one somebody already dismissed.
	if row := ansi.Strip(m.viewBanner(140)); !strings.Contains(row, "npm token") || strings.Contains(row, "GitHub token") {
		t.Fatalf("band = %q, want it to name the new refusal", row)
	}
	if got, _ := m.shownRejection(m.currentBox()); got.SecretID != "sec_npm" {
		t.Fatalf("the band acts on %s, want the new refusal", got.SecretID)
	}
	// Dismissed again, it stays down: both are covered now.
	m.dismissBanner()
	if got := m.bannerShowing(); got != bannerNone {
		t.Fatalf("banner = %v, want both refusals dismissed", got)
	}
}

// The band that is chosen and the band that is drawn read the same row. The
// workspace's own copy of the box, taken when it opened, does not know the
// harness; the listing does — and the refusal is both chosen and drawn from
// the listing, rather than chosen from one and drawn blank from the other.
func TestTheRefusalIsChosenAndDrawnFromTheSameRow(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithRejection(t, harnessRejection())
	m.paneBox.HarnessID = ""

	if got := m.bannerShowing(); got != bannerRejected {
		t.Fatalf("banner = %v, want the refusal the listing's row is affected by", got)
	}
	if row := m.viewBanner(120); row == "" {
		t.Fatal("the refusal was chosen and then drew nothing: two rows spent on no band")
	}
}

// The dismiss button is its own cells. The pad and border to the right of the
// band are the band, as they were before there was a button.
func TestTheDismissButtonIsOnlyItsCells(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.requests = []CredentialRequest{waitingRequest()}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the banner drawn", func() bool { return m.banner.live })

	y := m.banner.rows[0]
	if m.banner.closeEnd >= m.banner.end {
		t.Fatalf("button ends at %d, the band at %d: want padding right of the button", m.banner.closeEnd, m.banner.end)
	}
	if m.bannerCloseAt(m.banner.closeEnd+1, y) || !m.bannerAt(m.banner.closeEnd+1, y) {
		t.Fatal("the cell right of the button should press the band, not dismiss it")
	}
}
