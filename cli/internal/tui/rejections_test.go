package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// The band's whole first job: say which credential was refused and where.
//
// From inside a discobox a refused credential is indistinguishable from
// Discobox failing to deliver one, so the bar has to answer "is this the
// product or is this my token" before it offers to do anything about it.
func TestTheRefusedBandNamesTheCredentialAndTheHost(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithRejection(t, harnessRejection())

	row := ansi.Strip(m.viewRejectedBanner(120))
	for _, want := range []string{"Claude Code", "refused", "api.anthropic.com", "click to sign in"} {
		t.Run(want, func(t *testing.T) {
			if !strings.Contains(row, want) {
				t.Fatalf("band = %q, want it to say %q", row, want)
			}
		})
	}
}

// Nothing on the bar may read as an invitation to sign in inside the discobox.
// That cannot work and cannot stick — the sandbox agent puts the delivered
// sentinel back within thirty seconds (ADR 0059) — so a user sent there loses
// the time twice.
func TestTheRefusedBandDoesNotSendAnybodyIntoTheBox(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithRejection(t, harnessRejection())

	row := strings.ToLower(ansi.Strip(m.viewRejectedBanner(120)))
	for _, forbidden := range []string{"/login", "log in here", "sign in here", "in the box", "inside"} {
		if strings.Contains(row, forbidden) {
			t.Fatalf("band = %q, want nothing pointing into the sandbox (%q)", row, forbidden)
		}
	}
	// And the positive half, which is the one that does the work: a reader
	// mid-session in the terminal under this bar has to be told the sign-in
	// happens somewhere else, or "sign in again" is read as the /login they
	// were already about to type.
	if !strings.Contains(row, "on this machine") {
		t.Fatalf("band = %q, want it to say where the sign-in happens", row)
	}
}

// The same for a credential no harness owns: replacing a value is not something
// that can be done from inside the box either.
func TestTheRefusedBandPlacesEveryRemedyOutsideTheBox(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithRejection(t, secretRejection())

	row := strings.ToLower(ansi.Strip(m.viewRejectedBanner(120)))
	if !strings.Contains(row, "on this machine") {
		t.Fatalf("band = %q, want it to say where the credential is replaced", row)
	}
}

// A harness credential is refused for every discobox running that harness, not
// only the one that happened to send the request that found out.
func TestARefusedHarnessCredentialMarksEveryBoxOnThatHarness(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithRejection(t, harnessRejection())

	onHarness := Sandbox{ID: "sbx_elsewhere", Name: "another box", HarnessID: "harness_claude"}
	if !m.hasRejection(onHarness) {
		t.Fatal("a second box on the same harness was not marked")
	}
	other := Sandbox{ID: "sbx_other", Name: "codex box", HarnessID: "harness_codex"}
	if m.hasRejection(other) {
		t.Fatal("a box on a different harness was marked")
	}
}

// Anything else is about the discobox that ran into it: a project token that
// failed in one box says nothing about a box that has never used it.
func TestARefusedProjectSecretMarksOnlyTheBoxThatSawIt(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithRejection(t, secretRejection())

	if !m.hasRejection(Sandbox{ID: "sbx_one", HarnessID: "harness_claude"}) {
		t.Fatal("the box that saw the rejection was not marked")
	}
	if m.hasRejection(Sandbox{ID: "sbx_two", HarnessID: "harness_claude"}) {
		t.Fatal("a box that never used the credential was marked")
	}
}

// A refused credential outranks everything, the request an agent makes about it
// above all: the agent took the 401 and asked for a credential, so the two are
// one event and the request is the half that cannot be usefully answered.
func TestARefusedCredentialOutranksTheRequestItProvoked(t *testing.T) {
	t.Parallel()
	ready := readySandboxes()
	ds := newFakeSource(ready...)
	ds.rejections = []SecretRejection{{
		SecretID: "sec_claude", SecretName: "claude-code", Host: "api.anthropic.com",
		Reason: "unrefreshable", HarnessConfigID: "harness_claude", HarnessConfigName: "Claude Code",
		EnvName: "ANTHROPIC_API_KEY", FirstSeen: time.Now().Add(-time.Hour),
	}}
	m := newTestModel(t, ds)
	ready[0].HarnessID = "harness_claude"
	m.list.setAll(ready)
	box := ready[0]
	m.paneBox = box
	m.toolShown = &pane{tool: "diff"}
	m.setSecretRejections(ds.rejections)

	if got := m.bannerShowing(); got != bannerRejected {
		t.Fatalf("banner = %v, want the refused credential over the apply offer", got)
	}

	// The agent's own reaction to the 401 must not displace it.
	m.requests = map[string][]CredentialRequest{box.ID: {waitingRequest()}}
	if got := m.bannerShowing(); got != bannerRejected {
		t.Fatalf("banner = %v, want the refusal to stay up with a request behind it", got)
	}
	// And it says the request is there, so a queued question is not a mystery.
	row := ansi.Strip(m.viewRejectedBanner(140))
	if !strings.Contains(row, "waiting behind it") {
		t.Fatalf("band = %q, want it to say a request is queued", row)
	}

	// Dealt with, and the queue moves on: the request is next, then the offer.
	m.setSecretRejections(nil)
	if got := m.bannerShowing(); got != bannerCredential {
		t.Fatalf("banner = %v, want the request once the refusal is cleared", got)
	}
	m.requests = nil
	if got := m.bannerShowing(); got != bannerApply {
		t.Fatalf("banner = %v, want the offer once both are gone", got)
	}
}

// The refusal is the one thing to act on, so the request queued behind it does
// not throb either: one moving thing on a screen, and it is not this.
func TestAQueuedRequestBehindARefusalDoesNotThrob(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithRejection(t, harnessRejection())
	m.st = newStyles(true)
	m.requests = map[string][]CredentialRequest{m.paneBox.ID: {waitingRequest()}}

	if cmd := m.armBannerPulse(); cmd != nil || m.pulsing {
		t.Fatal("a request behind a refusal started the throb")
	}
}

// The throb is for an agent stopped mid-sentence with somebody in the loop. A
// credential that has been dead for an hour will still be dead in a minute, and
// a second moving bar would spend the attention the first one is for.
func TestTheRefusedBandDoesNotThrob(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithRejection(t, harnessRejection())
	m.st = newStyles(true)

	if cmd := m.armBannerPulse(); cmd != nil || m.pulsing {
		t.Fatal("the refused credential's band got a clock")
	}
}

// Pressing it opens the harness's own setup, over the discobox you were
// watching — the fix is out here, and the session is still there when it is
// done.
func TestPressingTheRefusedBandOpensTheHarnessSetup(t *testing.T) {
	t.Parallel()
	m, ds := workspaceWithRejection(t, harnessRejection())
	ds.harnesses = []Harness{{
		ID: "harness_claude", Name: "Claude Code", Slug: "claude-code",
		State: HarnessEnabled, Configurable: true,
	}}

	drain(t, m, m.openRejectedRemedy(m.paneBox), 0)

	if got := ds.configuredHarnesses(); len(got) != 1 || got[0] != "harness_claude" {
		t.Fatalf("configure opened for %v, want the harness that owns the credential", got)
	}
}

// And for anything else, the card that replaces the value — which is the whole
// of that remedy.
func TestPressingTheRefusedBandOpensTheSecretCard(t *testing.T) {
	t.Parallel()
	m, ds := workspaceWithRejection(t, secretRejection())
	ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "GitHub token", Type: "token", Host: "api.github.com"}}

	drain(t, m, m.openRejectedRemedy(m.paneBox), 0)

	if m.dialog == nil || !strings.Contains(m.dialog.title, "GitHub token") {
		t.Fatalf("dialog = %s, want the card for the refused credential", describe(m.dialog))
	}
}

// The secrets screen answers the same question from the same read: which of
// these credentials does not work. What stands on a credential matters less
// than whether it works at all.
func TestTheSecretsScreenMarksARefusedCredential(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithRejection(t, secretRejection())
	m.secrets.setAll([]Secret{
		{ID: "sec_gh", Name: "GitHub token", Host: "api.github.com", Grants: 2},
		{ID: "sec_openai", Name: "OpenAI key", Host: "api.openai.com", Grants: 1},
	})
	m.secrets.width, m.secrets.height = 100, 4

	var zones zones
	view := ansi.Strip(m.secrets.view(m.st, &zones, true))
	lines := strings.Split(view, "\n")
	var refused, working string
	for _, row := range lines {
		switch {
		case strings.Contains(row, "GitHub token"):
			refused = row
		case strings.Contains(row, "OpenAI key"):
			working = row
		}
	}
	if !strings.Contains(refused, "refused") {
		t.Fatalf("row = %q, want the refused credential to say so", refused)
	}
	if strings.Contains(working, "refused") {
		t.Fatalf("row = %q, want an unaffected credential left alone", working)
	}
}

// A credential that has been replaced stops being news: the band goes, and the
// panes get their rows back.
func TestClearingARejectionTakesTheBandAway(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithRejection(t, harnessRejection())

	if m.bannerShowing() != bannerRejected {
		t.Fatalf("banner = %v, want the refused credential", m.bannerShowing())
	}
	m.setSecretRejections(nil)
	if got := m.bannerShowing(); got != bannerNone {
		t.Fatalf("banner = %v, want none once the credential was replaced", got)
	}
}

func harnessRejection() SecretRejection {
	//nolint:gosec // G101: an environment variable name, which is what this
	// whole feature is about naming; there is no credential in this package.
	return SecretRejection{
		SecretID:          "sec_claude",
		SecretName:        "claude-code oauth",
		SecretType:        "oauth",
		Host:              "api.anthropic.com",
		Reason:            "refresh-failed",
		EnvName:           "ANTHROPIC_API_KEY",
		SandboxID:         "sbx_one",
		HarnessConfigID:   "harness_claude",
		HarnessConfigName: "Claude Code",
		FirstSeen:         time.Now().Add(-time.Hour),
		LastSeen:          time.Now().Add(-time.Minute),
	}
}

func secretRejection() SecretRejection {
	return SecretRejection{
		SecretID:   "sec_gh",
		SecretName: "GitHub token",
		SecretType: "token",
		Host:       "api.github.com",
		Reason:     "unrefreshable",
		SandboxID:  "sbx_one",
		FirstSeen:  time.Now().Add(-time.Hour),
		LastSeen:   time.Now().Add(-time.Minute),
	}
}

// workspaceWithRejection is a window looking at a discobox with one refused
// credential against it.
func workspaceWithRejection(t *testing.T, rejection SecretRejection) (*Model, *fakeSource) {
	t.Helper()
	boxes := testSandboxes()
	ds := newFakeSource(boxes...)
	ds.rejections = []SecretRejection{rejection}
	m := newTestModel(t, ds)
	// The band reads the listing's row, which carries the harness as the
	// workspace's does.
	boxes[0].HarnessID = "harness_claude"
	m.list.setAll(boxes)
	m.paneBox = boxes[0]
	// A tool window is a workspace as far as the band is concerned: the bar is
	// drawn over whatever the screen is showing.
	m.toolShown = &pane{tool: "diff"}
	m.setSecretRejections(ds.rejections)
	return m, ds
}

// The fix goes where the credential lives. The harnesses and secrets screens act
// on the server the header names; a refused credential's band acts on the
// credential's own server, which need not be the same one — and configuring the
// other server's harness of the same name is the mistake ADR 0131 §2 exists to
// prevent.
func TestTheRefusedBandsRemedyGoesToTheCredentialsServer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		rejection SecretRejection
		want      string
	}{
		{"harness", harnessRejection(), "OpenHarnessConfigure@beta"},
		{"secret", secretRejection(), "Secrets@beta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rejection := tc.rejection
			rejection.Server = "beta"
			m, ds := workspaceWithRejection(t, rejection)
			m.session.Servers = []string{"alpha", "beta"}
			// The header is on the primary; the box and its credential are on
			// beta.
			m.list.server = ""
			m.paneBox.Server = "beta"
			m.setSecretRejections([]SecretRejection{rejection})
			ds.harnessesOn = map[string][]Harness{"beta": {{
				ID: "harness_claude", Name: "Claude Code", Slug: "claude-code",
				State: HarnessEnabled, Configurable: true,
			}}}
			ds.secretsOn = map[string][]Secret{"beta": {{ID: "sec_gh", Name: "GitHub token", Type: "token"}}}

			drain(t, m, m.openRejectedRemedy(m.paneBox), 0)

			calls := strings.Join(ds.calls(), " ")
			if !strings.Contains(calls, tc.want) {
				t.Fatalf("calls = %s, want %s", calls, tc.want)
			}
			if strings.Contains(calls, "@alpha") {
				t.Fatalf("calls = %s, want nothing sent to the server the header names", calls)
			}
		})
	}
}

// A box on one server is not affected by a credential refused on another: IDs
// are only unique within a server.
func TestARejectionOnlyMarksBoxesOnItsOwnServer(t *testing.T) {
	t.Parallel()
	rejection := harnessRejection()
	rejection.Server = "beta"
	m, _ := workspaceWithRejection(t, rejection)
	m.session.Servers = []string{"alpha", "beta"}
	m.setSecretRejections([]SecretRejection{rejection})

	onBeta := Sandbox{ID: "sbx_b", HarnessID: "harness_claude", Server: "beta"}
	onAlpha := Sandbox{ID: "sbx_a", HarnessID: "harness_claude", Server: "alpha"}
	if !m.hasRejection(onBeta) {
		t.Fatal("a box on the credential's own server was not marked")
	}
	if m.hasRejection(onAlpha) {
		t.Fatal("a box on another server was marked by a harness ID that only means something on beta")
	}
}
