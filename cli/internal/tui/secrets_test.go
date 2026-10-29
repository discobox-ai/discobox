package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/pkg/secretroute"
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
		{ID: "sec_gh", Name: "gh", Type: "bearer", Routes: []secretroute.Route{{Hosts: []string{"github.com"}}}, MaxTTL: time.Hour},
		{ID: "sec_loose", Name: "OpenAI key", Type: "bearer", MaxTTL: time.Hour},
	}
	ds.projectBindings = []SecretBinding{
		{ID: "binding_1", SecretID: "sec_gh", TargetKind: BindingSandbox, TargetID: "sbx_one", Use: true, EnvVar: "GH_TOKEN",
			Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}, CreatedBy: "user:alice", Created: time.Now().Add(-time.Minute)},
		{ID: "binding_2", SecretID: "sec_gh", TargetKind: BindingProject, Use: true, EnvVar: "GITHUB"},
	}
	m := newTestModel(t, ds)
	send(t, m, keyPress(secretsKey))
	if !m.secretsOpen {
		t.Fatal("the secrets screen did not open")
	}
	return m, ds
}

func TestTheScreenShowsWhatEachSecretIsAndWhatStandsOnIt(t *testing.T) {
	t.Parallel()
	m, _ := secretsFixture(t)

	body := strings.Join(frame(m), "\n")
	for _, want := range []string{"Secrets", "gh", "github.com", "2 bindings", "OpenAI key", "sent nowhere"} {
		if !strings.Contains(body, want) {
			t.Fatalf("frame does not carry %q:\n%s", want, body)
		}
	}
}

// Routes are the whole of what limits where a credential may travel, so a
// secret with none says it is sent nowhere rather than showing a blank —
// blank reads as "not loaded".
func TestASecretWithNoRoutesSaysSo(t *testing.T) {
	t.Parallel()
	m, _ := secretsFixture(t)
	m.secrets.moveTo(1)
	if s := m.secrets.current(); s == nil || s.ID != "sec_loose" {
		t.Fatalf("cursor = %#v, want the secret with no routes", m.secrets.current())
	}
	if !strings.Contains(strings.Join(frame(m), "\n"), "sent nowhere") {
		t.Fatal("a secret with no routes is drawn as a blank")
	}
}

func TestBindingsAreListedAndTakenBack(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("enter"))
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatal("enter did not list the bindings standing on the secret")
	}
	if len(m.dialog.items) != 3 {
		t.Fatalf("items = %d, want the secret's two bindings and the row that makes one", len(m.dialog.items))
	}
	if m.dialog.items[0].key != bindingCreateItem {
		t.Fatalf("first row = %q, want making one to lead", m.dialog.items[0].key)
	}
	// A binding says what it covers before it offers to be taken back.
	joined := m.dialog.items[1].label + " " + m.dialog.items[1].detail
	for _, want := range []string{"GH_TOKEN", "api.github.com", "discobox " + testSandboxes()[0].Name} {
		if !strings.Contains(joined, want) {
			t.Fatalf("row = %q, want it to carry %q", joined, want)
		}
	}

	// Enter reads; x takes back.
	drain(t, m, m.dialog.action("binding_1"), 0)
	if m.dialog == nil || m.dialog.title != "Binding" {
		t.Fatalf("enter did not read the binding: %s", describe(m.dialog))
	}
	for _, want := range []string{"api.github.com", "GH_TOKEN", "discobox " + testSandboxes()[0].Name, "user:alice"} {
		if !strings.Contains(dialogText(m), want) {
			t.Fatalf("review = %q, want it to carry %q", dialogText(m), want)
		}
	}
	// Esc comes back to the bindings, so the unbind key is right there.
	send(t, m, keyPress("esc"))
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %s, want the bindings back", describe(m.dialog))
	}
	drain(t, m, m.dialog.alt("binding_1"), 0)
	if m.dialog == nil || m.dialog.kind != dlgConfirm {
		t.Fatalf("x did not ask before unbinding: %s", describe(m.dialog))
	}
	if !m.dialog.defaultNo {
		t.Fatal("the costly answer is the default")
	}
	drain(t, m, m.dialog.action("yes"), 0)
	if len(ds.unbindings) != 1 || ds.unbindings[0] != "binding_1" {
		t.Fatalf("unbindings = %v, want the chosen binding", ds.unbindings)
	}
	// The screen re-reads rather than guessing what the server now holds.
	if got := m.secrets.bindingsFor("sec_gh"); len(got) != 1 {
		t.Fatalf("bindings = %#v, want the one taken back gone", got)
	}
}

// A lapsed binding carries nothing, and is gone at the server's next sweep,
// so it is neither counted nor listed in the meantime.
func TestALapsedBindingIsNeitherCountedNorListed(t *testing.T) {
	t.Parallel()
	m, _ := secretsFixture(t)
	m.secrets.setBindings(append(m.secrets.bindings, SecretBinding{
		ID: "binding_old", SecretID: "sec_gh", TargetKind: BindingSandbox, TargetID: "sbx_gone", Use: true, EnvVar: "OLD",
		Expires: time.Now().Add(-time.Hour),
	}))
	if body := frameText(m); !strings.Contains(body, "2 bindings") {
		t.Fatalf("the row counts a lapsed binding:\n%s", body)
	}

	send(t, m, keyPress("enter"))
	var keys []string
	for _, it := range m.dialog.items {
		keys = append(keys, it.key)
	}
	if want := []string{bindingCreateItem, "binding_1", "binding_2"}; !slices.Equal(keys, want) {
		t.Fatalf("rows = %v, want %v", keys, want)
	}
}

// A binding to give on reads the same as one its discobox uses in every field
// but one, so that one is said wherever the binding is: its row, its card,
// and the question before it is taken back.
func TestADelegateBindingSaysWhatItIs(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.projectBindings[0].Use, ds.projectBindings[0].Delegate, ds.projectBindings[0].EnvVar = false, true, ""
	ds.mu.Unlock()
	send(t, m, keyPress(secretsKey), keyPress(secretsKey))

	send(t, m, keyPress("enter"))
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatal("enter did not list the bindings standing on the secret")
	}
	if label := m.dialog.items[1].label; label != "given on" {
		t.Fatalf("row = %q, want it to say it is only given on", label)
	}

	drain(t, m, m.dialog.action("binding_1"), 0)
	if !strings.Contains(dialogText(m), "never uses it") || strings.Contains(dialogText(m), "as variable") {
		t.Fatalf("review = %q, want it to say what it is for and no variable", dialogText(m))
	}

	send(t, m, keyPress("esc"))
	drain(t, m, m.dialog.alt("binding_1"), 0)
	if !strings.Contains(dialogText(m), "can no longer be given on") || strings.Contains(dialogText(m), "stops resolving") {
		t.Fatalf("unbind = %q, want it to say what taking back a delegation stops", dialogText(m))
	}
}

func TestANewSecretIsNamedRoutedAndStoredMasked(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("n"))
	if m.dialog == nil || m.dialog.kind != dlgForm {
		t.Fatalf("n did not open the new-secret form: %s", describe(m.dialog))
	}
	typeInto(t, m, "name", "npm")
	typeInto(t, m, "token", "npm_typedbyahuman")
	// A secret with no routes is sent nowhere, so one is asked for.
	drain(t, m, submitForm(t, m), 0)
	if len(ds.createdSecrets) != 0 || !strings.Contains(dialogText(m), "* for anywhere") {
		t.Fatalf("created = %#v, card = %q; want a route asked for", ds.createdSecrets, dialogText(m))
	}
	// The row being answered says what it means, so what a route covers is
	// on screen while it is being typed rather than a question ago.
	typeInto(t, m, secretRouteKey+"0", "registry.npmjs.org")
	if !strings.Contains(dialogText(m), "* for any host") {
		t.Fatalf("the form does not say what a route may be: %q", dialogText(m))
	}
	// A value is never drawn back, on the card that takes it or anywhere else.
	onRow(t, m, "token")
	if strings.Contains(dialogText(m), "npm_typedbyahuman") {
		t.Fatalf("the token is drawn in the clear:\n%s", dialogText(m))
	}
	drain(t, m, submitForm(t, m), 0)

	if len(ds.createdSecrets) != 1 {
		t.Fatalf("created = %#v, want one", ds.createdSecrets)
	}
	got := ds.createdSecrets[0]
	if got.Name != "npm" || len(got.Routes) != 1 || got.Routes[0].Hosts[0] != "registry.npmjs.org" || got.Value.Token != "npm_typedbyahuman" {
		t.Fatalf("stored = %#v, want what was typed", got)
	}
}

// The rows an answer makes irrelevant are not asked — but they stay on the
// card, saying so. A form whose fields appeared on choosing a kind would be
// four questions nobody knew were coming, which is the run of dialogs this
// replaced.
func TestTheOAuthRowsAreShownBeforeTheyAreAsked(t *testing.T) {
	t.Parallel()
	m, _ := secretsFixture(t)

	send(t, m, keyPress("n"))
	if rowShown(m, "refresh") {
		t.Fatal("a token is being asked to renew itself")
	}
	for _, want := range []string{"refresh token", "only for an OAuth credential"} {
		if !strings.Contains(dialogText(m), want) {
			t.Fatalf("the card does not say what an oauth credential will want (%q):\n%s", want, dialogText(m))
		}
	}
	onRow(t, m, "kind")
	m.dialog.form.cycle(1)
	for _, row := range []string{"access", "refresh", "tokenURL"} {
		if !rowShown(m, row) {
			t.Fatalf("the %s row is not asked for an oauth credential", row)
		}
	}
	if rowShown(m, "token") {
		t.Fatal("the plain token row is still asked")
	}
}

// A form that is not answered stays up with the reason on it: closing it would
// throw away everything already typed.
func TestARequiredRowRefusesTheFormRatherThanClosingIt(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("n"))
	send(t, m, keyPress("enter"))
	if m.dialog == nil || m.dialog.kind != dlgForm {
		t.Fatalf("the form went away unanswered: %s", describe(m.dialog))
	}
	if !strings.Contains(dialogText(m), "a secret needs a name") {
		t.Fatalf("the form does not say what it is waiting for:\n%s", dialogText(m))
	}
	if len(ds.createdSecrets) != 0 {
		t.Fatalf("created = %#v, want nothing stored", ds.createdSecrets)
	}
}

func TestTheRoutesCanBeEditedAndCleared(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("e"))
	if m.dialog == nil || m.dialog.kind != dlgForm {
		t.Fatalf("e did not offer what a secret says about itself: %s", describe(m.dialog))
	}
	// It opens on what the secret has, so editing is editing rather than
	// retyping.
	if got := formValue(t, m, secretRouteKey+"0"); got != "github.com" {
		t.Fatalf("route row = %q, want the current route", got)
	}
	clearRow(t, m, secretRouteKey+"0")
	drain(t, m, submitForm(t, m), 0)
	if len(ds.updated) != 1 || ds.updated[0].Routes == nil || len(*ds.updated[0].Routes) != 0 {
		t.Fatalf("updated = %#v, want the routes cleared", ds.updated)
	}
	// Only what changed is written back: the limit was not touched, so the
	// server was not told about it.
	if len(ds.limited) != 0 {
		t.Fatalf("limited = %v, want the untouched limit left alone", ds.limited)
	}
}

// A value cannot be read back, but it can be replaced. A card that would not
// take one made rotating a token a matter of deleting the secret and storing it
// again, which takes every grant standing on it along with it.
func TestASecretsValueIsReplacedFromTheSameCard(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("e"))
	if !rowShown(m, "token") {
		t.Fatal("the value cannot be replaced from the card that edits the secret")
	}
	// Leaving it empty is the ordinary answer: the stored value stays, and
	// nothing about the credential is written.
	if !strings.Contains(dialogText(m), "unchanged — type to replace it") {
		t.Fatalf("the row does not say what leaving it empty means:\n%s", dialogText(m))
	}
	send(t, m, keyPress("enter"))
	if len(ds.revalued) != 0 {
		t.Fatalf("revalued = %v, want an untouched value left alone", ds.revalued)
	}

	send(t, m, keyPress("e"))
	typeInto(t, m, "token", "ghp_rotated")
	send(t, m, keyPress("enter"))
	if len(ds.updated) != 1 || ds.updated[0].Value == nil {
		t.Fatalf("updated = %#v, want the new value sent", ds.updated)
	}
	if got := ds.updated[0].Value.Token; got != "ghp_rotated" {
		t.Fatalf("value = %q, want what was typed", got)
	}
	// One call, so a card cannot half-apply: everything it changed goes
	// together.
	if ds.updated[0].Name != nil || ds.updated[0].Routes != nil {
		t.Fatalf("update = %#v, want only what changed", ds.updated[0])
	}
}

// An oauth credential is one value, replaced whole: the server stores no half
// of one, so a new refresh token without the access token beside it would drop
// the rest. The card refuses rather than sending it, and says which part it
// cannot supply for you.
func TestReplacingAnOAuthValueTakesAllOfIt(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.projectSecrets = []Secret{{
		ID: "sec_oauth", Name: "claude", Type: "oauth", Routes: []secretroute.Route{{Hosts: []string{"api.anthropic.com"}}},
		//nolint:gosec // A token endpoint, which is a URL rather than a credential.
		OAuth: &SecretOAuth{TokenURL: "https://console.anthropic.com/v1/oauth/token"},
	}}
	ds.mu.Unlock()
	send(t, m, keyPress("r"))
	send(t, m, keyPress("e"))

	typeInto(t, m, "refresh", "sk-ant-ort01-new")
	send(t, m, keyPress("enter"))
	if len(ds.updated) != 0 {
		t.Fatalf("updated = %#v, want half a credential refused", ds.updated)
	}
	if !strings.Contains(dialogText(m), "access token too") {
		t.Fatalf("the card does not say what else it needs:\n%s", dialogText(m))
	}

	typeInto(t, m, "access", "sk-ant-oat01-new")
	send(t, m, keyPress("enter"))
	if len(ds.updated) != 1 || ds.updated[0].Value == nil {
		t.Fatalf("updated = %#v, want the whole credential stored", ds.updated)
	}
	got := *ds.updated[0].Value
	if got.Token != "sk-ant-oat01-new" || got.RefreshToken != "sk-ant-ort01-new" {
		t.Fatalf("value = %#v, want both tokens", got)
	}
	// The renewal fields it did not touch travel with it, because the value is
	// replaced whole and they are part of it.
	if got.TokenURL != "https://console.anthropic.com/v1/oauth/token" {
		t.Fatalf("value = %#v, want where it renews carried through", got)
	}
}

// The kind is the one thing an existing credential cannot be told: a token and
// an oauth credential are stored and renewed differently, and that is a new
// credential rather than an edit.
func TestTheKindOfAStoredCredentialCannotBeChanged(t *testing.T) {
	t.Parallel()
	m, _ := secretsFixture(t)

	send(t, m, keyPress("e"))
	if rowShown(m, "kind") {
		t.Fatal("an existing credential is offered a change of kind")
	}
	if !strings.Contains(dialogText(m), "that is a new credential rather than an edit") {
		t.Fatalf("the card does not say why:\n%s", dialogText(m))
	}
}

// Deleting takes the grants with it, so the question says so and Enter is not
// the answer.
func TestDeletingAsksFirstAndSaysWhatGoesWithIt(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("d"))
	if m.dialog == nil || m.dialog.kind != dlgConfirm {
		t.Fatal("d deleted without asking")
	}
	if !m.dialog.defaultNo {
		t.Fatal("the costly answer is the default")
	}
	if !strings.Contains(dialogText(m), "live bindings  2") {
		t.Fatalf("question = %q, want it to say what goes with the secret", dialogText(m))
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
	t.Parallel()
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
	send(t, m, keyPress(credentialsKey))
	// Reading the secrets comes first, then the question itself.
	drain(t, m, m.loadCredentialRequests(), 0)
	if m.dialog == nil {
		t.Fatal("the key did not open the waiting request")
	}
}

func TestTheScreenIsAToggleAndEscapeLeavesIt(t *testing.T) {
	t.Parallel()
	m, _ := secretsFixture(t)
	send(t, m, keyPress(secretsKey))
	if m.secretsOpen {
		t.Fatal("the key that opened the screen did not put it away")
	}
	send(t, m, keyPress(secretsKey), keyPress("esc"))
	if m.secretsOpen {
		t.Fatal("esc did not leave the screen")
	}
}

// A request no discobox owns — a value the control plane found stale with no
// discobox in the middle of using it — is exactly what this screen is for:
// there is no row to mark and no workspace to raise it in, so if it is not
// counted here it is invisible everywhere.
func TestTheScreenAnswersARequestNoDiscoboxOwns(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)
	orphan := waitingRequest()
	orphan.ID, orphan.SandboxID = "sreq_orphan", ""
	orphan.Refresh = &RefreshAsk{SecretID: "sec_gh", Cause: "stale"}
	ds.mu.Lock()
	ds.requests = []CredentialRequest{orphan}
	ds.mu.Unlock()
	send(t, m, tickMsg{})

	body := strings.Join(frame(m), "\n")
	if !strings.Contains(body, "1 waiting") || !strings.Contains(body, "control plane") {
		t.Fatalf("a request with no discobox is not shown on the operator's screen:\n%s", body)
	}
	// And no row is marked for it, since there is no row it belongs to.
	for _, s := range m.list.all {
		if s.PendingRequests != 0 {
			t.Fatalf("%s is marked for a request no discobox owns", s.ID)
		}
	}

	send(t, m, keyPress(credentialsKey))
	drain(t, m, m.loadCredentialRequests(), 0)
	if m.dialog == nil {
		t.Fatal("the key did not open the waiting request")
	}
	if m.dialog.title != "Refresh request" {
		t.Fatalf("dialog = %q, want the orphan request", dialogText(m))
	}
}

// Driven by keys rather than by calling the form's submit directly: the form
// takes typing, moves between its rows and is answered by Enter, and a card
// that could only be answered from a test is not one anybody can use.
func TestTheNewSecretFormIsAnsweredWithTheKeyboard(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("n"))
	if m.dialog == nil || m.dialog.kind != dlgForm {
		t.Fatalf("dialog = %s, want the new-secret form", describe(m.dialog))
	}
	send(t, m, typeString("npm")...)
	send(t, m, keyPress("down"), keyPress("down")) // past the kind, onto where it may be sent
	send(t, m, typeString("registry.npmjs.org")...)
	send(t, m, keyPress("down")) // the lifetime, on its default
	send(t, m, keyPress("down")) // past it to how the value is given, on "enter a value"
	send(t, m, keyPress("down")) // and to the value
	send(t, m, typeString("npm_typedbyahuman")...)
	send(t, m, keyPress("enter"))

	if m.dialog != nil {
		t.Fatalf("dialog = %s, want the form gone once it was answered", describe(m.dialog))
	}
	if len(ds.createdSecrets) != 1 {
		t.Fatalf("created = %#v, want one", ds.createdSecrets)
	}
	got := ds.createdSecrets[0]
	if got.Name != "npm" || got.Type != "token" || len(got.Routes) != 1 || got.Routes[0].Hosts[0] != "registry.npmjs.org" || got.Value.Token != "npm_typedbyahuman" {
		t.Fatalf("stored = %#v, want what was typed", got)
	}
	// The limit is set as the credential is stored rather than left to a second
	// visit — and zero is sent rather than left out, because zero is the answer
	// "no limit" and omitting it would take the server's default instead.
	if got.MaxTTLSeconds != 0 {
		t.Fatalf("limit = %d seconds, want no limit unless one is chosen", got.MaxTTLSeconds)
	}
}

// The value row is pasted into, because nobody types a credential. A terminal
// reports a bracketed paste as a message of its own rather than as the keys it
// would have taken to type, so a window that only handled key presses dropped
// it on the floor — the card sat there taking nothing, and the pasted token
// went into the composer behind it, which is not even on screen.
func TestATokenIsPastedIntoTheSecretItAnswers(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("n"))
	send(t, m, typeString("npm")...)
	send(t, m, keyPress("down"), keyPress("down"))
	send(t, m, tea.PasteMsg{Content: "registry.npmjs.org"})
	send(t, m, keyPress("down"), keyPress("down"), keyPress("down"))

	// Long, because a credential is as long as whoever issued it made it: a
	// character limit on the field would store the front of this and fail
	// later as a wrong password.
	token := "npm_" + strings.Repeat("s3cr3t", 60)
	send(t, m, tea.PasteMsg{Content: token})
	send(t, m, keyPress("enter"))

	if len(ds.createdSecrets) != 1 {
		t.Fatalf("created = %#v, want the secret the pasted answers describe", ds.createdSecrets)
	}
	got := ds.createdSecrets[0]
	if len(got.Routes) != 1 || got.Routes[0].Hosts[0] != "registry.npmjs.org" || got.Value.Token != token {
		t.Fatalf("created = {Routes: %v, Value: %q (%d chars)}, want the pasted route and the whole pasted token (%d chars)",
			got.Routes, got.Value.Token, len(got.Value.Token), len(token))
	}
	if v := m.prompt.Value(); v != "" {
		t.Fatalf("the composer behind the dialog took the paste: %q", v)
	}
}

// A binding made from this screen is one an operator makes because they
// already know the answer, rather than one that answers a request. It is one
// card: what it is on, what for, as which variable, and where and for how long
// — every key picked out of what the window already holds, so nothing here is
// an ID somebody has to type correctly.
func TestABindingIsOneFormWithEveryAnswerOnIt(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress(bindingCreateKey))
	if m.dialog == nil || m.dialog.kind != dlgForm {
		t.Fatalf("dialog = %s, want the binding form", describe(m.dialog))
	}
	// The narrowest target, which is the one that needs a discobox chosen.
	// The row for it is not asked until then.
	if rowShown(m, "sandbox") {
		t.Fatal("the discobox row is asked before the target calls for it")
	}
	onRow(t, m, "target")
	for m.dialog.form.chosen("target") != BindingSandbox {
		send(t, m, keyPress("right"))
	}
	if !rowShown(m, "sandbox") {
		t.Fatal("a binding to one discobox does not ask which discobox")
	}
	onRow(t, m, "sandbox")
	if got := m.dialog.form.chosen("sandbox"); got != testSandboxes()[0].ID {
		t.Fatalf("picker = %q, want the project's discoboxes", got)
	}

	// The form opens on a binding to use it, in the variable named for the
	// secret.
	if got := m.dialog.form.chosen("purpose"); got != "use" {
		t.Fatalf("purpose = %q, want a binding to use it by default", got)
	}
	if got := formValue(t, m, "envVar"); got != "GH" {
		t.Fatalf("variable = %q, want the secret's name upper-snaked", got)
	}
	if got := m.dialog.form.chosenLabel("ttl"); got != "1 hour" {
		t.Fatalf("lifetime = %q, want the secret's own limit as the default", got)
	}
	clearRow(t, m, "envVar")
	send(t, m, typeString("GH_TOKEN")...)
	typeInto(t, m, secretRouteKey+"0", "api.github.com")
	customTTL(t, m, "ttl", "900")
	send(t, m, keyPress("enter"))

	if len(ds.createdBindings) != 1 {
		t.Fatalf("created = %#v, want one binding", ds.createdBindings)
	}
	got := ds.createdBindings[0]
	if got.SecretID != "sec_gh" || got.TargetKind != BindingSandbox || got.TargetID != testSandboxes()[0].ID {
		t.Fatalf("binding = %#v, want it on the chosen discobox", got)
	}
	if got.EnvVar != "GH_TOKEN" || len(got.Routes) != 1 || got.Routes[0].Hosts[0] != "api.github.com" || got.TTLSeconds != 900 {
		t.Fatalf("binding = %#v, want the variable, route and lifetime that were answered", got)
	}
	if !got.Use || got.Delegate {
		t.Fatalf("binding = %#v, want a binding to use it", got)
	}
}

// A binding only to give on is in no variable, so the variable is not asked.
func TestABindingToGiveOnAsksNoVariable(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress(bindingCreateKey))
	if !rowShown(m, "envVar") {
		t.Fatal("a binding to use it does not ask its variable")
	}
	onRow(t, m, "purpose")
	send(t, m, keyPress("right"))
	if m.dialog.form.chosen("purpose") != "delegate" || rowShown(m, "envVar") {
		t.Fatal("a binding only to give on is asked its variable")
	}
	send(t, m, keyPress("ctrl+s"))

	if len(ds.createdBindings) != 1 || !ds.createdBindings[0].Delegate || ds.createdBindings[0].Use || ds.createdBindings[0].EnvVar != "" {
		t.Fatalf("created = %#v, want one binding only to give on", ds.createdBindings)
	}
}

// The card holds still as the cursor goes down it. The line explaining a row
// wraps to as many rows as the sentence needs, and a dialog is drawn from its
// content, so without a fixed allowance the window grew and shrank under the
// cursor — moving every row somebody was reading.
func TestTheFormIsTheSameHeightOnEveryRow(t *testing.T) {
	t.Parallel()
	m, _ := secretsFixture(t)

	for _, open := range []struct {
		what string
		key  string
	}{
		{"the new-secret card", "n"},
		{"the edit card", "e"},
		{"the binding card", bindingCreateKey},
	} {
		for _, width := range []int{60, 80, 120} {
			send(t, m, keyPress(open.key))
			f := m.dialog.form
			rows, on := 0, ""
			for i := range f.rows {
				if !f.answerable(i) {
					continue
				}
				f.cursor = i
				f.focus()
				height := len(strings.Split(m.dialog.view(m.st, &m.zones, width, 60), "\n"))
				if rows == 0 {
					rows, on = height, f.rows[i].key
					continue
				}
				if height != rows {
					t.Fatalf("%s at %d columns: %d rows on %q, %d on %q",
						open.what, width, rows, on, height, f.rows[i].key)
				}
			}
			send(t, m, keyPress("esc"))
		}
	}
}

// chooseTTL puts a lifetime picker on one of its presets.
func chooseTTL(t *testing.T, m *Model, row, preset string) {
	t.Helper()
	onRow(t, m, row)
	for i := 0; m.dialog.form.chosen(row) != preset; i++ {
		if i > len(m.dialog.form.rows)+8 {
			t.Fatalf("the %s picker has no %q", row, preset)
		}
		send(t, m, keyPress("right"))
	}
}

// customTTL takes the picker to custom and types a lifetime into the row that
// appears behind it.
func customTTL(t *testing.T, m *Model, row, howLong string) {
	t.Helper()
	chooseTTL(t, m, row, ttlCustom)
	clearRow(t, m, row+"Custom")
	send(t, m, typeString(howLong)...)
}

// rowShown is whether the form is taking an answer for a row. A row it is not
// is still on the card, saying why.
func rowShown(m *Model, key string) bool {
	for i, row := range m.dialog.form.rows {
		if row.key == key {
			return m.dialog.form.answerable(i)
		}
	}
	return false
}

// A lifetime that is not a number binds nothing, rather than quietly meaning
// something else — and the form stays up saying so, with what was typed on it.
func TestAnUnreadableLifetimeBindsNothing(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress(bindingCreateKey))
	customTTL(t, m, "ttl", "a while")
	send(t, m, keyPress("enter"))

	if len(ds.createdBindings) != 0 {
		t.Fatalf("created = %#v, want nothing", ds.createdBindings)
	}
	if m.dialog == nil || m.dialog.kind != dlgForm {
		t.Fatalf("dialog = %s, want the form still up", describe(m.dialog))
	}
	if !strings.Contains(dialogText(m), "a lifetime is 1h, 90m, 3d, 2w, 1mo, or forever") {
		t.Fatalf("the form does not say what it could not read:\n%s", dialogText(m))
	}
}

// Reading a binding and coming back lands on the bindings, not on the list
// behind them: the next move after reading one is reading the next, or taking
// it back.
func TestLeavingABindingReturnsToTheBindings(t *testing.T) {
	t.Parallel()
	m, _ := secretsFixture(t)

	send(t, m, keyPress("enter"))
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %s, want the bindings", describe(m.dialog))
	}
	drain(t, m, m.dialog.action("binding_1"), 0)
	if m.dialog == nil || m.dialog.title != "Binding" {
		t.Fatalf("dialog = %s, want the binding being read", describe(m.dialog))
	}

	drain(t, m, m.dialog.onCancel(), 0)
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %s, want the bindings back", describe(m.dialog))
	}
	if m.dialog.title != "gh" {
		t.Fatalf("title = %q, want the secret it was opened from", m.dialog.title)
	}
}

// Declining an unbind leaves you on the bindings, not on the screen behind
// them.
func TestDecliningAnUnbindReturnsToTheBindings(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("enter"))
	drain(t, m, m.dialog.alt("binding_1"), 0)
	if m.dialog == nil || m.dialog.kind != dlgConfirm {
		t.Fatalf("dialog = %s, want the question", describe(m.dialog))
	}
	drain(t, m, m.dialog.onCancel(), 0)

	if m.dialog == nil || m.dialog.kind != dlgActions || m.dialog.title != "gh" {
		t.Fatalf("dialog = %s, want the secret back", describe(m.dialog))
	}
	if len(ds.unbindings) != 0 {
		t.Fatal("declining took something back")
	}
}

// The two tables are one screen read downwards: off the bottom of the secrets
// is what is waiting, and off the top of that is the secrets again — the same
// way the discobox list and the prompt hand focus to each other.
func TestTheTablesHandFocusToEachOther(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.requests = []CredentialRequest{waitingRequest()}
	ds.mu.Unlock()
	send(t, m, tickMsg{})

	if m.table == tableRequests {
		t.Fatal("the screen opens on the requests")
	}
	send(t, m, keyPress("end"))
	send(t, m, keyPress("down"))
	if m.table != tableRequests {
		t.Fatal("down past the last secret did not reach the requests")
	}
	send(t, m, keyPress("up"))
	if m.table == tableRequests {
		t.Fatal("up past the first request did not return to the secrets")
	}
	// And Tab crosses either way, wherever the cursor is.
	send(t, m, keyPress("tab"))
	if m.table != tableRequests {
		t.Fatal("tab did not cross to the requests")
	}
	send(t, m, keyPress("tab"))
	if m.table == tableRequests {
		t.Fatal("tab did not cross back")
	}
	// Tab does not leave the screen; esc does.
	if !m.secretsOpen {
		t.Fatal("tab closed the screen")
	}
	send(t, m, keyPress("esc"))
	if m.secretsOpen {
		t.Fatal("esc did not leave the screen")
	}
}

// Enter on a request opens the same question the row mark and the workspace
// banner open, for that request rather than the oldest one.
func TestEnterOnARequestAnswersThatOne(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)
	older, newer := waitingRequest(), waitingRequest()
	older.ID, older.Created = "sreq_older", older.Created.Add(-time.Hour)
	newer.ID, newer.Name, newer.Routes = "sreq_newer", "npm", []secretroute.Route{{Hosts: []string{"registry.npmjs.org"}}}
	ds.mu.Lock()
	ds.requests = []CredentialRequest{older, newer}
	ds.mu.Unlock()
	send(t, m, tickMsg{})

	send(t, m, keyPress("tab"))
	if m.table != tableRequests || len(m.requestRows.all) != 2 {
		t.Fatalf("requests = %d, want both", len(m.requestRows.all))
	}
	// The second row, which is not the one C would have picked.
	send(t, m, keyPress("down"))
	chosen := m.requestRows.current()
	if chosen == nil {
		t.Fatal("no request under the cursor")
	}
	send(t, m, keyPress("enter"))
	drain(t, m, m.loadCredentialRequests(), 0)
	if m.dialog == nil {
		t.Fatal("enter answered nothing")
	}
	if !strings.Contains(dialogText(m), chosen.where()) {
		t.Fatalf("dialog = %q, want the request under the cursor (%s)", dialogText(m), chosen.where())
	}
}

func TestBindingsCanBeMadeFromTheBindingsList(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.projectBindings = nil
	ds.mu.Unlock()
	send(t, m, keyPress("r"))

	send(t, m, keyPress("enter"))
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %s, want the bindings", describe(m.dialog))
	}
	if !strings.Contains(dialogText(m), "nothing is bound to it") {
		t.Fatalf("body = %q, want it to say the secret has none", dialogText(m))
	}
	if len(m.dialog.items) != 1 || m.dialog.items[0].key != bindingCreateItem {
		t.Fatalf("items = %#v, want the row that makes one", m.dialog.items)
	}
	drain(t, m, m.dialog.action(bindingCreateItem), 0)
	if m.dialog == nil || m.dialog.kind != dlgForm {
		t.Fatalf("dialog = %s, want the binding form", describe(m.dialog))
	}
}

// What a request row says it is about. The proxy's reactive path names
// nothing — all it saw was a sentinel it could not resolve — and a person's
// ask is not that.
func TestARequestRowSaysWhatItIsAbout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		req  CredentialRequest
		want string
	}{
		{"an agent names the credential", CredentialRequest{Name: "github", EnvVar: "GH_TOKEN", SandboxID: "sbx_1"}, "github"},
		{"an ask to delegate says so", CredentialRequest{Name: "github", EnvVar: "GH_TOKEN", SandboxID: "sbx_1", Delegate: true}, "github · to delegate"},
		{"failing that, the variable", CredentialRequest{EnvVar: "GH_TOKEN", SandboxID: "sbx_1"}, "GH_TOKEN"},
		{"a discobox asked for a credential it did not name", CredentialRequest{SandboxID: "sbx_1"}, "a credential"},
	} {
		if got := requestSubject(tc.req); got != tc.want {
			t.Errorf("%s: subject = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := requesterLabel(CredentialRequest{}); got != "control plane" {
		t.Errorf("requester = %q, want the control plane", got)
	}
	if got := requesterLabel(CredentialRequest{SandboxID: "sbx_abcdefghij"}); got != "agent · abcdefgh" {
		t.Errorf("requester = %q, want the agent and a short id", got)
	}
}

// An OAuth credential is more than one field, and all of them are asked for:
// the access token travels, and the rest is what the control plane spends to
// renew it when that token goes stale. Choosing the kind is what brings those
// rows onto the card.
func TestAnOAuthSecretIsRegisteredFromTheWindow(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("n"))
	typeInto(t, m, "name", "claude")
	onRow(t, m, "kind")
	send(t, m, keyPress("right"))
	typeInto(t, m, secretRouteKey+"0", "api.anthropic.com")
	typeInto(t, m, "access", "sk-ant-oat01-access")
	typeInto(t, m, "refresh", "sk-ant-ort01-refresh")
	typeInto(t, m, "tokenURL", "https://console.anthropic.com/v1/oauth/token")
	typeInto(t, m, "client", "client-123")
	typeInto(t, m, "scopes", "user:profile user:inference")
	// Neither token is ever drawn back, on the card that takes them or after.
	if text := dialogText(m); strings.Contains(text, "sk-ant-oat01-access") || strings.Contains(text, "sk-ant-ort01-refresh") {
		t.Fatalf("a token is echoed:\n%s", text)
	}
	send(t, m, keyPress("enter"))

	if len(ds.createdSecrets) != 1 {
		t.Fatalf("created = %#v, want one", ds.createdSecrets)
	}
	got := ds.createdSecrets[0]
	if got.Type != "oauth" || got.Value.Token != "sk-ant-oat01-access" || got.Value.RefreshToken != "sk-ant-ort01-refresh" {
		t.Fatalf("stored = %#v, want the two tokens on an oauth secret", got)
	}
	if got.Value.TokenURL != "https://console.anthropic.com/v1/oauth/token" || got.Value.ClientID != "client-123" {
		t.Fatalf("stored = %#v, want where and as whom it renews", got)
	}
	if len(got.Value.Scopes) != 2 || got.Value.Scopes[0] != "user:profile" {
		t.Fatalf("stored = %#v, want the scopes as they were typed", got)
	}
}

// Without a refresh token there is nothing to renew with, so nothing is stored
// rather than an oauth secret the server would refuse. The form says which row
// it is waiting on and keeps everything already typed.
func TestAnOAuthSecretWithoutRefreshMaterialStoresNothing(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("n"))
	typeInto(t, m, "name", "claude")
	onRow(t, m, "kind")
	send(t, m, keyPress("right"))
	typeInto(t, m, "access", "sk-ant-oat01-access")
	send(t, m, keyPress("enter"))

	if len(ds.createdSecrets) != 0 {
		t.Fatalf("created = %#v, want nothing", ds.createdSecrets)
	}
	if !strings.Contains(dialogText(m), "needs a refresh token") {
		t.Fatalf("the form does not say what is missing:\n%s", dialogText(m))
	}
	if m.dialog.form.value("access") != "sk-ant-oat01-access" {
		t.Fatal("the form threw away what was already typed")
	}
}

// Opening an OAuth credential says what it is: where it renews, what the grant
// may do, when the access token goes stale — and none of it is the credential.
func TestOpeningAnOAuthSecretShowsWhatItIs(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.projectSecrets = []Secret{{
		ID: "sec_oauth", Name: "claude", Type: "oauth", Routes: []secretroute.Route{{Hosts: []string{"api.anthropic.com"}}}, MaxTTL: time.Hour,
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
	ds.projectBindings = nil
	ds.mu.Unlock()
	send(t, m, keyPress("r"))

	send(t, m, keyPress("enter"))
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
		if !strings.Contains(dialogText(m), want) {
			t.Fatalf("body = %q, want it to carry %q", dialogText(m), want)
		}
	}
	// And it still offers what a secret's view offers.
	if len(m.dialog.items) != 1 || m.dialog.items[0].key != bindingCreateItem {
		t.Fatalf("items = %#v, want the row that binds it", m.dialog.items)
	}
}

// An OAuth credential that cannot renew is the one worth saying so about: it
// will expire and stay expired.
func TestASecretThatCannotRenewSaysSo(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.projectSecrets = []Secret{{
		ID: "sec_oauth", Name: "half", Type: "oauth", MaxTTL: time.Hour,
		//nolint:gosec // A token endpoint, which is a URL rather than a credential.
		OAuth: &SecretOAuth{TokenURL: "https://example.com/token"},
	}}
	ds.mu.Unlock()
	send(t, m, keyPress("r"))
	send(t, m, keyPress("enter"))

	if m.dialog == nil || !strings.Contains(dialogText(m), "cannot renew itself") {
		t.Fatalf("body = %s, want it to say the credential cannot renew", describe(m.dialog))
	}
}

// The wire word is harnessConfig, because that is the resource. A person calls
// it the harness, and the window says what a person calls it — while still
// sending what the server expects.
func TestTheWindowSaysHarnessAndSendsHarnessConfig(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.projectBindings = []SecretBinding{{
		ID: "binding_h", SecretID: "sec_gh", TargetKind: BindingHarnessConfig,
		TargetID: "harness_1", Use: true, EnvVar: "GH_TOKEN",
	}}
	ds.mu.Unlock()
	send(t, m, keyPress("r"))

	// Drawn on the binding row, and again when the binding is read.
	send(t, m, keyPress("enter"))
	var row action
	for _, item := range m.dialog.items {
		if item.key == "binding_h" {
			row = item
		}
	}
	if !strings.Contains(row.detail, "harness") || strings.Contains(row.detail, "harnessConfig") {
		t.Fatalf("row = %q, want it to say harness", row.detail)
	}
	drain(t, m, m.dialog.action("binding_h"), 0)
	if strings.Contains(dialogText(m), "harnessConfig") {
		t.Fatalf("review = %q, want it to say harness", dialogText(m))
	}
	if !strings.Contains(dialogText(m), "harness") {
		t.Fatalf("review = %q, want the target named", dialogText(m))
	}

	// The form says harness too, and still sends the resource's own word.
	send(t, m, keyPress("esc"))
	send(t, m, keyPress("esc"))
	send(t, m, keyPress(bindingCreateKey))
	onRow(t, m, "target")
	for m.dialog.form.chosen("target") != BindingHarnessConfig {
		send(t, m, keyPress("right"))
	}
	if got := m.dialog.form.chosenLabel("target"); got != "one harness" {
		t.Fatalf("offered = %q, want it to say harness", got)
	}
	if !rowShown(m, "harness") {
		t.Fatal("a harness binding does not ask which harness")
	}
	send(t, m, keyPress("ctrl+s"))

	if len(ds.createdBindings) != 1 || ds.createdBindings[0].TargetKind != BindingHarnessConfig {
		t.Fatalf("created = %#v, want the server's own word on the wire", ds.createdBindings)
	}
	if got := ds.createdBindings[0].TargetID; got != testHarnesses()[0].ID {
		t.Fatalf("target = %q, want the harness chosen on the form", got)
	}
}

// A limit that is not a preset is prefilled on the custom row in the words it
// reads back in, and those words have to be ones the row accepts: a secret
// capped at a second was refused on a field nobody touched, because "1 second"
// was read as a count of days ending in "d".
func TestAnOddLimitSurvivesAnEditThatDoesNotTouchIt(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)
	m.secrets.all[0].MaxTTL = time.Second

	send(t, m, keyPress("e"))
	if got := m.dialog.form.chosen("ttl"); got != ttlCustom {
		t.Fatalf("ttl row = %q, want a limit that is no preset on the custom row", got)
	}
	send(t, m, keyPress("enter"))
	if m.dialog != nil && m.dialog.kind == dlgForm {
		t.Fatalf("the form refused a limit nobody touched: %q", m.dialog.form.err)
	}
	// And it is not rewritten either: nothing about it changed.
	if len(ds.limited) != 0 {
		t.Fatalf("limited = %v, want the untouched limit left alone", ds.limited)
	}
}

// A limit that cannot be read says what one looks like — the words the row
// takes, not a count of seconds.
func TestAnUnreadableLimitSaysWhatOneLooksLike(t *testing.T) {
	t.Parallel()
	m, _ := secretsFixture(t)

	send(t, m, keyPress("e"))
	customTTL(t, m, "ttl", "a while")
	send(t, m, keyPress("enter"))
	if m.dialog == nil || m.dialog.kind != dlgForm {
		t.Fatalf("dialog = %s, want the form still up", describe(m.dialog))
	}
	if !strings.Contains(dialogText(m), "a limit is 1h, 90m, 3d, 2w, 1mo, or no limit") {
		t.Fatalf("the form does not say what a limit looks like:\n%s", dialogText(m))
	}
}

// The limit is a ceiling on how long consent to a credential may last, and zero
// is the meaningful answer "no limit" rather than an empty field. Both have to
// be sayable in the window, and both have to read as what they are.
func TestTheBindingLimitIsEditedAndZeroMeansForever(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("e"))
	if m.dialog == nil || m.dialog.kind != dlgForm {
		t.Fatalf("e did not offer what a secret says about itself: %s", describe(m.dialog))
	}
	// It opens on what the secret already limits its bindings to, said as the
	// preset it is rather than as a number of seconds.
	if got := m.dialog.form.chosenLabel("ttl"); got != "1 hour" {
		t.Fatalf("ttl row = %q, want the secret's current limit as a preset", got)
	}
	onRow(t, m, "ttl")
	// The row is a ceiling, and says so: "bindings last 1 hour" would state it
	// as the lifetime every binding of the credential gets.
	if !strings.Contains(dialogText(m), "binding limit") || !strings.Contains(dialogText(m), "the longest a binding of this credential may last") {
		t.Fatalf("form = %q, want the limit drawn as a ceiling", dialogText(m))
	}

	// Forever is one of the presets, because zero is a meaningful answer here
	// rather than an empty field.
	chooseTTL(t, m, "ttl", ttlNever)
	if got := m.dialog.form.chosenLabel("ttl"); got != "no limit" {
		t.Fatalf("zero reads as %q, want it to say there is no limit", got)
	}
	send(t, m, keyPress("ctrl+s"))
	if len(ds.limited) != 1 || ds.limited[0] != "sec_gh=0" {
		t.Fatalf("limited = %v, want the limit lifted on the secret under the cursor", ds.limited)
	}
	// Only what changed: the routes were not touched, so they were not
	// rewritten.
	if len(ds.updated) != 1 || ds.updated[0].Routes != nil {
		t.Fatalf("updated = %#v, want the untouched routes left alone", ds.updated)
	}
	// And reading the secret says so — not the row, which carries what tells
	// two credentials apart. "never", which is how a duration of zero is
	// otherwise drawn, would read as the opposite of what it means.
	m.secrets.all[0].MaxTTL = 0
	drain(t, m, m.showBindings(), 0)
	if !strings.Contains(dialogText(m), "forever") {
		t.Fatalf("reading an unlimited secret says %q, want it to say its bindings may last forever", dialogText(m))
	}
}

// The row is for picking one credential out of a list. A kind and a limit are
// facts about a credential rather than ways to tell two of them apart, and a
// row carrying every field is a row nobody reads.
func TestTheRowCarriesWhatTellsSecretsApartAndNoMore(t *testing.T) {
	t.Parallel()
	m, _ := secretsFixture(t)

	body := strings.Join(frame(m), "\n")
	for _, want := range []string{"gh", "github.com", "OpenAI key", "sent nowhere", "2 bindings"} {
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
	t.Parallel()
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

// A capped secret cannot be bound forever, and finding that out from a server
// refusal teaches the rule one rejection at a time. The form says the limit
// against the row it applies to.
func TestTheBindingLifetimeRowNamesTheSecretsLimit(t *testing.T) {
	t.Parallel()
	m, _ := secretsFixture(t)

	m.askForBinding()
	if m.dialog == nil || m.dialog.kind != dlgForm {
		t.Fatal("no binding form was opened")
	}
	onRow(t, m, "ttl")
	if text := dialogText(m); !strings.Contains(text, "at most 1 hour") || !strings.Contains(text, "cannot be bound forever") {
		t.Fatalf("form = %q, want the secret's limit said before it is exceeded", text)
	}

	// A secret with no limit is the other half: forever is available, so the
	// row offers it.
	m.secrets.all[0].MaxTTL = 0
	m.askForBinding()
	onRow(t, m, "ttl")
	if got := m.dialog.form.chosenLabel("ttl"); got != "never expires" {
		t.Fatalf("lifetime = %q, want forever offered when nothing forbids it", got)
	}
}

// onRow puts the form's cursor on a named row, which is what a hint is shown
// for.
func onRow(t *testing.T, m *Model, key string) {
	t.Helper()
	f := m.dialog.form
	for i, row := range f.rows {
		if row.key == key {
			f.cursor = i
			f.focus()
			return
		}
	}
	t.Fatalf("the form has no %q row", key)
}

// typeInto puts the cursor on a row and types a value into it, key by key, the
// way a person answers a form.
func typeInto(t *testing.T, m *Model, row, value string) {
	t.Helper()
	onRow(t, m, row)
	send(t, m, typeString(value)...)
}

// clearRow empties a row of what it opened with.
func clearRow(t *testing.T, m *Model, row string) {
	t.Helper()
	onRow(t, m, row)
	for range formValue(t, m, row) {
		send(t, m, keyPress("backspace"))
	}
}

func formValue(t *testing.T, m *Model, row string) string {
	t.Helper()
	return m.dialog.form.value(row)
}

// submitForm answers the whole card, the way Enter does.
func submitForm(t *testing.T, m *Model) tea.Cmd {
	t.Helper()
	if why := m.dialog.form.submit(); why != "" {
		t.Fatalf("the form refused: %s", why)
	}
	return m.dialog.submit(m.dialog.form)
}

// Requests that arrive while the secrets screen is open are drawn: the table
// grows to the rows it is allowed, without waiting for a key or the pointer to
// lay the window out again.
func TestRequestsArrivingOnTheSecretsScreenAreDrawn(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)
	requests := make([]CredentialRequest, 30)
	for i := range requests {
		r := waitingRequest()
		r.ID = fmt.Sprintf("sreq_%02d", i)
		r.Name = fmt.Sprintf("cred-%02d", i)
		r.Created = r.Created.Add(time.Duration(i) * time.Minute)
		requests[i] = r
	}
	ds.mu.Lock()
	ds.requests = requests
	ds.mu.Unlock()
	// The poll, and nothing else: no key, no mouse.
	send(t, m, tickMsg{})

	text := frameText(m)
	if !strings.Contains(text, "30 waiting") {
		t.Fatalf("the poll did not deliver the requests:\n%s", text)
	}
	// A third of a 40-row window's room is ten rows.
	want := 10
	if got := strings.Count(text, "cred-"); got != want {
		t.Fatalf("the requests table drew %d of 30 waiting requests, want %d:\n%s", got, want, text)
	}
}

// A binding with seconds left says so, rather than rounding to zero minutes
// and reading as one that never expires.
func TestABindingAboutToLapseDoesNotReadAsNever(t *testing.T) {
	t.Parallel()
	now := time.Now()
	b := SecretBinding{Expires: now.Add(20 * time.Second)}
	if got := bindingExpiry(b, now); got != "expires in 20s" {
		t.Fatalf("expiry = %q, want the seconds left", got)
	}
	if got := bindingLifeLeft(b, now); got != "in 20s" {
		t.Fatalf("life left = %q, want the seconds left", got)
	}
}

// The row's count is taken when it is drawn, so a binding that lapses between
// reads stops being counted without waiting for the next one.
func TestTheRowStopsCountingABindingWhenItLapses(t *testing.T) {
	t.Parallel()
	m, _ := secretsFixture(t)
	m.secrets.setBindings(append(m.secrets.bindings, SecretBinding{
		ID: "binding_short", SecretID: "sec_gh", TargetKind: BindingProject, Use: true, EnvVar: "SHORT",
		Expires: time.Now().Add(time.Minute),
	}))
	if body := frameText(m); !strings.Contains(body, "3 bindings") {
		t.Fatalf("the row does not count the live binding:\n%s", body)
	}
	m.secrets.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if body := frameText(m); !strings.Contains(body, "2 bindings") {
		t.Fatalf("the row still counts a lapsed binding:\n%s", body)
	}
}
