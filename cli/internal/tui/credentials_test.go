package tui

import (
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/pkg/bindingplan"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// The credential inbox is an interruption budget: an agent can ask at any
// moment, so the list may only mark a row, and the workspace — where you are
// already looking at that one discobox — has to be impossible to look past.

// errTestRefused is the shape of refusal the server actually sends: what is
// wrong, and what to do about it.
var errTestRefused = errors.New("secret sec_gh is bound to api.github.com and cannot be granted for github.com; pick a secret for github.com, or clear the secret's host if this credential is used against both")

func waitingRequest() CredentialRequest {
	return CredentialRequest{
		ID:            "sreq_1",
		SandboxID:     "sbx_one",
		Name:          "github",
		EnvVar:        "GH_TOKEN",
		Use:           &RequestPart{Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}},
		Justification: "the task asks me to open a PR",
		Created:       time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
	}
}

func sourceWithRequest(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	ds := newFakeSource(testSandboxes()...)
	ds.requests = []CredentialRequest{waitingRequest()}
	ds.projectSecrets = []Secret{
		{ID: "sec_gh", Name: "GitHub token", Type: "bearer", Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}},
		{ID: "sec_openai", Name: "OpenAI key", Type: "bearer", Routes: []secretroute.Route{{Hosts: []string{"api.openai.com"}}}},
	}
	return newTestModel(t, ds), ds
}

func TestARowWithAWaitingRequestIsMarked(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithRequest(t)

	var marked string
	for _, line := range frame(m) {
		// A prefix of the name: the row ellipsizes it at this width.
		if strings.Contains(line, "fix flaky pool") {
			marked = line
		}
	}
	if marked == "" {
		t.Fatal("the discobox with the request is not on screen")
	}
	if !strings.Contains(marked, "!") {
		t.Fatalf("row = %q, want the mark that says a person is being waited on", marked)
	}
	// Every other row is unmarked: the mark is a fact about one discobox.
	others := 0
	for _, line := range frame(m) {
		if line != marked && strings.Contains(line, "!") {
			others++
		}
	}
	if others > 0 {
		t.Fatalf("%d other rows carry the mark", others)
	}
}

// The dialog is opened deliberately, from the row. Nothing about a request
// arriving takes the screen: the poll that finds one lands while a sentence is
// being typed into the prompt.
func TestAWaitingRequestNeverTakesTheScreen(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithRequest(t)

	send(t, m, tickMsg{})
	if m.dialog != nil {
		t.Fatal("a poll that found a request opened a dialog; it may only mark the row")
	}
	if m.focus != focusPrompt {
		t.Fatal("focus moved off the prompt")
	}
}

func TestTheRowKeyAsksWhatWasRequested(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithRequest(t)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	if !onRequestCard(m) {
		t.Fatalf("dialog = %s, want the request card", describe(m.dialog))
	}
	body := dialogText(m)
	for _, want := range []string{"github", "GH_TOKEN", "open a PR"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dialog body = %q, want it to carry %q", body, want)
		}
	}
	// Every secret is listed and can be chosen: one that cannot answer is
	// marked, and choosing it says why rather than leaving it unpickable.
	var keys []string
	for _, item := range m.dialog.items {
		if !item.enabled {
			t.Fatalf("%q cannot be chosen", item.label)
		}
		keys = append(keys, item.key)
	}
	if strings.Join(keys, ",") != "sec_gh,sec_openai,new,ttl" {
		t.Fatalf("rows = %v, want every secret and a new credential", keys)
	}
}

// An ask to delegate reads like an ask to use in every field but one, so the
// card says it before anything else about the credential.
func TestAnAskToDelegateSaysWhatItIs(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	req := waitingRequest()
	req.Delegate, req.Use = &RequestPart{Routes: req.routes()}, nil
	ds.requests = []CredentialRequest{req}
	ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "GitHub token", Routes: req.routes()}}
	m := newTestModel(t, ds)

	toSummary(t, m, "sec_gh")
	text := plainFrame(m)
	if !strings.Contains(text, "delegated to its workers") || strings.Contains(text, "used by this box") {
		t.Fatalf("summary = %q, want the delegation alone", text)
	}
	answerApproval(t, m, "review")
	if !strings.Contains(m.dialog.body, "could not use it itself") {
		t.Fatalf("card = %q, want it to say the box cannot use it", plainFrame(m))
	}
	if !onStep(m, "its workers") {
		t.Fatalf("dialog = %s, want the delegate part with no use part before it", describe(m.dialog))
	}
}

// The order is the whole of the opinion: the secret named what the request
// asks for comes first, and every secret is offered.
func TestTheLikeliestSecretIsOfferedFirst(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	req := waitingRequest()
	ds.requests = []CredentialRequest{req}
	ds.projectSecrets = []Secret{
		{ID: "sec_openai", Name: "OpenAI key", Type: "bearer", Routes: []secretroute.Route{{Hosts: []string{"api.openai.com"}}}},
		{ID: "sec_loose", Name: "anywhere token", Type: "bearer", Routes: []secretroute.Route{{Hosts: []string{"*"}}}},
		{ID: "sec_gh", Name: "github", Type: "bearer", Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}},
	}
	m := newTestModel(t, ds)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	var order []string
	for _, item := range m.dialog.items {
		order = append(order, item.key)
	}
	if strings.Join(order, ",") != "sec_gh,sec_loose,sec_openai,new,ttl" {
		t.Fatalf("order = %v, want those that can answer first, the one named github first among them", order)
	}
}

// A secret bound to the host being asked for is approved from the summary,
// using the lifetime selected with the credential.
func TestAMatchingSecretIsApprovedWithoutAQuestion(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)

	toSummary(t, m, "sec_gh")
	grantFor(t, m, time.Hour)

	if len(ds.approvals) != 1 {
		t.Fatalf("approvals = %#v, want one, asked nothing further", ds.approvals)
	}
}

func TestApprovingNamesTheRequestAndTheSecret(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)

	toSummary(t, m, "sec_gh")
	grantFor(t, m, time.Hour)

	if len(ds.approvals) != 1 {
		t.Fatalf("approvals = %#v, want one", ds.approvals)
	}
	if ds.approvals[0].RequestID != "sreq_1" || ds.approvals[0].SecretID != "sec_gh" {
		t.Fatalf("approval = %#v, want the request answered with the chosen secret", ds.approvals[0])
	}
	if m.dialog != nil {
		t.Fatal("the dialog stayed up after it was answered")
	}
	// The mark goes with the request: the inbox is re-read rather than
	// guessed at, so the row follows the server.
	if len(m.requests["sbx_one"]) != 0 {
		t.Fatalf("requests = %#v, want the answered one gone", m.requests)
	}
}

// Lifetime is selected with the credential and is retained through consent.

// Forever is offered, and sends the zero that means it never expires.
func TestForeverIsAnAnswerThatSaysSo(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)

	toSummary(t, m, "sec_gh")
	grantFor(t, m, lifetime.Forever)
	if len(ds.approvals) != 1 || ds.approvals[0].TTLSeconds != 0 {
		t.Fatalf("approvals = %#v, want the zero that means it never expires", ds.approvals)
	}
}

// The report is the only place the lifetime is said after the dialogs are gone.
func TestTheApprovalSaysHowLongItGrantedFor(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithRequest(t)

	toSummary(t, m, "sec_gh")
	grantFor(t, m, lifetime.Day)

	if !strings.Contains(strings.Join(frame(m), "\n"), "approved github for 1 day") {
		t.Fatalf("the window did not say what it granted:\n%s", strings.Join(frame(m), "\n"))
	}
}

func TestDenyingIsAnAnswerNotADismissal(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)

	toSummary(t, m, "sec_gh")
	answerApproval(t, m, "deny")

	if len(ds.denials) != 1 || ds.denials[0] != "sreq_1" {
		t.Fatalf("denials = %#v, want the request answered no", ds.denials)
	}
	if len(ds.approvals) != 0 {
		t.Fatal("denying approved something")
	}
}

// The workspace is the one screen that may not be subtle about it.
func TestTheWorkspaceSaysARequestIsWaitingAndWhichKeyAnswersIt(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithRequest(t)
	m.paneBox = Sandbox{ID: "sbx_one", Name: "one"}

	banner := m.viewCredentialBanner(80)
	if banner == "" {
		t.Fatal("the workspace drew nothing for a request waiting on the discobox it is showing")
	}
	if !strings.Contains(banner, "github") {
		t.Fatalf("banner = %q, want it to name what is being asked for", banner)
	}
	// The key it names is the leader's, which is not the list's letter: see
	// credentialsLeaderKey.
	if !strings.Contains(banner, m.leader()+" "+credentialsLeaderKey) {
		t.Fatalf("banner = %q, want it to name the key that answers", banner)
	}
	if !strings.Contains(banner, "click") {
		t.Fatalf("banner = %q, want it to say it can be clicked", banner)
	}

	// And nothing at all for a discobox with none: the row is only there while
	// somebody is waiting.
	m.paneBox = Sandbox{ID: "sbx_two"}
	if banner := m.viewCredentialBanner(80); banner != "" {
		t.Fatalf("banner = %q on a discobox with no request", banner)
	}
	// The hit test goes with it. A span left behind by a bar that is no longer
	// drawn is a row of the header that silently answers for a request nobody
	// is waiting on.
	if m.banner.live {
		t.Fatal("the banner's hit test outlived the banner")
	}
}

// The key that answers is not one keystroke from the key that opens a terminal.
// The leader's c is another terminal, and a shift away from it sat the dialog
// that hands out a credential — two commands one modifier apart, one of them
// consequential and reached for in a hurry.
func TestTheLeaderAnswersOnItsOwnKey(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		if credentialsLeaderKey == paneTerminalKey || strings.EqualFold(credentialsLeaderKey, paneTerminalKey) {
			t.Fatalf("the credential key (%q) is a shift away from the terminal key (%q)",
				credentialsLeaderKey, paneTerminalKey)
		}
		ds := newFakeSource(testSandboxes()...)
		ds.requests = []CredentialRequest{waitingRequest()}
		ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "GitHub token", Type: "bearer", Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}}}
		d, m, _ := openWorkspace(t, ds, "enter")
		d.wait("the banner", func() bool { return m.bannerTop() == 1 })

		d.key("ctrl+a")
		d.key(credentialsLeaderKey)
		d.wait("the question", func() bool { return onRequestCard(m) })
		if !strings.Contains(dialogText(m), "the task asks me to open a PR") {
			t.Fatalf("dialog = %q, want the request the banner is about", dialogText(m))
		}
	})
}

func TestTheBannerCountsSeveralRequests(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithRequest(t)
	second := waitingRequest()
	second.ID, second.Name = "sreq_2", "npm"
	m.setCredentialRequests([]CredentialRequest{waitingRequest(), second})
	m.paneBox = Sandbox{ID: "sbx_one"}

	banner := m.viewCredentialBanner(80)
	if !strings.Contains(banner, "2 credential requests") {
		t.Fatalf("banner = %q, want the count", banner)
	}
}

// A request nobody is looking at is still a request: answered from elsewhere,
// the window notices on the poll rather than holding a stale mark.
func TestAnAnsweredRequestClearsOnTheNextPoll(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	if len(m.requests["sbx_one"]) != 1 {
		t.Fatal("the request never arrived")
	}

	ds.mu.Lock()
	ds.requests = nil
	ds.mu.Unlock()
	send(t, m, tickMsg{})

	if len(m.requests["sbx_one"]) != 0 {
		t.Fatal("the mark outlived the request")
	}
}

// The banner is a button. A bar that says a person is being waited on, on a
// screen driven by a mouse as much as a keyboard, has to answer to the obvious
// gesture — and the whole band is the target, not the words on it.
func TestClickingTheBannerOpensTheQuestion(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		ds.requests = []CredentialRequest{waitingRequest()}
		ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "GitHub token", Type: "bearer", Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}}}
		d, m, _ := openWorkspace(t, ds, "enter")
		d.wait("the banner", func() bool { return m.bannerTop() == 1 })

		// The span is recorded by the draw, so there has to have been one, and it
		// covers both bands.
		d.wait("the banner drawn", func() bool { return m.banner.live })
		span := m.banner
		if len(span.rows) != 2 || span.rows[0] != 1 {
			t.Fatalf("banner rows = %v, want one under the header and one above the keys", span.rows)
		}
		if span.rows[1] != len(frame(m))-2 {
			t.Fatalf("the lower band is on row %d of a %d-row frame, want it just above the keys row",
				span.rows[1], len(frame(m)))
		}

		// Far from the words, still on the band — its left end, since its right
		// end is the dismiss button — and the lower one answers as the upper one
		// does.
		x := span.start + 1
		for _, row := range span.rows {
			if !m.bannerAt(x, row) {
				t.Fatalf("row %d does not answer as the band", row)
			}
		}
		clickWindow(d, x, span.rows[1])
		// The secrets are read first, so the status dialog gives way to the question.
		d.wait("the question", func() bool { return onRequestCard(m) })
		if !strings.Contains(dialogText(m), "the task asks me to open a PR") {
			t.Fatalf("dialog = %q, want the request the banner was about", dialogText(m))
		}
		// The press belongs to the banner: it must not also have started a
		// selection drag across the chrome.
		if m.chromeCapture {
			t.Fatal("the click also started a chrome selection")
		}
	})
}

// The band takes its rows from the panes rather than adding them to the frame,
// so everything the mouse aims at moves with it and the window stays the size
// the terminal is. This is the regression that would otherwise show up as
// clicks — and the hardware cursor — landing a row off.
//
// The two counts are not the same number: the boxes lose two rows, one at each
// end, and start one row lower. Answering both questions with one number is
// what puts a terminal's cursor somewhere other than the cell it is drawn in.
func TestTheBandMovesTheChromeItPushesDown(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, _ := openWorkspace(t, ds, "enter")
		d.key("ctrl+a")
		d.key("s")
		d.wait("the tab", func() bool { return m.shells.len() == 1 })

		rowsBefore, tabRowBefore := m.paneRows(), 1
		if _, _, ok := m.tabAt(m.width/2+4, tabRowBefore); !ok {
			t.Fatal("no tab where the strip is drawn with no banner")
		}
		_, originBefore := m.paneOrigin(m.terminals.visible())
		if got := len(frame(m)); got != m.height {
			t.Fatalf("frame = %d rows with no band, want the window's %d", got, m.height)
		}

		ds.mu.Lock()
		ds.requests = []CredentialRequest{waitingRequest()}
		ds.mu.Unlock()
		d.dispatch(tickMsg{})
		d.wait("the banner", func() bool { return m.bannerTop() == 1 })

		// The frame is still exactly the window: two rows of band, two rows off
		// the panes.
		if got := len(frame(m)); got != m.height {
			t.Fatalf("frame = %d rows with the band up, want the window's %d", got, m.height)
		}
		if m.paneRows() != rowsBefore-2 {
			t.Fatalf("paneRows = %d, want two fewer than %d: a row of band at each end", m.paneRows(), rowsBefore)
		}
		// The grid — and so the cursor drawn in it — moves down by the top band
		// alone. The band below it takes height, not position.
		if _, origin := m.paneOrigin(m.terminals.visible()); origin != originBefore+1 {
			t.Fatalf("pane origin = %d, want one below %d: only the top band moves the grid", origin, originBefore)
		}
		if _, _, ok := m.tabAt(m.width/2+4, tabRowBefore); ok {
			t.Fatal("the tab strip still answers on the row the banner now occupies")
		}
		if _, _, ok := m.tabAt(m.width/2+4, tabRowBefore+1); !ok {
			t.Fatal("the tab strip does not answer on the row it moved to")
		}
	})
}

// A refusal from the server is an instruction — "bound to api.github.com and
// cannot be granted for github.com; pick a secret for github.com, or clear the
// secret's host" — so it stays on screen. A status line that clears itself
// after four seconds, on a screen the request has just left, reads as nothing
// having happened, which is how a refused approval looks like a system that
// silently refuses to grant one secret twice.
func TestAFailedApprovalIsShownAndSaysWhatToDo(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	ds.mu.Lock()
	ds.approveErr = errTestRefused
	ds.mu.Unlock()

	toSummary(t, m, "sec_gh")
	grantFor(t, m, time.Hour)

	if m.dialog == nil {
		t.Fatal("the failure closed the dialog and left nothing on screen")
	}
	if !m.dialog.err {
		t.Fatal("the dialog is not drawn as a failure")
	}
	if !strings.Contains(dialogText(m), "clear the secret's host") {
		t.Fatalf("body = %q, want the server's own remedy kept intact", dialogText(m))
	}
	// And it says which request is still waiting, since the list behind it no
	// longer has the answer on screen.
	if !strings.Contains(dialogText(m), "github") || !strings.Contains(dialogText(m), "still waiting") {
		t.Fatalf("body = %q, want the request it was about", dialogText(m))
	}
	if len(m.requests["sbx_one"]) != 1 {
		t.Fatal("the request stopped waiting after a failed approval")
	}
}

// A failure that changed nothing says nothing about changes.
func TestAFailedApprovalThatChangedNothingSaysNothingOfChanges(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	ds.mu.Lock()
	ds.approveErr = errTestRefused
	ds.mu.Unlock()

	toSummary(t, m, "sec_gh")
	grantFor(t, m, time.Hour)

	if strings.Contains(dialogText(m), "already done") {
		t.Fatalf("failure = %q, want no changes claimed when there were none", dialogText(m))
	}
}

// A request card longer than the window still fits it, with its answers on
// screen: a card that runs off the bottom hides the secrets and Deny.
func TestALongRequestCardKeepsItsAnswersOnScreen(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)
	req := waitingRequest()
	req.AskedTTL = 72 * time.Hour
	req.Justification = strings.Repeat("the user asked me to have worker discoboxes keep four pull requests reviewed, green and rebased ", 40)
	ds.mu.Lock()
	ds.requests = []CredentialRequest{req}
	ds.mu.Unlock()
	send(t, m, tickMsg{})
	send(t, m, keyPress(credentialsKey))
	if m.dialog == nil {
		t.Fatal("the request card did not open")
	}

	lines := frame(m)
	if len(lines) > m.height {
		t.Errorf("the card is %d rows in a window of %d", len(lines), m.height)
	}
	onScreen := strings.Join(lines[:min(len(lines), m.height)], "\n")
	for _, answer := range []string{"Enter new credential…"} {
		if !strings.Contains(onScreen, answer) {
			t.Errorf("%s is not on screen", answer)
		}
	}
}

// The wheel reads a long request card to its end, with the answers still under
// it.
func TestTheWheelScrollsALongRequestCard(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)
	req := waitingRequest()
	// One word, so where the card happens to wrap cannot split it.
	const last = "and-that-is-the-end-of-why"
	req.Justification = strings.Repeat("the user asked me to keep four pull requests reviewed, green and rebased; ", 40) + last
	ds.mu.Lock()
	ds.requests = []CredentialRequest{req}
	ds.mu.Unlock()
	send(t, m, tickMsg{})
	send(t, m, keyPress(credentialsKey))

	if strings.Contains(frameText(m), last) {
		t.Fatalf("the card fits, so there is nothing to scroll:\n%s", frameText(m))
	}
	_, y := at(t, m, "the user asked me")
	seenLast := false
	for range 100 {
		seenLast = seenLast || strings.Contains(frameText(m), last)
		send(t, m, toScreen(m, tea.MouseWheelMsg{X: m.width / 2, Y: y, Button: tea.MouseWheelDown}))
	}
	text := frameText(m)
	if !seenLast {
		t.Errorf("the wheel did not reach the end of the card:\n%s", text)
	}
	if !strings.Contains(text, "Enter new credential…") {
		t.Errorf("the answers left the card as it scrolled:\n%s", text)
	}
}

// r on a request card shows the request as the server sent it, as JSON, and
// Esc goes back to the card.

// A secret that allows raw asks, after the lifetime, whether the discobox is
// handed a placeholder or the value itself — the person's choice, since no
// request asks (ADR 26-10-27-840 §2). The placeholder is where it opens, and
// a secret that does not allow raw asks nothing more.

// A request that asks both to use a credential and to delegate it is
// answered part by part, on one card each before anything is sent: no row
// approves one part and leaves the other unsaid, the card says what an
// approved part replaces, and the answer goes as one call with both
// decisions (ADR 26-11-02-022 §§3–5).

// A request asking to add, to a discobox already holding the variable, is
// asked about with both outcomes on one card: what applies there now, grant
// by grant with its lifetime, the routes asked that none of them names, and
// what replacing would cut short (ADR 26-11-03-329 §5). The way it asked is
// first; granted so, it is an addition, by sentinel even of a secret that
// allows its value to be handed over.

// A request with one part is approved by choosing a secret, as before, and
// says only that part.

// replacedRows is what replacedFields says, a row to a line, its label
// before a bar so where the field starts shows.
func replacedRows(fields []field) string {
	var rows []string
	for _, f := range fields {
		rows = append(rows, f.label+"|"+f.value)
	}
	return strings.Join(rows, "\n")
}

// The card says, of each binding the asking discobox holds of the secret,
// what approving a part does to it — kept, gone, losing its use, split — by
// the rule the server applies, and when a new one is made (ADR 26-11-02-022
// §5).
func TestTheCardSaysWhatEachBindingBecomes(t *testing.T) {
	t.Parallel()
	at := func(m int) time.Time { return time.Date(2026, 11, 2, 0, m, 0, 0, time.UTC) }
	standing := []SecretBinding{
		{ID: "old", Use: true, EnvVar: "GITHUB_TOKEN", Created: at(0)},
		{ID: "asked", Use: true, EnvVar: "GH_TOKEN", Created: at(1)},
		{ID: "both", Use: true, Delegate: true, EnvVar: "GH_LEAD", Created: at(2)},
	}
	use := replacedRows(replacedFields(standing, false, "GH_TOKEN", bindingplan.Replace, nil, nil))
	for _, want := range []string{
		"the one in GH_TOKEN, sent to the secret's routes — kept, takes this approval",
		"the one in GITHUB_TOKEN, sent to the secret's routes — goes",
		"the one in GH_LEAD, sent to the secret's routes — loses its use; its delegation stays",
	} {
		if !strings.Contains(use, want) {
			t.Errorf("use part:\n%s\nwant %q", use, want)
		}
	}
	delegate := replacedRows(replacedFields(standing, true, "GH_TOKEN", "", nil, nil))
	if !strings.Contains(delegate, "stays the delegation; its use moves to a binding of its own in GH_LEAD") || strings.Contains(delegate, "GITHUB_TOKEN") {
		t.Errorf("delegate part:\n%s", delegate)
	}
	if got := replacedRows(replacedFields(standing[2:], false, "GH_TOKEN", bindingplan.Replace, nil, nil)); !strings.Contains(got, "and a new binding is made") {
		t.Errorf("a use part with nothing to keep:\n%s", got)
	}
	// Both parts approved over the binding that uses and delegates: the use
	// part takes its use, and the delegate part then updates the delegation
	// left — no split, no second new use binding.
	both := standing[2:]
	if got := replacedRows(replacedFields(both, true, "GH_TOKEN", bindingplan.Replace, nil, nil)); !strings.Contains(got, "the delegation, sent to the secret's routes — kept, takes this approval") ||
		strings.Contains(got, "moves") || strings.Contains(got, "new binding") {
		t.Errorf("delegate part after the use part:\n%s", got)
	}
}

// A binding is set against the routes the part asks for as a diff: what it
// loses, and — on the binding kept, whatever its fate — what it gains, the
// routes both name counted rather than listed again. A binding that goes says
// how many of its routes the approval carries on, never that they are the
// same, and a side with no routes of its own is the secret's.
func TestTheCardDiffsAReplacedBindingsRoutes(t *testing.T) {
	t.Parallel()
	at := func(m int) time.Time { return time.Date(2026, 11, 2, 0, m, 0, 0, time.UTC) }
	route := func(path string) secretroute.Route {
		return secretroute.Route{Hosts: []string{"api.github.com"}, Paths: []string{path}}
	}
	routes := func(paths ...string) []secretroute.Route {
		var out []secretroute.Route
		for _, p := range paths {
			out = append(out, route(p))
		}
		return out
	}
	r := func(path string) string { return secretroute.Format(route(path)) }
	for _, tc := range []struct {
		name     string
		standing []SecretBinding
		delegate bool
		asked    []secretroute.Route
		secret   []secretroute.Route
		want     []string
	}{{
		name: "the kept one loses and gains, the one that goes only loses",
		standing: []SecretBinding{
			{ID: "old", Use: true, EnvVar: "GITHUB_TOKEN", Created: at(0), Routes: routes("/a", "/gone")},
			{ID: "asked", Use: true, EnvVar: "GH_TOKEN", Created: at(1), Routes: routes("/a", "/b", "/dropped")},
		},
		asked: routes("/a", "/b", "/new"),
		want: []string{
			"replaces|the one in GITHUB_TOKEN — goes",
			"|  1 route also in this approval",
			"|- " + r("/gone"),
			"|the one in GH_TOKEN — kept, takes this approval",
			"|  2 routes unchanged",
			"|- " + r("/dropped"),
			"|+ " + r("/new"),
		},
	}, {
		// The split binding stays the delegation and takes the approval's
		// routes in place, so a widening is said on it.
		name:     "a split delegation gains",
		standing: []SecretBinding{{ID: "both", Use: true, Delegate: true, EnvVar: "GH_TOKEN", Created: at(0), Routes: routes("/a")}},
		delegate: true,
		asked:    routes("/a", "/wider"),
		want: []string{
			"replaces|the one in GH_TOKEN — stays the delegation; its use moves to a binding of its own in GH_TOKEN",
			"|  1 route unchanged",
			"|+ " + r("/wider"),
		},
	}, {
		name: "one that goes, all of it carried on, is not the same",
		standing: []SecretBinding{
			{ID: "old", Use: true, EnvVar: "GITHUB_TOKEN", Created: at(0), Routes: routes("/a")},
			{ID: "asked", Use: true, EnvVar: "GH_TOKEN", Created: at(1), Routes: routes("/a", "/b")},
		},
		asked: routes("/a", "/b"),
		want: []string{
			"replaces|the one in GITHUB_TOKEN — goes",
			"|  every route also in this approval",
			"|the one in GH_TOKEN — kept, takes this approval",
			"|  the same routes",
		},
	}, {
		name:     "a part naming no routes is set against the secret's",
		standing: []SecretBinding{{ID: "asked", Use: true, EnvVar: "GH_TOKEN", Created: at(0), Routes: routes("/a", "/narrow")}},
		secret:   routes("/a", "/wide"),
		want: []string{
			"replaces|the one in GH_TOKEN — kept, takes this approval",
			"|  1 route unchanged",
			"|- " + r("/narrow"),
			"|+ " + r("/wide"),
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			useAs := bindingplan.Replace
			if tc.delegate {
				useAs = ""
			}
			fields := replacedFields(tc.standing, tc.delegate, "GH_TOKEN", useAs, tc.asked, tc.secret)
			if got, want := replacedRows(fields), strings.Join(tc.want, "\n"); got != want {
				t.Errorf("fields:\n%s\nwant:\n%s", got, want)
			}
			for _, f := range fields {
				want := toneAlert
				switch {
				case strings.HasPrefix(f.value, "+ "):
					want = toneOK
				case strings.HasPrefix(f.value, "  "):
					want = toneDim
				}
				if f.tone != want {
					t.Errorf("%q has tone %v, want %v", f.value, f.tone, want)
				}
			}
		})
	}
}
