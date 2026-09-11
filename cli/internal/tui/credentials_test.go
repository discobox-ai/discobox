package tui

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"

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

// grantFor answers the lifetime step — the second of every approval — with one
// of its presets.
func grantFor(t *testing.T, m *Model, d time.Duration) {
	t.Helper()
	if !onLifetimeStep(m) {
		t.Fatalf("dialog = %s, want the step asking how long", describe(m.dialog))
	}
	drain(t, m, m.dialog.action(strconv.FormatInt(lifetime.Seconds(d), 10)), 0)
}

func onLifetimeStep(m *Model) bool {
	return m.dialog != nil && m.dialog.kind == dlgActions && m.dialog.title == "How long?"
}

func onRequestCard(m *Model) bool {
	return m.dialog != nil && m.dialog.kind == dlgActions && m.dialog.title == "Credential request"
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
	if m.dialog == nil {
		t.Fatal("the key on a marked row opened nothing")
	}
	body := dialogText(m)
	for _, want := range []string{"github", "GH_TOKEN", "api.github.com", "open a PR", "Open a pull request"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dialog body = %q, want it to carry %q", body, want)
		}
	}
	// Every secret can be chosen. Greying one out can leave the only sensible
	// answer unpickable while offering an unrelated one; what a binding costs
	// is said in the row and asked about on the way through.
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
	t.Parallel()
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

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	// A binding under the host being asked for: api.github.com does not answer
	// for github.com, which is a different host serving different things.
	req := waitingRequest()
	req.Host = "github.com"
	ds.requests = []CredentialRequest{req}
	ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "gh", Type: "bearer", Host: "api.github.com"}}
	m := newTestModel(t, ds)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	if m.dialog == nil || m.dialog.kind != dlgConfirm {
		t.Fatal("choosing a secret bound elsewhere did not ask about the binding")
	}
	for _, want := range []string{"api.github.com", "github.com", "gh"} {
		if !strings.Contains(dialogText(m), want) {
			t.Fatalf("question = %q, want it to name %q", dialogText(m), want)
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
	if !strings.Contains(dialogText(m), "bind gh to github.com instead") {
		t.Fatalf("question = %q, want it to offer the binding that covers both", dialogText(m))
	}
	if !strings.Contains(dialogText(m), "no goes back to the request") {
		t.Fatalf("question = %q, want it to say what No does", dialogText(m))
	}

	// Yes is agreed to, not yet done: the lifetime is still to be chosen, and
	// going back from it must find the secret as it was.
	drain(t, m, m.dialog.action("yes"), 0)
	if len(ds.bound) != 0 {
		t.Fatal("the binding moved before the approval was finished")
	}
	grantFor(t, m, time.Hour)
	if len(ds.bound) != 1 || ds.bound[0] != "sec_gh=github.com" {
		t.Fatalf("bound = %v, want the secret moved to the host that covers both", ds.bound)
	}
	if len(ds.approvals) != 1 || ds.approvals[0].SecretID != "sec_gh" {
		t.Fatalf("approvals = %#v, want the request answered with it", ds.approvals)
	}
}

// A secret bound to the host being asked for goes straight on to the lifetime:
// there is nothing about the binding to ask.
func TestAMatchingSecretIsApprovedWithoutAQuestion(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	grantFor(t, m, time.Hour)

	if len(ds.unbound) != 0 {
		t.Fatal("a matching secret was unbound")
	}
	if len(ds.approvals) != 1 {
		t.Fatalf("approvals = %#v, want one, asked nothing further", ds.approvals)
	}
}

func TestApprovingNamesTheRequestAndTheSecret(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	if m.dialog == nil {
		t.Fatal("no dialog")
	}
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
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

// How long a credential is handed out for is the half of an approval nobody
// thinks to look for — until it was asked, the window minted whatever the
// credential's own ceiling happened to be, which for most credentials was
// forever. So it is a step of its own, after the secret, and it has to be
// answered.
func TestChoosingASecretAsksHowLongNext(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	// The request card asks one thing: which secret.
	if strings.Contains(dialogText(m), "granted for") || strings.Contains(dialogText(m), "1 hour") {
		t.Fatalf("card = %q, want no lifetime on it; that is the next step", dialogText(m))
	}
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)

	if !onLifetimeStep(m) {
		t.Fatalf("dialog = %s, want the step asking how long", describe(m.dialog))
	}
	if len(ds.approvals) != 0 {
		t.Fatal("it approved before asking how long")
	}
	var labels []string
	for _, item := range m.dialog.items {
		labels = append(labels, item.label)
	}
	if want := []string{"1 hour", "1 day", "1 week", "1 month", "forever", "custom…"}; strings.Join(labels, ",") != strings.Join(want, ",") {
		t.Fatalf("offered = %v, want %v", labels, want)
	}
	// It says what the lifetime is for: the credential, what answers it, and
	// where it may go.
	for _, want := range []string{"github", "GitHub token", "api.github.com"} {
		if !strings.Contains(dialogText(m), want) {
			t.Fatalf("step = %q, want it to name %q", dialogText(m), want)
		}
	}

	// It opens on an hour, so Enter is the default answer.
	send(t, m, keyPress("enter"))
	if len(ds.approvals) != 1 || ds.approvals[0].TTLSeconds != 3600 {
		t.Fatalf("approvals = %#v, want the hour it opened on", ds.approvals)
	}
}

// Forever is offered, and says what it means rather than leaving the word to
// do it alone.
func TestForeverIsAnAnswerThatSaysSo(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	for _, item := range m.dialog.items {
		if item.label == "forever" && !strings.Contains(item.detail, "never expires") {
			t.Fatalf("forever = %q, want it to say it never expires", item.detail)
		}
	}
	grantFor(t, m, lifetime.Forever)
	if len(ds.approvals) != 1 || ds.approvals[0].TTLSeconds != 0 {
		t.Fatalf("approvals = %#v, want the zero that means it never expires", ds.approvals)
	}
}

// Esc on the lifetime is "not that secret", not "never mind": it goes back to
// the request, which is still waiting.
func TestEscOnTheLifetimeGoesBackToTheRequest(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	send(t, m, keyPress("esc"))

	if !onRequestCard(m) {
		t.Fatalf("dialog = %s, want the request back", describe(m.dialog))
	}
	if len(ds.approvals) != 0 {
		t.Fatal("going back approved something")
	}
}

// When the secret raised a question on the way to the lifetime, that question
// is the dialog before it, and Esc goes back there — with nothing it agreed to
// done yet.
func TestEscOnTheLifetimeGoesBackToTheBindingQuestion(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	req := waitingRequest()
	req.Host = "github.com"
	ds.requests = []CredentialRequest{req}
	ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "gh", Type: "bearer", Host: "api.github.com"}}
	m := newTestModel(t, ds)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	drain(t, m, m.dialog.action("yes"), 0)
	send(t, m, keyPress("esc"))

	if m.dialog == nil || m.dialog.kind != dlgConfirm || m.dialog.title != "Bound to another host" {
		t.Fatalf("dialog = %s, want the binding question back", describe(m.dialog))
	}
	if len(ds.bound) != 0 || len(ds.approvals) != 0 {
		t.Fatal("going back changed something")
	}
	// And agreeing a second time binds once, not twice.
	drain(t, m, m.dialog.action("yes"), 0)
	grantFor(t, m, time.Hour)
	if len(ds.bound) != 1 || len(ds.updated) != 1 {
		t.Fatalf("bound = %v, updates = %d, want the one change agreed to", ds.bound, len(ds.updated))
	}
}

// Every lifetime offered is one the `discobox secret` flags parse: the window
// and the CLI mint the same grant from the same words.
func TestTheOfferedLifetimesAreTheOnesTheFlagsTake(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithRequest(t)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	for _, item := range m.dialog.items {
		if item.key == lifetimeCustom {
			continue
		}
		d, err := lifetime.Parse(item.label)
		if err != nil {
			t.Fatalf("%q is offered and cannot be typed back: %v", item.label, err)
		}
		if got := strconv.FormatInt(lifetime.Seconds(d), 10); got != item.key {
			t.Fatalf("%q means %s seconds, and the step would grant %s", item.label, got, item.key)
		}
	}
}

// The presets are the common answers, not every answer.
func TestALifetimeCanBeTypedIn(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	drain(t, m, m.dialog.action(lifetimeCustom), 0)
	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatalf("dialog = %s, want somewhere to type a lifetime", describe(m.dialog))
	}
	// Esc goes back to the presets, one step, not to the request.
	send(t, m, keyPress("esc"))
	if !onLifetimeStep(m) {
		t.Fatalf("dialog = %s, want the presets back", describe(m.dialog))
	}

	drain(t, m, m.dialog.action(lifetimeCustom), 0)
	// Not a lifetime: the card stays up saying what one looks like, rather
	// than closing onto the screen behind it having granted nothing.
	drain(t, m, m.dialog.action("a while"), 0)
	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatalf("dialog = %s, want the question still up", describe(m.dialog))
	}
	// The refusal is the one thing on the re-ask that the first ask did not
	// carry; the footer naming the spellings is on both.
	if !strings.Contains(dialogText(m), `"a while" is not a lifetime`) {
		t.Fatalf("card = %q, want the refusal of what was typed", dialogText(m))
	}

	drain(t, m, m.dialog.action("36h"), 0)
	if len(ds.approvals) != 1 || ds.approvals[0].TTLSeconds != 36*3600 {
		t.Fatalf("approvals = %#v, want the typed lifetime", ds.approvals)
	}
}

// A credential that caps how long its grants may live is one the server would
// refuse the grant on. The window says so before the choice — on the secret's
// row, and on the lifetimes it does not allow — and asks, in the words the
// server would use, when one of those is chosen anyway.
func TestALifetimeLongerThanTheCredentialAllowsAsksFirst(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	ds.mu.Lock()
	ds.projectSecrets[0].MaxTTL = time.Hour
	ds.mu.Unlock()

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	if !strings.Contains(dialogText(m), "at most 1 hour") {
		t.Fatalf("card = %q, want the credential's limit on its row", dialogText(m))
	}
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	for _, item := range m.dialog.items {
		over := item.label != "1 hour" && item.key != lifetimeCustom
		if over != strings.Contains(item.detail, "asks first") {
			t.Fatalf("%s = %q, want only the lifetimes over the limit to say they ask", item.label, item.detail)
		}
	}
	grantFor(t, m, lifetime.Day)

	if m.dialog == nil || m.dialog.kind != dlgConfirm {
		t.Fatalf("dialog = %s, want the question about the limit", describe(m.dialog))
	}
	if !m.dialog.defaultNo {
		t.Fatal("raising how long every grant on a credential may live is not an Enter away")
	}
	if len(ds.approvals) != 0 {
		t.Fatal("it approved before asking")
	}
	for _, want := range []string{"1 hour", "1 day", "GitHub token"} {
		if !strings.Contains(dialogText(m), want) {
			t.Fatalf("question = %q, want it to name %q", dialogText(m), want)
		}
	}

	drain(t, m, m.dialog.action("yes"), 0)
	if len(ds.limited) != 1 || ds.limited[0] != "sec_gh=86400" {
		t.Fatalf("limited = %v, want the credential's limit raised to what was granted", ds.limited)
	}
	if len(ds.approvals) != 1 || ds.approvals[0].TTLSeconds != 86400 {
		t.Fatalf("approvals = %#v, want the day it asked about", ds.approvals)
	}
}

// No goes back to the lifetime, where a shorter one can be chosen instead.
func TestDecliningTheLimitReturnsToTheLifetime(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	ds.mu.Lock()
	ds.projectSecrets[0].MaxTTL = time.Hour
	ds.mu.Unlock()

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	grantFor(t, m, lifetime.Day)
	send(t, m, keyPress("esc"))

	if !onLifetimeStep(m) {
		t.Fatalf("dialog = %s, want the lifetime back", describe(m.dialog))
	}
	if len(ds.limited) != 0 || len(ds.approvals) != 0 {
		t.Fatal("declining changed something")
	}
	grantFor(t, m, time.Hour)
	if len(ds.limited) != 0 || len(ds.approvals) != 1 || ds.approvals[0].TTLSeconds != 3600 {
		t.Fatalf("limited = %v, approvals = %#v, want the hour granted and the limit left alone", ds.limited, ds.approvals)
	}
}

// The report is the only place the lifetime is said after the dialogs are gone.
func TestTheApprovalSaysHowLongItGrantedFor(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithRequest(t)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	grantFor(t, m, lifetime.Day)

	if !strings.Contains(strings.Join(frame(m), "\n"), "approved github for 1 day") {
		t.Fatalf("the window did not say what it granted:\n%s", strings.Join(frame(m), "\n"))
	}
}

func TestDenyingIsAnAnswerNotADismissal(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
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
	t.Parallel()
	m, ds := sourceWithRequest(t)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	drain(t, m, m.dialog.action("new"), 0)
	// How long comes before the token, so going back never has to hold a
	// token that was already typed.
	grantFor(t, m, lifetime.Week)
	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatal("choosing a new credential did not ask for one")
	}
	if !strings.Contains(dialogText(m), "1 week") {
		t.Fatalf("card = %q, want it to say how long it is being granted for", dialogText(m))
	}
	// Esc goes back one step, to the lifetime; and from there to the request.
	send(t, m, keyPress("esc"))
	if !onLifetimeStep(m) {
		t.Fatalf("dialog = %s, want the lifetime back", describe(m.dialog))
	}
	send(t, m, keyPress("esc"))
	if !onRequestCard(m) {
		t.Fatalf("dialog = %s, want the request back", describe(m.dialog))
	}
	drain(t, m, m.dialog.action("new"), 0)
	grantFor(t, m, lifetime.Week)
	// A credential is not drawn back as it is typed.
	if m.dialog.input.EchoMode == 0 {
		t.Fatal("the token is echoed in the clear")
	}
	drain(t, m, m.dialog.action("ghp_typedbyahuman"), 0)

	if len(ds.createdSecrets) != 1 {
		t.Fatalf("created = %#v, want the typed credential stored once", ds.createdSecrets)
	}
	created := ds.createdSecrets[0]
	if created.Value.Token != "ghp_typedbyahuman" {
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
	if ds.approvals[0].TTLSeconds != 604800 {
		t.Fatalf("ttl = %d, want the week that was chosen", ds.approvals[0].TTLSeconds)
	}
}

func TestAnEmptyTokenLeavesTheRequestWaiting(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	drain(t, m, m.dialog.action("new"), 0)
	grantFor(t, m, time.Hour)
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
	if credentialsLeaderKey == paneTerminalKey || strings.EqualFold(credentialsLeaderKey, paneTerminalKey) {
		t.Fatalf("the credential key (%q) is a shift away from the terminal key (%q)",
			credentialsLeaderKey, paneTerminalKey)
	}
	ds := newFakeSource(testSandboxes()...)
	ds.requests = []CredentialRequest{waitingRequest()}
	ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "GitHub token", Type: "bearer", Host: "api.github.com"}}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the banner", func() bool { return m.bannerTop() == 1 })

	d.key("ctrl+a")
	d.key(credentialsLeaderKey)
	d.wait("the question", func() bool { return m.dialog != nil && m.dialog.kind == dlgActions })
	if !strings.Contains(dialogText(m), "api.github.com") {
		t.Fatalf("dialog = %q, want the request the banner is about", dialogText(m))
	}
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
	ds := newFakeSource(testSandboxes()...)
	ds.requests = []CredentialRequest{waitingRequest()}
	ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "GitHub token", Type: "bearer", Host: "api.github.com"}}
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
		t.Fatalf("the lower band is on row %d of a %d-row frame, want it just above the status line",
			span.rows[1], len(frame(m)))
	}

	// Far from the words, still on the band — and the lower one answers as the
	// upper one does.
	x := span.end - 2
	for _, row := range span.rows {
		if !m.bannerAt(x, row) {
			t.Fatalf("row %d does not answer as the band", row)
		}
	}
	d.dispatch(tea.MouseClickMsg{X: x, Y: span.rows[1], Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: x, Y: span.rows[1], Button: tea.MouseLeft})
	// The secrets are read first, so the status dialog gives way to the question.
	d.wait("the question", func() bool { return m.dialog != nil && m.dialog.kind == dlgActions })
	if !strings.Contains(dialogText(m), "api.github.com") {
		t.Fatalf("dialog = %q, want the request the banner was about", dialogText(m))
	}
	// The press belongs to the banner: it must not also have started a
	// selection drag across the chrome.
	if m.chromeCapture {
		t.Fatal("the click also started a chrome selection")
	}
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

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
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

// A change agreed to on the way through is applied before the grant is minted —
// the server checks the ceiling at minting — so an approval that fails after it
// leaves a changed credential behind. The failure says so, since nothing else
// on screen will.
func TestAFailedApprovalSaysWhatWasAlreadyChanged(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	ds.mu.Lock()
	ds.projectSecrets[0].MaxTTL = time.Hour
	ds.approveErr = errors.New("secret request status changed concurrently; refresh and try again")
	ds.mu.Unlock()

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	grantFor(t, m, lifetime.Forever)
	drain(t, m, m.dialog.action("yes"), 0)

	if m.dialog == nil || !m.dialog.err {
		t.Fatalf("dialog = %s, want the failure", describe(m.dialog))
	}
	text := dialogText(m)
	for _, want := range []string{"changed concurrently", "already done", "GitHub token's limit was lifted", "forever"} {
		if !strings.Contains(text, want) {
			t.Fatalf("failure = %q, want it to say %q", text, want)
		}
	}
}

// A failure that changed nothing says nothing about changes.
func TestAFailedApprovalThatChangedNothingSaysNothingOfChanges(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	ds.mu.Lock()
	ds.approveErr = errTestRefused
	ds.mu.Unlock()

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	grantFor(t, m, time.Hour)

	if strings.Contains(dialogText(m), "already done") {
		t.Fatalf("failure = %q, want no changes claimed when there were none", dialogText(m))
	}
}

// Saying no goes back to the question. A dialog that closes onto the list,
// having done nothing and said nothing, is how a deliberate "not that one"
// reads as the window ignoring the keypress — which is what a refused
// approval looked like.
func TestDecliningToRebindReturnsToTheQuestion(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	req := waitingRequest()
	req.Host = "github.com"
	ds.requests = []CredentialRequest{req}
	ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "gh", Type: "token", Host: "api.github.com"}}
	m := newTestModel(t, ds)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	if m.dialog == nil || m.dialog.kind != dlgConfirm {
		t.Fatal("choosing a secret bound elsewhere did not ask")
	}

	// Esc is the same answer as no, and it is the one a hurried reader gives.
	drain(t, m, m.dialog.onCancel(), 0)
	if !onRequestCard(m) {
		t.Fatalf("dialog = %s, want the picker back", describe(m.dialog))
	}
	if !strings.Contains(dialogText(m), "github.com") {
		t.Fatalf("body = %q, want the request still in front of you", dialogText(m))
	}
	if len(ds.bound) != 0 || len(ds.approvals) != 0 {
		t.Fatal("declining changed something")
	}
}
