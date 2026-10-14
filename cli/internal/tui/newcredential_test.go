package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/wellknown"
)

// onNewCredentialForm is whether the card up is the new-secret form a request
// is answered with.
func onNewCredentialForm(m *Model) bool {
	return m.dialog != nil && m.dialog.kind == dlgForm && m.dialog.title == "New credential"
}

// toNewCredential opens the request and chooses a new credential, granted for
// the lifetime given, landing on the form.
func toNewCredential(t *testing.T, m *Model, ttl time.Duration) {
	t.Helper()
	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	drain(t, m, m.dialog.action("new"), 0)
	grantFor(t, m, ttl)
	if !onNewCredentialForm(m) {
		t.Fatalf("dialog = %s, want the new-secret form", describe(m.dialog))
	}
}

// githubRequest is the waiting request asking for com.github.api.
func githubRequest(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	m, ds := sourceWithRequest(t)
	ds.mu.Lock()
	ds.requests[0].WellKnownID = wellknown.GitHubAPI
	ds.mu.Unlock()
	send(t, m, credentialsLoadedMsg{requests: ds.requests})
	return m, ds
}

// A request for a well-known credential answered with a new one opens the
// secrets screen's own form, chosen as that credential: its name, host and
// the command that prints it are filled in, and storing it runs the command
// and approves the request with it for the lifetime chosen.
func TestANewWellKnownCredentialIsTheFormFilledIn(t *testing.T) {
	t.Parallel()
	m, ds := githubRequest(t)
	toNewCredential(t, m, lifetime.Week)

	f := m.dialog.form
	known, _ := wellknown.Lookup(wellknown.GitHubAPI)
	if f.chosen("kind") != wellKnownKind+wellknown.GitHubAPI || f.value("name") != known.Name || f.value("host") != known.Host() ||
		f.chosen("source") != "command" || f.value("command") != "gh auth token" {
		t.Fatalf("form = kind %q name %q host %q source %q command %q, want the well-known credential filled in",
			f.chosen("kind"), f.value("name"), f.value("host"), f.chosen("source"), f.value("command"))
	}
	if card := cardWords(m); !strings.Contains(card, "granted for 1 week") {
		t.Fatalf("card = %q, want it to say what the request is approved for", card)
	}
	send(t, m, keyPress("enter"))
	if len(ds.createdSecrets) != 1 {
		t.Fatalf("created = %#v, want one", ds.createdSecrets)
	}
	got := ds.createdSecrets[0]
	if got.WellKnownID != wellknown.GitHubAPI || got.Name != known.Name || !slices.Equal(got.RefreshCommand, ghCommand) ||
		got.Value.Token != "" || got.ValueTTLSeconds == nil || *got.ValueTTLSeconds != 86400 {
		t.Fatalf("created = %#v, want the credential from its command", got)
	}
	if len(ds.approvals) != 1 || ds.approvals[0].SecretID != "sec_new" || ds.approvals[0].TTLSeconds != 604800 {
		t.Fatalf("approvals = %#v, want the request approved with it for the week", ds.approvals)
	}
}

// Every field of the filled form stays the person's: a token typed in
// instead of the command is what is stored.
func TestTheFilledFormIsStillEditable(t *testing.T) {
	t.Parallel()
	m, ds := githubRequest(t)
	toNewCredential(t, m, time.Hour)
	f := m.dialog.form
	f.choose("source", "value")
	f.set("token", "ghp_typedbyahuman")
	f.set("name", "github-ci")
	send(t, m, keyPress("enter"))
	if len(ds.createdSecrets) != 1 || ds.createdSecrets[0].Name != "github-ci" || ds.createdSecrets[0].Value.Token != "ghp_typedbyahuman" ||
		ds.createdSecrets[0].RefreshCommand != nil {
		t.Fatalf("created = %#v, want what was typed", ds.createdSecrets)
	}
}

// A name the project already holds for that kind and host is offered as the
// first free one, which is what the server would otherwise refuse.
func TestATakenNameIsOfferedAsTheFirstFreeOne(t *testing.T) {
	t.Parallel()
	m, ds := githubRequest(t)
	known, _ := wellknown.Lookup(wellknown.GitHubAPI)
	ds.mu.Lock()
	ds.projectSecrets = append(ds.projectSecrets, Secret{ID: "sec_dupe", Name: known.Name, Type: "token", Host: known.Host()})
	ds.mu.Unlock()
	toNewCredential(t, m, time.Hour)
	if got := m.dialog.form.value("name"); got != known.Name+"-2" {
		t.Fatalf("name = %q, want the first free one", got)
	}
}

// A request that names no well-known credential fills the form with the name,
// kind and host it asks for; the token is typed, masked, and an empty one
// is refused on the form with the request still waiting.
func TestANewCredentialForAnyOtherAskIsFilledFromIt(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	toNewCredential(t, m, lifetime.Day)
	f := m.dialog.form
	if f.chosen("kind") != "token" || f.value("name") != "github" || f.value("host") != "api.github.com" || f.chosen("source") != "value" {
		t.Fatalf("form = kind %q name %q host %q source %q, want the request's", f.chosen("kind"), f.value("name"), f.value("host"), f.chosen("source"))
	}
	send(t, m, keyPress("enter"))
	if len(ds.createdSecrets) != 0 || !onNewCredentialForm(m) || f.err == "" {
		t.Fatalf("created = %#v, dialog = %s, want an empty token refused on the form", ds.createdSecrets, describe(m.dialog))
	}
	for _, row := range f.rows {
		if row.key == "token" && !row.masked {
			t.Fatal("the token is drawn in the clear")
		}
	}
	f.set("token", "ghp_typedbyahuman")
	send(t, m, keyPress("enter"))
	if len(ds.createdSecrets) != 1 || ds.createdSecrets[0].Name != "github" || ds.createdSecrets[0].Host != "api.github.com" ||
		ds.createdSecrets[0].Value.Token != "ghp_typedbyahuman" {
		t.Fatalf("created = %#v, want it shaped by the request", ds.createdSecrets)
	}
	if len(ds.approvals) != 1 || ds.approvals[0].TTLSeconds != 86400 {
		t.Fatalf("approvals = %#v, want the day chosen", ds.approvals)
	}
}

// Esc from the form is the request, still waiting, with nothing stored.
func TestEscFromTheNewCredentialFormLeavesTheRequestWaiting(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	toNewCredential(t, m, time.Hour)
	m.dialog.form.set("token", "ghp_typedbyahuman")
	send(t, m, keyPress("esc"))
	if !onRequestCard(m) || len(ds.createdSecrets) != 0 || len(ds.approvals) != 0 || len(m.requests["sbx_one"]) != 1 {
		t.Fatalf("dialog = %s, created %d, approved %d; want the request back, waiting", describe(m.dialog), len(ds.createdSecrets), len(ds.approvals))
	}
	// And the lifetime's Esc is the request too.
	drain(t, m, m.dialog.action("new"), 0)
	send(t, m, keyPress("esc"))
	if !onRequestCard(m) {
		t.Fatalf("dialog = %s, want the request back", describe(m.dialog))
	}
}

// A grant limit set below the lifetime being granted is refused on the form,
// before anything is stored: the server would refuse the grant after.
func TestALimitShorterThanTheGrantIsRefusedOnTheForm(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	toNewCredential(t, m, lifetime.Week)
	f := m.dialog.form
	f.set("token", "ghp_typedbyahuman")
	f.choose("ttl", "3600")
	send(t, m, keyPress("enter"))
	if len(ds.createdSecrets) != 0 || !strings.Contains(f.err, "shorter than the 1 week") {
		t.Fatalf("created = %#v, err = %q, want the limit refused on the form", ds.createdSecrets, f.err)
	}
}

// The form's key line is whole at the width a card is drawn, Esc included.
func TestTheNewCredentialFormSaysEscAtNinetyColumns(t *testing.T) {
	t.Parallel()
	m, _ := githubRequest(t)
	send(t, m, sizeMsg(90, 60))
	toNewCredential(t, m, time.Hour)
	if frame := plainFrame(m); !strings.Contains(frame, "enter stores and approves") || !strings.Contains(frame, "esc back, nothing stored") {
		t.Fatalf("key line is cut:\n%s", frame)
	}
}

// githubAnswered is the waiting com.github.api request in a project that
// already has a secret marked as that ID's answer.
func githubAnswered(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	m, ds := githubRequest(t)
	ds.mu.Lock()
	ds.projectSecrets = append(ds.projectSecrets, Secret{ID: "sec_marked", Name: "gh-main", Type: "token", Host: "github.com", WellKnownID: wellknown.GitHubAPI})
	ds.mu.Unlock()
	send(t, m, credentialsLoadedMsg{requests: ds.requests})
	return m, ds
}

// With the ID already answered, a new credential on the request card is still
// one: filled as a plain token from the well-known credential, saying why it
// is not marked, stored without the ID — so the server's refusal to mark a
// second is never reached — and this one request approved with it.
func TestANewCredentialForAnAnsweredIDIsAPlainToken(t *testing.T) {
	t.Parallel()
	m, ds := githubAnswered(t)
	toNewCredential(t, m, time.Hour)
	f := m.dialog.form
	known, _ := wellknown.Lookup(wellknown.GitHubAPI)
	if f.chosen("kind") != "token" || f.value("name") != known.Name || f.value("host") != known.Host() ||
		f.chosen("source") != "command" || f.value("command") != "gh auth token" {
		t.Fatalf("form = kind %q name %q host %q source %q command %q, want a plain token filled from the credential",
			f.chosen("kind"), f.value("name"), f.value("host"), f.chosen("source"), f.value("command"))
	}
	if card := cardWords(m); !strings.Contains(card, "com.github.api is already answered by gh-main; this one is stored without the ID, and answers only this request") {
		t.Fatalf("card = %q, want it to say why it is not marked", card)
	}
	send(t, m, keyPress("enter"))
	if len(ds.createdSecrets) != 1 || ds.createdSecrets[0].WellKnownID != "" {
		t.Fatalf("created = %#v, want one stored without the ID", ds.createdSecrets)
	}
	if len(ds.approvals) != 1 || ds.approvals[0].SecretID != "sec_new" || (m.dialog != nil && m.dialog.err) {
		t.Fatalf("approvals = %#v, dialog = %s; want this request approved with it, nothing refused", ds.approvals, describe(m.dialog))
	}
}

// In auto approve a rule for an answered ID answers with the marked secret,
// so a new credential could never be the rule's: the row is there, disabled,
// saying so and where to change what the rule answers with.
func TestAutoApprovesNewCredentialIsDisabledForAnAnsweredID(t *testing.T) {
	t.Parallel()
	m, ds := githubAnswered(t)
	send(t, m, keyPress("tab"), keyPress(credentialsKey), keyPress(autoApproveKey))
	var row action
	for _, item := range m.dialog.items {
		if item.key == "new" {
			row = item
		}
	}
	if row.enabled || !strings.Contains(row.why, "answers with gh-main") || !strings.Contains(row.why, "F4, e") {
		t.Fatalf("row = %#v, want it disabled, saying why and where to edit", row)
	}
	step := m.dialog
	send(t, m, keyPress("n"))
	if m.dialog != step || len(ds.createdSecrets) != 0 {
		t.Fatalf("dialog = %s, want n to do nothing", describe(m.dialog))
	}
}

// Auto approve's form says a secret stored from it answers the ID from then
// on, even if the rule is declined; and leaving the flow without a rule after
// it was stored says what was stored and how to remove it — on the way back
// to the secrets, and on a refusal.
func TestAutoApproveSaysWhatItStoredWhenNoRuleIsMade(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	req := waitingRequest()
	req.WellKnownID = wellknown.GitHubAPI
	ds.requests = []CredentialRequest{req}
	ds.proposals = proposalsForTheRequest()
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress(credentialsKey), keyPress(autoApproveKey))
	drain(t, m, m.dialog.action("new"), 0)
	if card := cardWords(m); !strings.Contains(card, "every request for com.github.api from then on, by a rule or by you — even if you decline the rule") {
		t.Fatalf("card = %q, want it to say the secret answers the ID from then on", card)
	}
	send(t, m, keyPress("enter"))
	if !onRuleStep(m, "which discoboxes") {
		t.Fatalf("dialog = %s, want the flow gone on", describe(m.dialog))
	}
	known, _ := wellknown.Lookup(wellknown.GitHubAPI)
	want := "stored " + known.Name + " with no rule made: it stays, answering every request for com.github.api · d on the secrets table (F4) removes it"
	send(t, m, keyPress("esc"))
	if m.dialog == nil || m.dialog.title != "Auto approve" || !strings.Contains(m.status, want) {
		t.Fatalf("dialog = %s, status = %q; want the secret step and the stored secret said", describe(m.dialog), m.status)
	}

	// A refusal of the rule says it too, beside the server's reason.
	ds.mu.Lock()
	ds.approveErr = errRuleRefused
	ds.mu.Unlock()
	drain(t, m, m.dialog.action("secret:sec_new"), 0)
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	drain(t, m, m.dialog.action("content:1"), 0)
	grantFor(t, m, time.Hour)
	send(t, m, keyPress("y"))
	if m.dialog == nil || !m.dialog.err || !strings.Contains(cardWords(m), want) {
		t.Fatalf("dialog = %s, want the refusal to say what was stored:\n%s", describe(m.dialog), wholeCard(m))
	}
}
