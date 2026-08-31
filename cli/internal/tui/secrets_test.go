package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The secrets screen is the operator's side of the credential inbox: what the
// project holds, what stands on it, and what is waiting on a person. Every
// action here is the same API call the `discobox secret` commands make — a
// window that could approve something the CLI could not would be a second
// policy.

// describe is what a dialog is, for a failure message: dumping the struct
// prints a textinput's entire style tree and buries the one line that matters.
func describe(d *dialog) string {
	if d == nil {
		return "<no dialog>"
	}
	return fmt.Sprintf("kind=%d title=%q body=%q", d.kind, d.title, d.body)
}

func secretsFixture(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	ds := newFakeSource(testSandboxes()...)
	ds.projectSecrets = []Secret{
		{ID: "sec_gh", Name: "gh", Type: "bearer", Host: "github.com", MaxTTL: time.Hour},
		{ID: "sec_loose", Name: "OpenAI key", Type: "bearer", MaxTTL: time.Hour},
	}
	ds.projectGrants = []Grant{
		{ID: "grant_1", SecretID: "sec_gh", Scope: "sandbox", ScopeKey: "sbx_one", Host: "api.github.com",
			Uses: []GrantUse{{ID: "use_7f3c", Description: "Open a PR"}}, Granted: time.Now().Add(-time.Minute)},
		{ID: "grant_2", SecretID: "sec_gh", Scope: "project", ScopeKey: "project-1", Host: "github.com"},
	}
	m := newTestModel(t, ds)
	send(t, m, key(secretsKey))
	if !m.secretsOpen {
		t.Fatal("the secrets screen did not open")
	}
	return m, ds
}

func TestTheScreenShowsWhatEachSecretIsAndWhatStandsOnIt(t *testing.T) {
	m, _ := secretsFixture(t)

	body := strings.Join(frame(m), "\n")
	for _, want := range []string{"Secrets", "gh", "github.com", "2 grants", "OpenAI key", "any host"} {
		if !strings.Contains(body, want) {
			t.Fatalf("frame does not carry %q:\n%s", want, body)
		}
	}
}

// The binding is the whole of what limits where a credential may travel, so an
// unbound secret says so rather than showing a blank — blank reads as "not
// loaded".
func TestAnUnboundSecretSaysSo(t *testing.T) {
	m, _ := secretsFixture(t)
	m.secrets.moveTo(1)
	if s := m.secrets.current(); s == nil || s.ID != "sec_loose" {
		t.Fatalf("cursor = %#v, want the unbound secret", m.secrets.current())
	}
	if !strings.Contains(strings.Join(frame(m), "\n"), "any host") {
		t.Fatal("an unbound secret is drawn as a blank")
	}
}

func TestGrantsAreListedAndRevoked(t *testing.T) {
	m, ds := secretsFixture(t)

	send(t, m, key("enter"))
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatal("enter did not list the grants standing on the secret")
	}
	if len(m.dialog.items) != 3 {
		t.Fatalf("items = %d, want the secret's two grants and the row that makes one", len(m.dialog.items))
	}
	if m.dialog.items[0].key != grantCreateItem {
		t.Fatalf("first row = %q, want making one to lead", m.dialog.items[0].key)
	}
	// A grant says what it covers before it offers to be withdrawn.
	joined := m.dialog.items[1].label + " " + m.dialog.items[1].detail
	for _, want := range []string{"api.github.com", "sandbox", "sbx_one", "approved use"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("row = %q, want it to carry %q", joined, want)
		}
	}

	// Enter reads; x withdraws.
	drain(t, m, m.dialog.action("grant_1"), 0)
	if m.dialog == nil || m.dialog.kind != dlgText {
		t.Fatalf("enter did not read the grant: %s", describe(m.dialog))
	}
	for _, want := range []string{"api.github.com", "sandbox", "Open a PR", "use_7f3c", "discobox-access run --use"} {
		if !strings.Contains(m.dialog.body, want) {
			t.Fatalf("review = %q, want it to carry %q", m.dialog.body, want)
		}
	}
	// Esc comes back to the grants, so the withdraw key is right there.
	send(t, m, key("esc"))
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %s, want the grants back", describe(m.dialog))
	}
	drain(t, m, m.dialog.alt("grant_1"), 0)
	if m.dialog == nil || m.dialog.kind != dlgConfirm {
		t.Fatalf("x did not ask before withdrawing: %s", describe(m.dialog))
	}
	if !m.dialog.defaultNo {
		t.Fatal("the costly answer is the default")
	}
	drain(t, m, m.dialog.action("yes"), 0)
	if len(ds.revoked) != 1 || ds.revoked[0] != "grant_1" {
		t.Fatalf("revoked = %v, want the chosen grant", ds.revoked)
	}
	// The screen re-reads rather than guessing what the server now holds.
	if got := m.secrets.grantsFor("sec_gh"); len(got) != 1 {
		t.Fatalf("grants = %#v, want the withdrawn one gone", got)
	}
}

func TestANewSecretIsNamedBoundAndStoredMasked(t *testing.T) {
	m, ds := secretsFixture(t)

	send(t, m, key("n"))
	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatal("n did not ask for a name")
	}
	drain(t, m, m.dialog.action("npm"), 0)
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %s, want the kind question", describe(m.dialog))
	}
	drain(t, m, m.dialog.action("token"), 0)
	if m.dialog == nil || !strings.Contains(m.dialog.body, "beneath it") {
		t.Fatalf("the binding question does not say what a host covers: %q", m.dialog.body)
	}
	drain(t, m, m.dialog.action("registry.npmjs.org"), 0)
	if m.dialog == nil || m.dialog.input.EchoMode == 0 {
		t.Fatal("the token is asked for in the clear")
	}
	drain(t, m, m.dialog.action("npm_typedbyahuman"), 0)

	if len(ds.createdSecrets) != 1 {
		t.Fatalf("created = %#v, want one", ds.createdSecrets)
	}
	got := ds.createdSecrets[0]
	if got.Name != "npm" || got.Host != "registry.npmjs.org" || got.Value != "npm_typedbyahuman" {
		t.Fatalf("stored = %#v, want what was typed", got)
	}
}

func TestTheBindingCanBeEditedAndReleased(t *testing.T) {
	m, ds := secretsFixture(t)

	send(t, m, key("e"))
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatal("e did not offer what a secret says about itself")
	}
	drain(t, m, m.dialog.action("host"), 0)
	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatal("picking the binding did not ask for one")
	}
	// It opens on what the secret has, so editing is editing rather than
	// retyping.
	if m.dialog.input.Value() != "github.com" {
		t.Fatalf("input = %q, want the current binding", m.dialog.input.Value())
	}
	drain(t, m, m.dialog.action(""), 0)
	if len(ds.unbound) != 1 || ds.unbound[0] != "sec_gh" {
		t.Fatalf("unbound = %v, want the emptied binding released", ds.unbound)
	}
}

// Deleting takes the grants with it, so the question says so and Enter is not
// the answer.
func TestDeletingAsksFirstAndSaysWhatGoesWithIt(t *testing.T) {
	m, ds := secretsFixture(t)

	send(t, m, key("d"))
	if m.dialog == nil || m.dialog.kind != dlgConfirm {
		t.Fatal("d deleted without asking")
	}
	if !m.dialog.defaultNo {
		t.Fatal("the costly answer is the default")
	}
	if !strings.Contains(m.dialog.body, "2 live grants") {
		t.Fatalf("question = %q, want it to say what goes with the secret", m.dialog.body)
	}
	if len(ds.deleted) != 0 {
		t.Fatal("it deleted before the answer")
	}

	drain(t, m, m.dialog.action("yes"), 0)
	if len(ds.deleted) != 1 || ds.deleted[0] != "sec_gh" {
		t.Fatalf("deleted = %v", ds.deleted)
	}
}

// A request nobody's discobox row can carry — one with no sandbox — still has
// to be answerable, and this screen is where an operator is looking.
func TestTheScreenCountsAndOpensWaitingRequests(t *testing.T) {
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.requests = []CredentialRequest{waitingRequest()}
	ds.mu.Unlock()
	send(t, m, tickMsg{})

	body := strings.Join(frame(m), "\n")
	if !strings.Contains(body, "Requests") || !strings.Contains(body, "1 waiting") {
		t.Fatalf("the screen does not show what is waiting:\n%s", body)
	}
	if !strings.Contains(body, "github") || !strings.Contains(body, "api.github.com") {
		t.Fatalf("the waiting request is not drawn as a row:\n%s", body)
	}
	send(t, m, key(credentialsKey))
	// Reading the secrets comes first, then the question itself.
	drain(t, m, m.loadCredentialRequests(), 0)
	if m.dialog == nil {
		t.Fatal("the key did not open the waiting request")
	}
}

func TestTheScreenIsAToggleAndEscapeLeavesIt(t *testing.T) {
	m, _ := secretsFixture(t)
	send(t, m, key(secretsKey))
	if m.secretsOpen {
		t.Fatal("the key that opened the screen did not put it away")
	}
	send(t, m, key(secretsKey), key("esc"))
	if m.secretsOpen {
		t.Fatal("esc did not leave the screen")
	}
}

// A request no discobox owns — made from the CLI, or by a person — is exactly
// what this screen is for: there is no row to mark and no workspace to raise it
// in, so if it is not counted here it is invisible everywhere.
func TestTheScreenAnswersARequestNoDiscoboxOwns(t *testing.T) {
	m, ds := secretsFixture(t)
	orphan := waitingRequest()
	orphan.ID, orphan.SandboxID = "sreq_orphan", ""
	ds.mu.Lock()
	ds.requests = []CredentialRequest{orphan}
	ds.mu.Unlock()
	send(t, m, tickMsg{})

	body := strings.Join(frame(m), "\n")
	if !strings.Contains(body, "1 waiting") || !strings.Contains(body, "a person") {
		t.Fatalf("a request with no discobox is not shown on the operator's screen:\n%s", body)
	}
	// And no row is marked for it, since there is no row it belongs to.
	for _, s := range m.list.all {
		if s.PendingRequests != 0 {
			t.Fatalf("%s is marked for a request no discobox owns", s.ID)
		}
	}

	send(t, m, key(credentialsKey))
	drain(t, m, m.loadCredentialRequests(), 0)
	if m.dialog == nil {
		t.Fatal("the key did not open the waiting request")
	}
	if !strings.Contains(m.dialog.body, "api.github.com") {
		t.Fatalf("dialog = %q, want the orphan request", m.dialog.body)
	}
}

// Driven by keys rather than by calling the dialog's action directly, because
// that is where the chain broke: the model took the answered dialog down after
// the action had already put the next question up, so a new secret asked for a
// name and then went back to the list.
func TestTheNewSecretChainAsksEveryQuestion(t *testing.T) {
	m, ds := secretsFixture(t)

	send(t, m, key("n"))
	if m.dialog == nil || !strings.Contains(m.dialog.body, "called") {
		t.Fatalf("dialog = %s, want the name question", describe(m.dialog))
	}
	send(t, m, typeString("npm")...)
	send(t, m, key("enter"))
	if m.dialog == nil {
		t.Fatal("answering the name left no dialog; the chain was cut")
	}
	if m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %s, want the kind question", describe(m.dialog))
	}
	send(t, m, key("enter")) // a token, the first row
	if !strings.Contains(m.dialog.body, "Which host may it be sent to?") {
		t.Fatalf("dialog = %q, want the host question", m.dialog.body)
	}
	send(t, m, typeString("registry.npmjs.org")...)
	send(t, m, key("enter"))
	if m.dialog == nil || !strings.Contains(m.dialog.body, "Paste the token") {
		t.Fatalf("dialog = %s, want the value question", describe(m.dialog))
	}
	if m.dialog.input.EchoMode == 0 {
		t.Fatal("the value question echoes the token")
	}
	send(t, m, typeString("npm_value")...)
	send(t, m, key("enter"))

	if len(ds.createdSecrets) != 1 {
		t.Fatalf("created = %#v, want the secret the three answers describe", ds.createdSecrets)
	}
	got := ds.createdSecrets[0]
	if got.Name != "npm" || got.Host != "registry.npmjs.org" || got.Value != "npm_value" {
		t.Fatalf("created = %#v", got)
	}
}

// A pre-approval is a grant minted because somebody already knows the answer,
// rather than one that answers a request. The scope decides whether there is a
// key to ask for next, and every key is picked out of what the window already
// holds — nothing here is an ID somebody has to type correctly.
func TestAPreApprovalIsScopedThenBoundThenTimed(t *testing.T) {
	m, ds := secretsFixture(t)

	send(t, m, key(grantCreateKey))
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %s, want the scope question", describe(m.dialog))
	}
	var scopes []string
	for _, item := range m.dialog.items {
		scopes = append(scopes, item.key)
	}
	if len(scopes) != 3 {
		t.Fatalf("scopes = %v, want all three", scopes)
	}

	// The narrowest scope, which is the one that needs a discobox chosen.
	send(t, m, typeString("sandbox")...)
	drain(t, m, m.dialog.action("sandbox"), 0)
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %s, want the discobox picker", describe(m.dialog))
	}
	if m.dialog.items[0].key != testSandboxes()[0].ID {
		t.Fatalf("picker = %#v, want the project's discoboxes", m.dialog.items)
	}
	drain(t, m, m.dialog.action(testSandboxes()[0].ID), 0)

	// A discobox grant is asked how it may be used; this is the ordinary kind.
	drain(t, m, m.dialog.action("any"), 0)

	// The host opens on the secret's own binding, which is the common answer
	// and the widest the server will take.
	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatalf("dialog = %s, want the host question", describe(m.dialog))
	}
	if m.dialog.input.Value() != "github.com" {
		t.Fatalf("host = %q, want the secret's binding", m.dialog.input.Value())
	}
	drain(t, m, m.dialog.action("api.github.com"), 0)

	// Then the lifetime, prefilled from the secret's own default.
	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatalf("dialog = %s, want the lifetime question", describe(m.dialog))
	}
	if m.dialog.input.Value() != "3600" {
		t.Fatalf("ttl = %q, want the secret's default", m.dialog.input.Value())
	}
	drain(t, m, m.dialog.action("900"), 0)

	if len(ds.createdGrants) != 1 {
		t.Fatalf("created = %#v, want one grant", ds.createdGrants)
	}
	got := ds.createdGrants[0]
	if got.SecretID != "sec_gh" || got.Scope != "sandbox" || got.ScopeKey != testSandboxes()[0].ID {
		t.Fatalf("grant = %#v, want it scoped to the chosen discobox", got)
	}
	if got.Host != "api.github.com" || got.TTLSeconds != 900 {
		t.Fatalf("grant = %#v, want the host and lifetime that were answered", got)
	}
}

// A project grant needs no key, so it goes straight to the host.
func TestAProjectPreApprovalSkipsTheKey(t *testing.T) {
	m, _ := secretsFixture(t)

	send(t, m, key(grantCreateKey))
	drain(t, m, m.dialog.action("project"), 0)
	drain(t, m, m.dialog.action("any"), 0)
	if m.dialog == nil || m.dialog.kind != dlgInput || !strings.Contains(m.dialog.body, "every discobox in this project") {
		t.Fatalf("dialog = %s, want the host question for a project grant", describe(m.dialog))
	}
}

// A lifetime that is not a number grants nothing, rather than quietly meaning
// something else.
func TestAnUnreadableLifetimeGrantsNothing(t *testing.T) {
	m, ds := secretsFixture(t)

	send(t, m, key(grantCreateKey))
	drain(t, m, m.dialog.action("project"), 0)
	drain(t, m, m.dialog.action("any"), 0)
	drain(t, m, m.dialog.action("github.com"), 0)
	drain(t, m, m.dialog.action("a while"), 0)
	if len(ds.createdGrants) != 0 {
		t.Fatalf("created = %#v, want nothing", ds.createdGrants)
	}
}

// Reading a grant and coming back lands on the grants, not on the list behind
// them: the next move after reading one is reading the next, or withdrawing it.
func TestLeavingAGrantReturnsToTheGrants(t *testing.T) {
	m, _ := secretsFixture(t)

	send(t, m, key("enter"))
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %s, want the grants", describe(m.dialog))
	}
	drain(t, m, m.dialog.action("grant_1"), 0)
	if m.dialog == nil || m.dialog.kind != dlgText {
		t.Fatalf("dialog = %s, want the grant being read", describe(m.dialog))
	}

	drain(t, m, m.dialog.onCancel(), 0)
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %s, want the grants back", describe(m.dialog))
	}
	if m.dialog.title != "gh" {
		t.Fatalf("title = %q, want the secret it was opened from", m.dialog.title)
	}
}

// Declining a revoke leaves you on the grants, not on the screen behind them.
func TestDecliningARevokeReturnsToTheGrants(t *testing.T) {
	m, ds := secretsFixture(t)

	send(t, m, key("enter"))
	drain(t, m, m.dialog.alt("grant_1"), 0)
	if m.dialog == nil || m.dialog.kind != dlgConfirm {
		t.Fatalf("dialog = %s, want the question", describe(m.dialog))
	}
	drain(t, m, m.dialog.onCancel(), 0)

	if m.dialog == nil || m.dialog.kind != dlgActions || m.dialog.title != "gh" {
		t.Fatalf("dialog = %s, want the secret back", describe(m.dialog))
	}
	if len(ds.revoked) != 0 {
		t.Fatal("declining withdrew something")
	}
}

// The two tables are one screen read downwards: off the bottom of the secrets
// is what is waiting, and off the top of that is the secrets again — the same
// way the discobox list and the prompt hand focus to each other.
func TestTheTablesHandFocusToEachOther(t *testing.T) {
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.requests = []CredentialRequest{waitingRequest()}
	ds.mu.Unlock()
	send(t, m, tickMsg{})

	if m.onRequests {
		t.Fatal("the screen opens on the requests")
	}
	send(t, m, key("end"))
	send(t, m, key("down"))
	if !m.onRequests {
		t.Fatal("down past the last secret did not reach the requests")
	}
	send(t, m, key("up"))
	if m.onRequests {
		t.Fatal("up past the first request did not return to the secrets")
	}
	// And Tab crosses either way, wherever the cursor is.
	send(t, m, key("tab"))
	if !m.onRequests {
		t.Fatal("tab did not cross to the requests")
	}
	send(t, m, key("tab"))
	if m.onRequests {
		t.Fatal("tab did not cross back")
	}
	// Tab no longer leaves the screen; esc does.
	if !m.secretsOpen {
		t.Fatal("tab closed the screen")
	}
	send(t, m, key("esc"))
	if m.secretsOpen {
		t.Fatal("esc did not leave the screen")
	}
}

// Enter on a request opens the same question the row mark and the workspace
// banner open, for that request rather than the oldest one.
func TestEnterOnARequestAnswersThatOne(t *testing.T) {
	m, ds := secretsFixture(t)
	older, newer := waitingRequest(), waitingRequest()
	older.ID, older.Host, older.Created = "sreq_older", "api.github.com", older.Created.Add(-time.Hour)
	newer.ID, newer.Name, newer.Host = "sreq_newer", "npm", "registry.npmjs.org"
	ds.mu.Lock()
	ds.requests = []CredentialRequest{older, newer}
	ds.mu.Unlock()
	send(t, m, tickMsg{})

	send(t, m, key("tab"))
	if !m.onRequests || len(m.requestRows.all) != 2 {
		t.Fatalf("requests = %d, want both", len(m.requestRows.all))
	}
	// The second row, which is not the one C would have picked.
	send(t, m, key("down"))
	chosen := m.requestRows.current()
	if chosen == nil {
		t.Fatal("no request under the cursor")
	}
	send(t, m, key("enter"))
	drain(t, m, m.loadCredentialRequests(), 0)
	if m.dialog == nil {
		t.Fatal("enter answered nothing")
	}
	if !strings.Contains(m.dialog.body, chosen.Host) {
		t.Fatalf("dialog = %q, want the request under the cursor (%s)", m.dialog.body, chosen.Host)
	}
}

// Making a grant starts from the grants: a secret with none is not a dead end
// telling somebody to go and run another command.
func TestGrantsCanBeMadeFromTheGrantsList(t *testing.T) {
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.projectGrants = nil
	ds.mu.Unlock()
	send(t, m, key("r"))

	send(t, m, key("enter"))
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %s, want the grants", describe(m.dialog))
	}
	if !strings.Contains(m.dialog.body, "Nothing stands on this secret") {
		t.Fatalf("body = %q, want it to say the secret has none", m.dialog.body)
	}
	if len(m.dialog.items) != 1 || m.dialog.items[0].key != grantCreateItem {
		t.Fatalf("items = %#v, want the row that makes one", m.dialog.items)
	}
	drain(t, m, m.dialog.action(grantCreateItem), 0)
	if m.dialog == nil || !strings.Contains(m.dialog.body, "Who may use it") {
		t.Fatalf("dialog = %s, want the scope question", describe(m.dialog))
	}
}

// What a request row says it is about. The proxy's reactive path names
// nothing — all it saw was a sentinel it could not resolve — and a person's
// ask is not that.
func TestARequestRowSaysWhatItIsAbout(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  CredentialRequest
		want string
	}{
		{"an agent names the credential", CredentialRequest{Name: "github", SandboxID: "sbx_1", Uses: []string{"x"}}, "github"},
		{"failing that, the variable", CredentialRequest{EnvVar: "GH_TOKEN", SandboxID: "sbx_1"}, "GH_TOKEN"},
		{"the proxy saw a sentinel", CredentialRequest{SandboxID: "sbx_1", Type: "token"}, "an unresolvable sentinel"},
		{"a person asked for a credential", CredentialRequest{Type: "token"}, "a token credential"},
	} {
		if got := requestSubject(tc.req); got != tc.want {
			t.Errorf("%s: subject = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := requesterLabel(CredentialRequest{}); got != "a person" {
		t.Errorf("requester = %q, want a person", got)
	}
	if got := requesterLabel(CredentialRequest{SandboxID: "sbx_abcdefghij"}); got != "proxy · abcdefgh" {
		t.Errorf("requester = %q, want the proxy and a short id", got)
	}
}

// A pre-approval in the shape an agent asks for: never injected, taken one
// declared use at a time. The question that decides it only comes up for a
// discobox, because that is the only scope the binding has.
func TestAGrantCanBeMadeAccessOnly(t *testing.T) {
	m, ds := secretsFixture(t)

	send(t, m, key(grantCreateKey))
	drain(t, m, m.dialog.action("sandbox"), 0)
	drain(t, m, m.dialog.action(testSandboxes()[0].ID), 0)

	if m.dialog == nil || !strings.Contains(m.dialog.title, "How may it be used") {
		t.Fatalf("dialog = %s, want the question about how it may be used", describe(m.dialog))
	}
	if !strings.Contains(m.dialog.body, "readable by everything in it") {
		t.Fatalf("body = %q, want it to say what an injected credential means", m.dialog.body)
	}
	drain(t, m, m.dialog.action("access"), 0)

	if m.dialog == nil || m.dialog.kind != dlgInput || !strings.Contains(m.dialog.title, "Delivered as") {
		t.Fatalf("dialog = %s, want the variable question", describe(m.dialog))
	}
	drain(t, m, m.dialog.action("GH_TOKEN"), 0)

	if m.dialog == nil || !strings.Contains(m.dialog.title, "Approved use") {
		t.Fatalf("dialog = %s, want the use question", describe(m.dialog))
	}
	drain(t, m, m.dialog.action("Open a pull request against the current repo"), 0)
	drain(t, m, m.dialog.action("api.github.com"), 0)
	drain(t, m, m.dialog.action("900"), 0)

	if len(ds.createdGrants) != 1 {
		t.Fatalf("created = %#v, want one grant", ds.createdGrants)
	}
	got := ds.createdGrants[0]
	if got.EnvVar != "GH_TOKEN" || len(got.Uses) != 1 || got.Uses[0] != "Open a pull request against the current repo" {
		t.Fatalf("grant = %#v, want the variable and the use that were answered", got)
	}
	if got.Scope != "sandbox" || got.Host != "api.github.com" {
		t.Fatalf("grant = %#v, want it scoped and hosted", got)
	}
}

// Choosing the ordinary kind skips both questions: an injected credential has
// no use to declare and no variable to name here.
func TestAnInjectedGrantSkipsTheUseQuestions(t *testing.T) {
	m, ds := secretsFixture(t)

	send(t, m, key(grantCreateKey))
	drain(t, m, m.dialog.action("sandbox"), 0)
	drain(t, m, m.dialog.action(testSandboxes()[0].ID), 0)
	drain(t, m, m.dialog.action("any"), 0)

	if m.dialog == nil || !strings.Contains(m.dialog.body, "Where may it be sent") {
		t.Fatalf("dialog = %s, want the host question", describe(m.dialog))
	}
	drain(t, m, m.dialog.action("api.github.com"), 0)
	drain(t, m, m.dialog.action("0"), 0)

	if len(ds.createdGrants) != 1 || len(ds.createdGrants[0].Uses) != 0 || ds.createdGrants[0].EnvVar != "" {
		t.Fatalf("grant = %#v, want a plain standing grant", ds.createdGrants)
	}
}

// A project grant is asked the same question: the binding it needs is per
// discobox, but it is minted when that discobox's agent first asks, so the
// grant does not need to know which boxes it will cover.
func TestAProjectGrantCanBeAccessOnlyToo(t *testing.T) {
	m, ds := secretsFixture(t)

	send(t, m, key(grantCreateKey))
	drain(t, m, m.dialog.action("project"), 0)
	if m.dialog == nil || !strings.Contains(m.dialog.title, "How may it be used") {
		t.Fatalf("dialog = %s, want the question about how it may be used", describe(m.dialog))
	}
	drain(t, m, m.dialog.action("access"), 0)
	drain(t, m, m.dialog.action("GH_TOKEN"), 0)
	drain(t, m, m.dialog.action("Open a PR"), 0)
	drain(t, m, m.dialog.action("api.github.com"), 0)
	drain(t, m, m.dialog.action("0"), 0)

	if len(ds.createdGrants) != 1 {
		t.Fatalf("created = %#v, want one grant", ds.createdGrants)
	}
	got := ds.createdGrants[0]
	if got.Scope != "project" || got.EnvVar != "GH_TOKEN" || len(got.Uses) != 1 {
		t.Fatalf("grant = %#v, want a project grant in the agent credentials shape", got)
	}
}

// An OAuth credential is more than one field, and all of them are asked for:
// the access token travels, and the rest is what the control plane spends to
// renew it when that token goes stale.
func TestAnOAuthSecretIsRegisteredFromTheWindow(t *testing.T) {
	m, ds := secretsFixture(t)

	send(t, m, key("n"))
	drain(t, m, m.dialog.action("claude"), 0)
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %s, want the kind question", describe(m.dialog))
	}
	drain(t, m, m.dialog.action("oauth"), 0)
	drain(t, m, m.dialog.action("api.anthropic.com"), 0)

	if m.dialog == nil || !strings.Contains(m.dialog.body, "access token") {
		t.Fatalf("dialog = %s, want the access token question", describe(m.dialog))
	}
	if m.dialog.input.EchoMode == 0 {
		t.Fatal("the access token is echoed")
	}
	drain(t, m, m.dialog.action("sk-ant-oat01-access"), 0)

	if m.dialog == nil || !strings.Contains(m.dialog.body, "refresh token") {
		t.Fatalf("dialog = %s, want the refresh token question", describe(m.dialog))
	}
	if m.dialog.input.EchoMode == 0 {
		t.Fatal("the refresh token is echoed")
	}
	drain(t, m, m.dialog.action("sk-ant-ort01-refresh"), 0)

	if m.dialog == nil || !strings.Contains(m.dialog.body, "exchanged for a new access token") {
		t.Fatalf("dialog = %s, want the token URL question", describe(m.dialog))
	}
	drain(t, m, m.dialog.action("https://console.anthropic.com/v1/oauth/token"), 0)
	drain(t, m, m.dialog.action("client-123"), 0)
	drain(t, m, m.dialog.action("user:profile user:inference"), 0)

	if len(ds.createdSecrets) != 1 {
		t.Fatalf("created = %#v, want one", ds.createdSecrets)
	}
	got := ds.createdSecrets[0]
	if got.Type != "oauth" || got.Value != "sk-ant-oat01-access" || got.RefreshToken != "sk-ant-ort01-refresh" {
		t.Fatalf("stored = %#v, want the two tokens on an oauth secret", got)
	}
	if got.TokenURL != "https://console.anthropic.com/v1/oauth/token" || got.ClientID != "client-123" {
		t.Fatalf("stored = %#v, want where and as whom it renews", got)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "user:profile" {
		t.Fatalf("stored = %#v, want the scopes as they were typed", got)
	}
}

// Without a refresh token there is nothing to renew with, so nothing is stored
// rather than an oauth secret the server would refuse.
func TestAnOAuthSecretWithoutRefreshMaterialStoresNothing(t *testing.T) {
	m, ds := secretsFixture(t)

	send(t, m, key("n"))
	drain(t, m, m.dialog.action("claude"), 0)
	drain(t, m, m.dialog.action("oauth"), 0)
	drain(t, m, m.dialog.action("api.anthropic.com"), 0)
	drain(t, m, m.dialog.action("sk-ant-oat01-access"), 0)
	drain(t, m, m.dialog.action("  "), 0)

	if len(ds.createdSecrets) != 0 {
		t.Fatalf("created = %#v, want nothing", ds.createdSecrets)
	}
}

// Opening an OAuth credential says what it is: where it renews, what the grant
// may do, when the access token goes stale — and none of it is the credential.
func TestOpeningAnOAuthSecretShowsWhatItIs(t *testing.T) {
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.projectSecrets = []Secret{{
		ID: "sec_oauth", Name: "claude", Type: "oauth", Host: "api.anthropic.com", MaxTTL: time.Hour,
		//nolint:gosec // A token endpoint, which is a URL rather than a credential.
		OAuth: &SecretOAuth{
			TokenURL:             "https://console.anthropic.com/v1/oauth/token",
			ClientID:             "client-123",
			Scopes:               []string{"user:profile", "user:inference"},
			SubscriptionType:     "max",
			AccessTokenExpiresAt: time.Now().Add(2 * time.Hour),
			Refreshable:          true,
		},
	}}
	ds.projectGrants = nil
	ds.mu.Unlock()
	send(t, m, key("r"))

	send(t, m, key("enter"))
	if m.dialog == nil {
		t.Fatal("enter opened nothing")
	}
	if m.dialog.title != "claude" {
		t.Fatalf("title = %q, want the secret", m.dialog.title)
	}
	for _, want := range []string{
		"oauth", "api.anthropic.com", "renews at", "console.anthropic.com/v1/oauth/token",
		"client-123", "user:profile", "max", "token stale",
	} {
		if !strings.Contains(m.dialog.body, want) {
			t.Fatalf("body = %q, want it to carry %q", m.dialog.body, want)
		}
	}
	// And it still offers what a secret's view offers.
	if len(m.dialog.items) != 1 || m.dialog.items[0].key != grantCreateItem {
		t.Fatalf("items = %#v, want the row that grants it", m.dialog.items)
	}
}

// An OAuth credential that cannot renew is the one worth saying so about: it
// will expire and stay expired.
func TestASecretThatCannotRenewSaysSo(t *testing.T) {
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.projectSecrets = []Secret{{
		ID: "sec_oauth", Name: "half", Type: "oauth", MaxTTL: time.Hour,
		//nolint:gosec // A token endpoint, which is a URL rather than a credential.
		OAuth: &SecretOAuth{TokenURL: "https://example.com/token"},
	}}
	ds.mu.Unlock()
	send(t, m, key("r"))
	send(t, m, key("enter"))

	if m.dialog == nil || !strings.Contains(m.dialog.body, "cannot renew itself") {
		t.Fatalf("body = %s, want it to say the credential cannot renew", describe(m.dialog))
	}
}

// The wire word is harnessConfig, because that is the resource. A person calls
// it the harness, and the window says what a person calls it — while still
// sending what the server expects.
func TestTheWindowSaysHarnessAndSendsHarnessConfig(t *testing.T) {
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.projectGrants = []Grant{{
		ID: "grant_h", SecretID: "sec_gh", Scope: "harnessConfig",
		ScopeKey: "harness_1", Host: "api.github.com",
	}}
	ds.mu.Unlock()
	send(t, m, key("r"))

	// Drawn on the grant row, and again when the grant is read.
	send(t, m, key("enter"))
	var row action
	for _, item := range m.dialog.items {
		if item.key == "grant_h" {
			row = item
		}
	}
	if !strings.Contains(row.label, "harness") || strings.Contains(row.label, "harnessConfig") {
		t.Fatalf("row = %q, want it to say harness", row.label)
	}
	drain(t, m, m.dialog.action("grant_h"), 0)
	if strings.Contains(m.dialog.body, "harnessConfig") {
		t.Fatalf("review = %q, want it to say harness", m.dialog.body)
	}
	if !strings.Contains(m.dialog.body, "harness") {
		t.Fatalf("review = %q, want the scope named", m.dialog.body)
	}

	// The picker says harness too, and still sends the resource's own word.
	send(t, m, key("esc"))
	send(t, m, key("esc"))
	send(t, m, key(grantCreateKey))
	var offered action
	for _, item := range m.dialog.items {
		if item.key == "harnessConfig" {
			offered = item
		}
	}
	if offered.label != "One harness" {
		t.Fatalf("offered = %q, want it to say harness", offered.label)
	}
	drain(t, m, m.dialog.action("harnessConfig"), 0)
	if m.dialog == nil || m.dialog.title != "Which harness?" {
		t.Fatalf("dialog = %s, want the harness picker", describe(m.dialog))
	}
	drain(t, m, m.dialog.action(testHarnesses()[0].ID), 0)
	drain(t, m, m.dialog.action("any"), 0)
	drain(t, m, m.dialog.action("api.github.com"), 0)
	drain(t, m, m.dialog.action("0"), 0)

	if len(ds.createdGrants) != 1 || ds.createdGrants[0].Scope != "harnessConfig" {
		t.Fatalf("created = %#v, want the server's own word on the wire", ds.createdGrants)
	}
}

// The limit is a ceiling on how long consent to a credential may last, and zero
// is the meaningful answer "no limit" rather than an empty field. Both have to
// be sayable in the window, and both have to read as what they are.
func TestTheGrantLimitIsEditedAndZeroMeansForever(t *testing.T) {
	m, ds := secretsFixture(t)

	send(t, m, key("e"))
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatal("e did not offer what a secret says about itself")
	}
	drain(t, m, m.dialog.action("ttl"), 0)
	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatal("picking the limit did not ask for one")
	}
	// It opens on what the secret has, and says what zero does.
	if m.dialog.input.Value() != "3600" {
		t.Fatalf("input = %q, want the secret's current limit in seconds", m.dialog.input.Value())
	}
	if !strings.Contains(m.dialog.body, "ceiling") || !strings.Contains(m.dialog.body, "0 lifts it") {
		t.Fatalf("question = %q, want it to say it is a ceiling and what 0 does", m.dialog.body)
	}

	drain(t, m, m.dialog.action("0"), 0)
	if len(ds.limited) != 1 || ds.limited[0] != "sec_gh=0" {
		t.Fatalf("limited = %v, want the limit lifted on the secret under the cursor", ds.limited)
	}
	// And reading the secret says so — not the row, which carries what tells
	// two credentials apart. "never", which is how a duration of zero is
	// otherwise drawn, would read as the opposite of what it means.
	if body := describeSecret(Secret{Name: "gh", Type: "token", MaxTTL: 0}, time.Now()); !strings.Contains(body, "forever") {
		t.Fatalf("reading an unlimited secret says %q, want it to say its grants may live forever", body)
	}
}

// The row is for picking one credential out of a list. A kind and a limit are
// facts about a credential rather than ways to tell two of them apart, and a
// row carrying every field is a row nobody reads.
func TestTheRowCarriesWhatTellsSecretsApartAndNoMore(t *testing.T) {
	m, _ := secretsFixture(t)

	body := strings.Join(frame(m), "\n")
	for _, want := range []string{"gh", "github.com", "OpenAI key", "any host", "2 grants"} {
		if !strings.Contains(body, want) {
			t.Fatalf("frame does not carry %q:\n%s", want, body)
		}
	}
	// Both fixture secrets are tokens capped at an hour, so neither word
	// belongs anywhere on the screen until a secret is opened.
	for _, gone := range []string{"bearer", "1h"} {
		if strings.Contains(body, gone) {
			t.Fatalf("frame still carries %q, which belongs in the secret, not the row:\n%s", gone, body)
		}
	}
}

// The two tables are one question — which of these credentials answers this
// request — so they sit together, with the leftover room below them both.
func TestTheRequestsTableSitsUnderTheSecrets(t *testing.T) {
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.requests = []CredentialRequest{{ID: "sreq_1", Host: "api.github.com", SandboxID: "sbx_one", Created: time.Now()}}
	ds.mu.Unlock()
	drain(t, m, m.loadCredentialRequests(), 0)

	rows := frame(m)
	secrets, requests := -1, -1
	for i, row := range rows {
		if strings.Contains(row, "Secrets") && secrets < 0 {
			secrets = i
		}
		if strings.Contains(row, "Requests") && requests < 0 {
			requests = i
		}
	}
	if secrets < 0 || requests < 0 {
		t.Fatalf("both titles should be on screen; secrets=%d requests=%d", secrets, requests)
	}
	// Two secrets, then two blank rows, then the lower table's title.
	if want := secrets + len(ds.projectSecrets) + 3; requests != want {
		t.Fatalf("requests title on row %d, want %d — two blank rows under the last secret:\n%s",
			requests, want, strings.Join(rows, "\n"))
	}
}

// A capped secret cannot be granted forever, and finding that out from a server
// refusal teaches the rule one rejection at a time. The question says the limit.
func TestTheGrantLifetimeQuestionNamesTheSecretsLimit(t *testing.T) {
	m, _ := secretsFixture(t)

	m.askForGrantTTL(NewGrant{SecretID: "sec_gh", TTLSeconds: 3600})
	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatal("no lifetime was asked for")
	}
	if !strings.Contains(m.dialog.body, "at most 1h") || !strings.Contains(m.dialog.body, "cannot be granted forever") {
		t.Fatalf("question = %q, want the secret's limit said before it is exceeded", m.dialog.body)
	}

	// A secret with no limit is the other half: forever is available, so the
	// question offers it.
	m.secrets.all[0].MaxTTL = 0
	m.askForGrantTTL(NewGrant{SecretID: "sec_gh"})
	if !strings.Contains(m.dialog.body, "0 never expires") {
		t.Fatalf("question = %q, want forever offered when nothing forbids it", m.dialog.body)
	}
}
