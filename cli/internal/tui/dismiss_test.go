package tui

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// Dismissing a band shows what was queued behind it, not an empty header: the
// refusal outranks the request, and taking it down is how the request is seen.
func TestDismissingABandShowsTheOneBehindIt(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithRejection(t, harnessRejection())
	m.setCredentialRequests([]console.CredentialRequest{waitingRequest()})

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

	m.setSecretRejections([]console.SecretRejection{rejection})
	if got := m.bannerShowing(); got != bannerNone {
		t.Fatalf("banner = %v, want the same refusal to stay dismissed across a poll", got)
	}

	again := rejection
	again.FirstSeen = time.Now()
	m.setSecretRejections([]console.SecretRejection{again})
	if got := m.bannerShowing(); got != bannerRejected {
		t.Fatalf("banner = %v, want a refusal first seen since to show", got)
	}
}

// workspaceWithHarnessChanges is a workspace open on a box whose harness has a
// change to save, as the listing reports it.
func workspaceWithHarnessChanges(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	boxes := testSandboxes()
	ds := newFakeSource(boxes...)
	m := newTestModel(t, ds)
	boxes[0].HarnessID, boxes[0].HarnessName = "harness_claude", "Claude Code"
	boxes[0].HarnessConfigPending, boxes[0].HarnessConfigCandidate = true, "cand_1"
	m.Box = boxes[0]
	m.list.setAll(boxes)
	m.toolShown = &pane{Tool: "diff"}
	return m, ds
}

// A dismissed harness change stays down while the box reports the same
// candidate, and comes back for a different one: the box captured another
// change, which nobody has seen.
func TestADismissedHarnessChangeComesBackForANewCandidate(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithHarnessChanges(t)
	if got := m.bannerShowing(); got != bannerHarnessChanges {
		t.Fatalf("banner = %v, want the harness change", got)
	}
	m.dismissBanner()
	if got := m.bannerShowing(); got != bannerNone {
		t.Fatalf("banner = %v, want the harness change dismissed", got)
	}

	boxes := m.list.All()
	m.list.setAll(boxes)
	m.pruneDismissed()
	if got := m.bannerShowing(); got != bannerNone {
		t.Fatalf("banner = %v, want it to stay dismissed while the candidate is the same", got)
	}

	boxes[0].HarnessConfigCandidate = "cand_2"
	m.list.setAll(boxes)
	m.pruneDismissed()
	if got := m.bannerShowing(); got != bannerHarnessChanges {
		t.Fatalf("banner = %v, want the band back for a new candidate", got)
	}
}

// The harness change is last: a request, which a person is being waited on
// for, is drawn over it, and dismissing the request reveals it.
func TestAHarnessChangeQueuesBehindARequest(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithHarnessChanges(t)
	req := waitingRequest()
	req.SandboxID = m.Box.ID
	m.setCredentialRequests([]console.CredentialRequest{req})
	if got := m.bannerShowing(); got != bannerCredential {
		t.Fatalf("banner = %v, want the request over the harness change", got)
	}
	m.dismissBanner()
	if got := m.bannerShowing(); got != bannerHarnessChanges {
		t.Fatalf("banner = %v, want the harness change once the request is dismissed", got)
	}
}

// A dismissal covers the requests it saw. Answering one of them leaves the
// rest dismissed; a request that arrives after brings the band back.
func TestADismissedRequestBandReturnsForANewRequest(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithHarnessChanges(t)
	m.list.All()[0].HarnessConfigPending, m.list.All()[0].HarnessConfigCandidate = false, ""
	first, second := waitingRequest(), waitingRequest()
	first.ID, second.ID = "req_1", "req_2"
	for _, req := range []*console.CredentialRequest{&first, &second} {
		req.SandboxID = m.Box.ID
	}
	m.setCredentialRequests([]console.CredentialRequest{first, second})
	m.dismissBanner()

	m.setCredentialRequests([]console.CredentialRequest{second})
	if got := m.bannerShowing(); got != bannerNone {
		t.Fatalf("banner = %v, want a request already dismissed to stay dismissed", got)
	}

	third := second
	third.ID = "req_3"
	m.setCredentialRequests([]console.CredentialRequest{second, third})
	if got := m.bannerShowing(); got != bannerCredential {
		t.Fatalf("banner = %v, want a new request to bring the band back", got)
	}
}

// A band is about one discobox. Dismissing a harness change in one box leaves
// another box's up, even with the same candidate name.
func TestADismissalIsForOneDiscobox(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithHarnessChanges(t)
	boxes := m.list.All()
	boxes[1].HarnessID, boxes[1].HarnessName = "harness_claude", "Claude Code"
	boxes[1].HarnessConfigPending, boxes[1].HarnessConfigCandidate = true, "cand_1"
	m.list.setAll(boxes)
	m.dismissBanner()

	m.Box = boxes[1]
	if got := m.bannerShowing(); got != bannerHarnessChanges {
		t.Fatalf("banner = %v, want the other box's band still up", got)
	}
}

// The ✕ is on the band and pressing it dismisses; pressing anywhere else on
// the band still does what the band says.
func TestTheBandsCloseButtonDismissesIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		ds.requests = []console.CredentialRequest{waitingRequest()}
		d, m, _ := openWorkspace(t, ds, "enter")
		d.wait("the banner drawn", func() bool { return m.banner.live })

		top := windowLines(m)[m.banner.rows[0]]
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
		clickWindow(d, x, y)
		d.wait("the band gone", func() bool { return m.bannerShowing() == bannerNone })
		if m.dialog != nil {
			t.Fatalf("dialog = %s, want the dismiss to open nothing", describe(m.dialog))
		}
	})
}

// The leader key is the button from the keyboard — pressed in the pane the
// keys are typed in, not posted: the pane hands its bindings' messages back
// tagged with itself, and one the window does not take from there is dropped.
func TestTheLeaderKeyDismissesTheBand(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		boxes := testSandboxes()
		boxes[0].HarnessName = "Claude Code"
		boxes[0].HarnessConfigCandidate, boxes[0].HarnessConfigPending = "cand_1", true
		ds := newFakeSource(boxes...)
		d, m, _ := openWorkspace(t, ds, "enter")
		d.wait("the harness change band", func() bool { return m.bannerShowing() == bannerHarnessChanges })
		d.key("ctrl+a")
		d.key(bannerDismissKey)
		d.wait("the band dismissed", func() bool { return m.bannerShowing() == bannerNone })
	})
}

// The refusal's key, pressed in the pane, opens its remedy: for a harness's
// shared login, the dialog saying to sign in again in the box.
func TestTheRefusalKeyOpensItsRemedyFromAPane(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		boxes := testSandboxes()
		boxes[0].HarnessID = harnessRejection().HarnessConfigID
		ds := newFakeSource(boxes...)
		ds.rejections = []console.SecretRejection{harnessRejection()}
		d, m, _ := openWorkspace(t, ds, "enter")
		d.wait("the refusal band", func() bool { return m.bannerShowing() == bannerRejected })
		d.key("ctrl+a")
		d.key(rejectedKey)
		d.wait("the remedy", func() bool { return m.dialog != nil })
	})
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
	m.setSecretRejections([]console.SecretRejection{first, second})
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
	m.Box.HarnessID = ""

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
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		ds.requests = []console.CredentialRequest{waitingRequest()}
		d, m, _ := openWorkspace(t, ds, "enter")
		d.wait("the banner drawn", func() bool { return m.banner.live })

		y := m.banner.rows[0]
		if m.banner.closeEnd >= m.banner.end {
			t.Fatalf("button ends at %d, the band at %d: want padding right of the button", m.banner.closeEnd, m.banner.end)
		}
		if m.bannerCloseAt(m.banner.closeEnd+1, y) || !m.bannerAt(m.banner.closeEnd+1, y) {
			t.Fatal("the cell right of the button should press the band, not dismiss it")
		}
	})
}
