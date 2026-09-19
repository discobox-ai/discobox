package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The band says which harness has nothing to sign in with and sends the reader
// to configure it — out here, since a sign-in typed into the box cannot stick.
func TestTheUncredentialedBandNamesTheHarnessAndTheRemedy(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithUncredentialedHarness(t)

	if got := m.bannerShowing(); got != bannerUncredentialed {
		t.Fatalf("banner = %v, want the signed-out harness", got)
	}
	row := ansi.Strip(m.viewUncredentialedBanner(140))
	for _, want := range []string{"Claude Code", "no credentials", "configure", "on this machine", m.leader() + " " + rejectedKey} {
		if !strings.Contains(row, want) {
			t.Fatalf("band = %q, want it to say %q", row, want)
		}
	}
	lower := strings.ToLower(row)
	for _, forbidden := range []string{"/login", "log in here", "sign in here", "in the box", "inside"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("band = %q, want nothing pointing into the sandbox (%q)", row, forbidden)
		}
	}
}

// The workspace keeps the row it was opened from, and whether the harness has
// credentials changes under it: the band follows the listing, so a configure
// that binds something takes it down without the workspace being reopened.
func TestTheUncredentialedBandFollowsTheListing(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithUncredentialedHarness(t)

	fixed := m.list.all
	fixed[0].HarnessUncredentialed = false
	m.list.setAll(fixed)
	if got := m.bannerShowing(); got != bannerNone {
		t.Fatalf("banner = %v, want none once the listing says the harness has credentials", got)
	}

	// And the other way: a harness whose credential was deleted while the
	// workspace was open raises it.
	fixed[0].HarnessUncredentialed = true
	m.list.setAll(fixed)
	if got := m.bannerShowing(); got != bannerUncredentialed {
		t.Fatalf("banner = %v, want the band once the listing says the harness has none", got)
	}
}

// The request an agent makes because it has no credential is the symptom, as it
// is behind a refusal: the band stays up over it and says it is queued.
func TestTheUncredentialedBandOutranksTheRequestItProvokes(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithUncredentialedHarness(t)
	m.requests = map[string][]CredentialRequest{m.paneBox.ID: {waitingRequest()}}

	if got := m.bannerShowing(); got != bannerUncredentialed {
		t.Fatalf("banner = %v, want the signed-out harness over the request", got)
	}
	if row := ansi.Strip(m.viewUncredentialedBanner(160)); !strings.Contains(row, "waiting behind it") {
		t.Fatalf("band = %q, want it to say a request is queued", row)
	}
	if cmd := m.armBannerPulse(); cmd != nil || m.pulsing {
		t.Fatal("a request behind the signed-out harness started the throb")
	}
}

// A refused credential is the more specific sentence, and it is the one drawn.
func TestARefusedCredentialOutranksTheUncredentialedBand(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithUncredentialedHarness(t)
	m.setSecretRejections([]SecretRejection{secretRejection()})

	if got := m.bannerShowing(); got != bannerRejected {
		t.Fatalf("banner = %v, want the refusal over the signed-out harness", got)
	}
}

// Pressing it — or its key — opens the harness's setup over the workspace, on
// the discobox's own server rather than the one the header names.
func TestTheUncredentialedRemedyConfiguresTheHarnessOnItsServer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		press func(*Model) tea.Cmd
	}{
		// The band answers the bar that was drawn, so it is drawn first.
		{"band", func(m *Model) tea.Cmd { m.viewBanner(140); return m.pressBanner() }},
		{"key", func(m *Model) tea.Cmd { _, cmd := m.Update(openRejectedMsg{}); return cmd }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, ds := workspaceWithUncredentialedHarness(t)
			m.session.Servers = []string{"alpha", "beta"}
			m.list.server = ""
			boxes := m.list.all
			boxes[0].Server = "beta"
			m.list.setAll(boxes)
			m.paneBox.Server = "beta"
			ds.harnessesOn = map[string][]Harness{"beta": {claudeHarness()}}

			drain(t, m, tc.press(m), 0)

			if got := ds.configuredHarnesses(); len(got) != 1 || got[0] != "harness_claude" {
				t.Fatalf("configure opened for %v, want the signed-out harness", got)
			}
			calls := strings.Join(ds.calls(), " ")
			if !strings.Contains(calls, "OpenHarnessConfigure@beta") || strings.Contains(calls, "@alpha") {
				t.Fatalf("calls = %s, want the setup opened on beta alone", calls)
			}
		})
	}
}

// A bar over the harness's own setup telling you to run that setup is the
// window talking about what it is already doing.
func TestTheUncredentialedBandStepsAsideForItsOwnSetup(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithUncredentialedHarness(t)
	m.overlay = &pane{configure: &configurePane{harness: claudeHarness()}}

	if got := m.bannerShowing(); got == bannerUncredentialed {
		t.Fatal("the band stayed up over the harness's own configure flow")
	}
}

func claudeHarness() Harness {
	return Harness{
		ID: "harness_claude", Name: "Claude Code", Slug: "claude-code",
		State: HarnessEnabled, Configurable: true,
	}
}

// workspaceWithUncredentialedHarness is a window looking at a discobox whose
// harness has nothing bound. The flag is on the listing's row and not on the
// row the workspace was opened from, which is the shape a poll leaves.
func workspaceWithUncredentialedHarness(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	boxes := testSandboxes()
	ds := newFakeSource(boxes...)
	ds.harnesses = []Harness{claudeHarness()}
	m := newTestModel(t, ds)
	opened := boxes[0]
	opened.HarnessID = "harness_claude"
	m.paneBox = opened
	boxes[0].HarnessID, boxes[0].HarnessName, boxes[0].HarnessUncredentialed = "harness_claude", "Claude Code", true
	m.list.setAll(boxes)
	m.toolShown = &pane{tool: "diff"}
	return m, ds
}
