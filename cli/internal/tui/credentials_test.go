package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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
		Host:          "api.github.com",
		Type:          "bearer",
		Justification: "the task asks me to open a PR",
		Uses:          []string{"Open a pull request against the current repo"},
		Created:       time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
	}
}

func sourceWithRequest(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	ds := newFakeSource(testSandboxes()...)
	ds.requests = []CredentialRequest{waitingRequest()}
	ds.projectSecrets = []Secret{
		{ID: "sec_gh", Name: "GitHub token", Type: "bearer", Host: "api.github.com"},
		{ID: "sec_openai", Name: "OpenAI key", Type: "bearer", Host: "api.openai.com"},
	}
	return newTestModel(t, ds), ds
}

func TestARowWithAWaitingRequestIsMarked(t *testing.T) {
	m, _ := sourceWithRequest(t)

	var marked string
	for _, line := range frame(m) {
		if strings.Contains(line, testSandboxes()[0].Name) {
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
	m, _ := sourceWithRequest(t)

	send(t, m, key("tab"), key(credentialsKey))
	if m.dialog == nil {
		t.Fatal("the key on a marked row opened nothing")
	}
	body := m.dialog.body
	for _, want := range []string{"github", "GH_TOKEN", "api.github.com", "open a PR", "Open a pull request"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dialog body = %q, want it to carry %q", body, want)
		}
	}
	// Every secret can be chosen. Greying one out is how the picker used to
	// leave the only sensible answer unpickable while offering an unrelated
	// one; what a binding costs is now said in the row and asked about on the
	// way through.
	for _, item := range m.dialog.items {
		if strings.HasPrefix(item.key, "secret:") && !item.enabled {
			t.Fatalf("%q cannot be chosen; every secret is an answer somebody may mean", item.label)
		}
	}
	var openai action
	for _, item := range m.dialog.items {
		if item.key == "secret:sec_openai" {
			openai = item
		}
	}
	if !strings.Contains(openai.detail, "api.openai.com") {
		t.Fatalf("detail = %q, want it to say what it is bound to", openai.detail)
	}
}

// The order is the whole of the opinion: the secret for the site being asked
// about comes first, however the inference spelled its host.
func TestTheLikeliestSecretIsOfferedFirst(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	// The shape the server produces: a ghp_ token is bound to the site, and the
	// agent asks for one of its hosts.
	req := waitingRequest()
	req.Host = "api.github.com"
	ds.requests = []CredentialRequest{req}
	ds.projectSecrets = []Secret{
		{ID: "sec_openai", Name: "OpenAI key", Type: "bearer", Host: "api.openai.com"},
		// Bound below what is being asked for: it answers for nothing here,
		// so it sorts last and asks on the way through.
		{ID: "sec_narrow", Name: "uploads only", Type: "bearer", Host: "uploads.github.com"},
		{ID: "sec_loose", Name: "unbound token", Type: "bearer"},
		{ID: "sec_gh", Name: "gh", Type: "bearer", Host: "github.com"},
	}
	m := newTestModel(t, ds)

	send(t, m, key("tab"), key(credentialsKey))
	if m.dialog == nil {
		t.Fatal("no dialog")
	}
	var order []string
	for _, item := range m.dialog.items {
		if strings.HasPrefix(item.key, "secret:") {
			order = append(order, strings.TrimPrefix(item.key, "secret:"))
		}
	}
	// The site's secret covers the host asked for, so it leads; then the
	// unbound one; then the two that answer for something else.
	want := []string{"sec_gh", "sec_loose"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("order = %v, want the covering secret first, then unbound (%v)", order, want)
		}
	}
	for _, id := range order[len(want):] {
		if id != "sec_openai" && id != "sec_narrow" {
			t.Fatalf("order = %v, want the secrets that answer for another host last", order)
		}
	}
}

// Choosing a secret bound to a neighboring host asks about the binding rather
// than failing at the server, and the answer is what the server would suggest.
func TestChoosingANeighbouringHostAsksAboutTheBinding(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	// A binding under the host being asked for: api.github.com does not answer
	// for github.com, which is a different host serving different things.
	req := waitingRequest()
	req.Host = "github.com"
	ds.requests = []CredentialRequest{req}
	ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "gh", Type: "bearer", Host: "api.github.com"}}
	m := newTestModel(t, ds)

	send(t, m, key("tab"), key(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	if m.dialog == nil || m.dialog.kind != dlgConfirm {
		t.Fatal("choosing a secret bound elsewhere did not ask about the binding")
	}
	for _, want := range []string{"api.github.com", "github.com", "gh"} {
		if !strings.Contains(m.dialog.body, want) {
			t.Fatalf("question = %q, want it to name %q", m.dialog.body, want)
		}
	}
	if !m.dialog.defaultNo {
		t.Fatal("the costly answer is the default; widening where a credential may be sent is not an Enter away")
	}
	if len(ds.approvals) != 0 {
		t.Fatal("it approved before asking")
	}

	// The site covers both, so that is what it offers to bind to rather than
	// releasing the binding altogether.
	if !strings.Contains(m.dialog.body, "Bind gh to github.com instead") {
		t.Fatalf("question = %q, want it to offer the binding that covers both", m.dialog.body)
	}
	if !strings.Contains(m.dialog.body, "No leaves the request waiting") {
		t.Fatalf("question = %q, want it to say what No does", m.dialog.body)
	}

	drain(t, m, m.dialog.action("yes"), 0)
	if len(ds.bound) != 1 || ds.bound[0] != "sec_gh=github.com" {
		t.Fatalf("bound = %v, want the secret moved to the host that covers both", ds.bound)
	}
	if len(ds.approvals) != 1 || ds.approvals[0].SecretID != "sec_gh" {
		t.Fatalf("approvals = %#v, want the request answered with it", ds.approvals)
	}
}

// A secret bound to the host being asked for is approved straight away: there
// is nothing to ask about.
func TestAMatchingSecretIsApprovedWithoutAQuestion(t *testing.T) {
	m, ds := sourceWithRequest(t)

	send(t, m, key("tab"), key(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)

	if len(ds.unbound) != 0 {
		t.Fatal("a matching secret was unbound")
	}
	if len(ds.approvals) != 1 {
		t.Fatalf("approvals = %#v, want one, asked nothing further", ds.approvals)
	}
}

func TestApprovingNamesTheRequestAndTheSecret(t *testing.T) {
	m, ds := sourceWithRequest(t)

	send(t, m, key("tab"), key(credentialsKey))
	if m.dialog == nil {
		t.Fatal("no dialog")
	}
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)

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

func TestDenyingIsAnAnswerNotADismissal(t *testing.T) {
	m, ds := sourceWithRequest(t)

	send(t, m, key("tab"), key(credentialsKey))
	drain(t, m, m.dialog.action("deny"), 0)

	if len(ds.denials) != 1 || ds.denials[0] != "sreq_1" {
		t.Fatalf("denials = %#v, want the request answered no", ds.denials)
	}
	if len(ds.approvals) != 0 {
		t.Fatal("denying approved something")
	}
}

// The project may not have the credential yet, which is the common case the
// first time an agent asks for one.
func TestANewCredentialIsStoredThenApproved(t *testing.T) {
	m, ds := sourceWithRequest(t)

	send(t, m, key("tab"), key(credentialsKey))
	drain(t, m, m.dialog.action("new"), 0)
	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatal("choosing a new credential did not ask for one")
	}
	// A credential is not drawn back as it is typed.
	if m.dialog.input.EchoMode == 0 {
		t.Fatal("the token is echoed in the clear")
	}
	drain(t, m, m.dialog.action("ghp_typedbyahuman"), 0)

	if len(ds.createdSecrets) != 1 {
		t.Fatalf("created = %#v, want the typed credential stored once", ds.createdSecrets)
	}
	created := ds.createdSecrets[0]
	if created.Value != "ghp_typedbyahuman" {
		t.Fatalf("stored value = %q", created.Value)
	}
	// It is stored against what was asked for, so the secret it becomes is one
	// the next request can be answered with too.
	if created.Name != "github" || created.Host != "api.github.com" || created.Type != "bearer" {
		t.Fatalf("stored = %#v, want it shaped by the request", created)
	}
	if len(ds.approvals) != 1 || ds.approvals[0].SecretID != "sec_new" {
		t.Fatalf("approvals = %#v, want the request approved with what was just stored", ds.approvals)
	}
}

func TestAnEmptyTokenLeavesTheRequestWaiting(t *testing.T) {
	m, ds := sourceWithRequest(t)

	send(t, m, key("tab"), key(credentialsKey))
	drain(t, m, m.dialog.action("new"), 0)
	drain(t, m, m.dialog.action("   "), 0)

	if len(ds.createdSecrets) != 0 || len(ds.approvals) != 0 {
		t.Fatal("an empty token stored or approved something")
	}
	if len(m.requests["sbx_one"]) != 1 {
		t.Fatal("the request stopped waiting")
	}
}

// The workspace is the one screen that may not be subtle about it.
func TestTheWorkspaceSaysARequestIsWaitingAndWhichKeyAnswersIt(t *testing.T) {
	m, _ := sourceWithRequest(t)
	m.paneBox = Sandbox{ID: "sbx_one", Name: "one"}

	banner := m.viewCredentialBanner(80)
	if banner == "" {
		t.Fatal("the workspace drew nothing for a request waiting on the discobox it is showing")
	}
	if !strings.Contains(banner, "github") {
		t.Fatalf("banner = %q, want it to name what is being asked for", banner)
	}
	if !strings.Contains(banner, credentialsKey) {
		t.Fatalf("banner = %q, want it to name the key that answers", banner)
	}

	// And nothing at all for a discobox with none: the row is only there while
	// somebody is waiting.
	m.paneBox = Sandbox{ID: "sbx_two"}
	if banner := m.viewCredentialBanner(80); banner != "" {
		t.Fatalf("banner = %q on a discobox with no request", banner)
	}
}

func TestTheBannerCountsSeveralRequests(t *testing.T) {
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
	ds := newFakeSource(testSandboxes()...)
	ds.requests = []CredentialRequest{waitingRequest()}
	ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "GitHub token", Type: "bearer", Host: "api.github.com"}}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the banner", func() bool { return m.credentialBannerRows() == 1 })

	// The span is recorded by the draw, so there has to have been one.
	d.wait("the banner drawn", func() bool { return m.credentials.live })
	span := m.credentials
	if span.row != 1 {
		t.Fatalf("banner row = %d, want it under the header", span.row)
	}

	// Far from the words, still on the band.
	x := span.end - 2
	d.dispatch(tea.MouseClickMsg{X: x, Y: span.row, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: x, Y: span.row, Button: tea.MouseLeft})
	// The secrets are read first, so the status dialog gives way to the question.
	d.wait("the question", func() bool { return m.dialog != nil && m.dialog.kind == dlgActions })
	if !strings.Contains(m.dialog.body, "api.github.com") {
		t.Fatalf("dialog = %q, want the request the banner was about", m.dialog.body)
	}
	// The press belongs to the banner: it must not also have started a
	// selection drag across the chrome.
	if m.chromeCapture {
		t.Fatal("the click also started a chrome selection")
	}
}

// The banner takes a row from the panes rather than adding one to the frame,
// so everything the mouse aims at below it moves down with it. This is the
// regression that would otherwise show up as clicks landing one row off.
func TestTheBannerMovesTheChromeItPushesDown(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })

	rowsBefore, tabRowBefore := m.paneRows(), 1
	if _, _, ok := m.tabAt(m.width/2+4, tabRowBefore); !ok {
		t.Fatal("no tab where the strip is drawn with no banner")
	}

	ds.mu.Lock()
	ds.requests = []CredentialRequest{waitingRequest()}
	ds.mu.Unlock()
	d.dispatch(tickMsg{})
	d.wait("the banner", func() bool { return m.credentialBannerRows() == 1 })

	if m.paneRows() != rowsBefore-1 {
		t.Fatalf("paneRows = %d, want one fewer than %d: the banner is a row taken from the panes", m.paneRows(), rowsBefore)
	}
	if _, _, ok := m.tabAt(m.width/2+4, tabRowBefore); ok {
		t.Fatal("the tab strip still answers on the row the banner now occupies")
	}
	if _, _, ok := m.tabAt(m.width/2+4, tabRowBefore+1); !ok {
		t.Fatal("the tab strip does not answer on the row it moved to")
	}
}

// A refusal from the server is an instruction — "bound to api.github.com and
// cannot be granted for github.com; pick a secret for github.com, or clear the
// secret's host" — so it stays on screen. A status line that clears itself
// after four seconds, on a screen the request has just left, reads as nothing
// having happened, which is how a refused approval looks like a system that
// silently refuses to grant one secret twice.
func TestAFailedApprovalIsShownAndSaysWhatToDo(t *testing.T) {
	m, ds := sourceWithRequest(t)
	ds.mu.Lock()
	ds.approveErr = errTestRefused
	ds.mu.Unlock()

	send(t, m, key("tab"), key(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)

	if m.dialog == nil {
		t.Fatal("the failure closed the dialog and left nothing on screen")
	}
	if !m.dialog.err {
		t.Fatal("the dialog is not drawn as a failure")
	}
	if !strings.Contains(m.dialog.body, "clear the secret's host") {
		t.Fatalf("body = %q, want the server's own remedy kept intact", m.dialog.body)
	}
	// And it says which request is still waiting, since the list behind it no
	// longer has the answer on screen.
	if !strings.Contains(m.dialog.body, "github") || !strings.Contains(m.dialog.body, "still waiting") {
		t.Fatalf("body = %q, want the request it was about", m.dialog.body)
	}
	if len(m.requests["sbx_one"]) != 1 {
		t.Fatal("the request stopped waiting after a failed approval")
	}
}

// Saying no goes back to the question. A dialog that closes onto the list,
// having done nothing and said nothing, is how a deliberate "not that one"
// reads as the window ignoring the keypress — which is what a refused
// approval looked like.
func TestDecliningToRebindReturnsToTheQuestion(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	req := waitingRequest()
	req.Host = "github.com"
	ds.requests = []CredentialRequest{req}
	ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "gh", Type: "token", Host: "api.github.com"}}
	m := newTestModel(t, ds)

	send(t, m, key("tab"), key(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	if m.dialog == nil || m.dialog.kind != dlgConfirm {
		t.Fatal("choosing a secret bound elsewhere did not ask")
	}

	// Esc is the same answer as no, and it is the one a hurried reader gives.
	drain(t, m, m.dialog.onCancel(), 0)
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %#v, want the picker back", m.dialog)
	}
	if !strings.Contains(m.dialog.body, "github.com") {
		t.Fatalf("body = %q, want the request still in front of you", m.dialog.body)
	}
	if len(ds.bound) != 0 || len(ds.approvals) != 0 {
		t.Fatal("declining changed something")
	}
}
