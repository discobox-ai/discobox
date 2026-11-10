package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/secretroute"
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

// A harness's refused login is replaced the way it was made: signing in again
// in a discobox and saving the change (ADR 26-11-02-152). The bar says so.
func TestTheRefusedHarnessBandSaysToSignInAgain(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithRejection(t, harnessRejection())

	row := strings.ToLower(ansi.Strip(m.viewRejectedBanner(120)))
	if !strings.Contains(row, "sign in again") {
		t.Fatalf("band = %q, want it to say to sign in again", row)
	}
	if strings.Contains(row, "on this machine") {
		t.Fatalf("band = %q, want no remedy sent outside the box", row)
	}
}

// A credential no harness owns is replaced out here: replacing a value is not
// something that can be done from inside the box.
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

	onHarness := console.Sandbox{ID: "sbx_elsewhere", Name: "another box", HarnessID: "harness_claude"}
	if !m.inbox().Wanted(console.NoticeRejected, onHarness) {
		t.Fatal("a second box on the same harness was not marked")
	}
	other := console.Sandbox{ID: "sbx_other", Name: "codex box", HarnessID: "harness_codex"}
	if m.inbox().Wanted(console.NoticeRejected, other) {
		t.Fatal("a box on a different harness was marked")
	}
}

// Anything else is about the discobox that ran into it: a project token that
// failed in one box says nothing about a box that has never used it.
func TestARefusedProjectSecretMarksOnlyTheBoxThatSawIt(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithRejection(t, secretRejection())

	if !m.inbox().Wanted(console.NoticeRejected, console.Sandbox{ID: "sbx_one", HarnessID: "harness_claude"}) {
		t.Fatal("the box that saw the rejection was not marked")
	}
	if m.inbox().Wanted(console.NoticeRejected, console.Sandbox{ID: "sbx_two", HarnessID: "harness_claude"}) {
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
	ds.rejections = []console.SecretRejection{{
		SecretID: "sec_claude", SecretName: "claude-code", Host: "api.anthropic.com",
		Reason: "unrefreshable", HarnessConfigID: "harness_claude", HarnessConfigName: "Claude Code",
		EnvName: "ANTHROPIC_API_KEY", FirstSeen: time.Now().Add(-time.Hour),
	}}
	m := newTestModel(t, ds)
	ready[0].HarnessID = "harness_claude"
	m.list.setAll(ready)
	box := ready[0]
	m.Box = box
	m.toolShown = &pane{Tool: "diff"}
	m.setSecretRejections(ds.rejections)

	if got := m.bannerShowing(); got != console.NoticeRejected {
		t.Fatalf("banner = %v, want the refused credential", got)
	}

	// The agent's own reaction to the 401 must not displace it.
	m.requests = map[string][]console.CredentialRequest{box.ID: {waitingRequest()}}
	if got := m.bannerShowing(); got != console.NoticeRejected {
		t.Fatalf("banner = %v, want the refusal to stay up with a request behind it", got)
	}
	// And it says the request is there, so a queued question is not a mystery.
	row := ansi.Strip(m.viewRejectedBanner(140))
	if !strings.Contains(row, "waiting behind it") {
		t.Fatalf("band = %q, want it to say a request is queued", row)
	}

	// Dealt with, and the queue moves on: the request is next, then nothing.
	m.setSecretRejections(nil)
	if got := m.bannerShowing(); got != console.NoticeRequest {
		t.Fatalf("banner = %v, want the request once the refusal is cleared", got)
	}
	m.requests = nil
	if got := m.bannerShowing(); got != console.NoNotice {
		t.Fatalf("banner = %v, want no band once both are gone", got)
	}
}

// The refusal is the one thing to act on, so the request queued behind it does
// not throb either: one moving thing on a screen, and it is not this.
func TestAQueuedRequestBehindARefusalDoesNotThrob(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithRejection(t, harnessRejection())
	m.st = newStyles(true)
	m.requests = map[string][]console.CredentialRequest{m.Box.ID: {waitingRequest()}}

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

// Pressing it for a harness's login says how to sign in again — in the box,
// then save the change — and reads nothing: there is no setup out here to open.
func TestPressingTheRefusedBandExplainsSigningInAgain(t *testing.T) {
	t.Parallel()
	m, ds := workspaceWithRejection(t, harnessRejection())
	before := len(ds.calls())

	drain(t, m, m.openRejectedRemedy(m.Box), 0)

	if m.dialog == nil || m.dialog.kind != dlgMessage {
		t.Fatalf("dialog = %s, want the sign-in explanation", describe(m.dialog))
	}
	for _, want := range []string{"Sign in again in a new discobox", "save it", "replaces the refused login"} {
		if !strings.Contains(m.dialog.body, want) {
			t.Fatalf("body = %q, want %q", m.dialog.body, want)
		}
	}
	if calls := ds.calls(); len(calls) != before {
		t.Fatalf("calls = %v, want nothing read for a harness's login", calls[before:])
	}
}

// A refused login is the control plane's record, and a pending harness change
// is the box's own report: the change never takes the refusal down, or a box
// could turn "your login was refused" into "click to review and save".
func TestARefusedHarnessLoginStaysUpOverAHarnessChange(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithRejection(t, harnessRejection())
	boxes := m.list.All()
	boxes[0].HarnessConfigPending, boxes[0].HarnessConfigCandidate = true, "cand_1"
	m.list.setAll(boxes)
	if got := m.bannerShowing(); got != console.NoticeRejected {
		t.Fatalf("banner = %v, want the refused login over the harness change", got)
	}
	m.dismissBanner()
	if got := m.bannerShowing(); got != console.NoticeHarnessChanges {
		t.Fatalf("banner = %v, want the harness change once the refusal is dismissed", got)
	}
}

// And for anything else, the card that replaces the value — which is the whole
// of that remedy.
func TestPressingTheRefusedBandOpensTheSecretCard(t *testing.T) {
	t.Parallel()
	m, ds := workspaceWithRejection(t, secretRejection())
	ds.projectSecrets = []console.Secret{{ID: "sec_gh", Name: "GitHub token", Type: "token", Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}}}

	drain(t, m, m.openRejectedRemedy(m.Box), 0)

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
	m.secrets.setAll([]console.Secret{
		{ID: "sec_gh", Name: "GitHub token", Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}},
		{ID: "sec_openai", Name: "OpenAI key", Routes: []secretroute.Route{{Hosts: []string{"api.openai.com"}}}},
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

	if m.bannerShowing() != console.NoticeRejected {
		t.Fatalf("banner = %v, want the refused credential", m.bannerShowing())
	}
	m.setSecretRejections(nil)
	if got := m.bannerShowing(); got != console.NoNotice {
		t.Fatalf("banner = %v, want none once the credential was replaced", got)
	}
}

func harnessRejection() console.SecretRejection {
	//nolint:gosec // G101: an environment variable name, which is what this
	// whole feature is about naming; there is no credential in this package.
	return console.SecretRejection{
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

func secretRejection() console.SecretRejection {
	return console.SecretRejection{
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
func workspaceWithRejection(t *testing.T, rejection console.SecretRejection) (*Model, *fakeSource) {
	t.Helper()
	boxes := testSandboxes()
	ds := newFakeSource(boxes...)
	ds.rejections = []console.SecretRejection{rejection}
	m := newTestModel(t, ds)
	// The band reads the listing's row, which carries the harness as the
	// workspace's does.
	boxes[0].HarnessID = "harness_claude"
	m.list.setAll(boxes)
	m.Box = boxes[0]
	// A tool window is a workspace as far as the band is concerned: the bar is
	// drawn over whatever the screen is showing.
	m.toolShown = &pane{Tool: "diff"}
	m.setSecretRejections(ds.rejections)
	return m, ds
}

// The fix goes where the credential lives. The harnesses and secrets screens act
// on the server the header names; a refused credential's band acts on the
// credential's own server, which need not be the same one — and replacing the
// other server's secret of the same name is the mistake ADR 0131 §2 exists to
// prevent.
func TestTheRefusedBandsRemedyGoesToTheCredentialsServer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		rejection console.SecretRejection
		want      string
	}{
		{"secret", secretRejection(), "Secrets@beta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rejection := tc.rejection
			rejection.Server = "beta"
			m, ds := workspaceWithRejection(t, rejection)
			m.session.Servers = []string{"alpha", "beta"}
			// The header is on the primary; the box and its credential are on
			// beta.
			m.list.Filter.Server = ""
			m.Box.Server = "beta"
			m.setSecretRejections([]console.SecretRejection{rejection})
			ds.secretsOn = map[string][]console.Secret{"beta": {{ID: "sec_gh", Name: "GitHub token", Type: "token"}}}

			drain(t, m, m.openRejectedRemedy(m.Box), 0)

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
	m.setSecretRejections([]console.SecretRejection{rejection})

	onBeta := console.Sandbox{ID: "sbx_b", HarnessID: "harness_claude", Server: "beta"}
	onAlpha := console.Sandbox{ID: "sbx_a", HarnessID: "harness_claude", Server: "alpha"}
	if !m.inbox().Wanted(console.NoticeRejected, onBeta) {
		t.Fatal("a box on the credential's own server was not marked")
	}
	if m.inbox().Wanted(console.NoticeRejected, onAlpha) {
		t.Fatal("a box on another server was marked by a harness ID that only means something on beta")
	}
}
