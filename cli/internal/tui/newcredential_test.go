package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/pkg/secretroute"
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

// Every field of the filled form stays the person's.
func TestTheFilledFormIsStillEditable(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	toNewCredential(t, m, time.Hour)
	f := m.dialog.form
	f.set("token", "ghp_typedbyahuman")
	f.set("name", "github-ci")
	send(t, m, keyPress("enter"))
	if len(ds.createdSecrets) != 1 || ds.createdSecrets[0].Name != "github-ci" || ds.createdSecrets[0].Value.Token != "ghp_typedbyahuman" ||
		ds.createdSecrets[0].RefreshCommand != nil {
		t.Fatalf("created = %#v, want what was typed", ds.createdSecrets)
	}
}

// A name the project already holds is offered as the
// first free one, which is what the server would otherwise refuse.
func TestATakenNameIsOfferedAsTheFirstFreeOne(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	ds.mu.Lock()
	ds.projectSecrets = append(ds.projectSecrets, Secret{ID: "sec_dupe", Name: "github", Type: "bearer", Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}})
	ds.mu.Unlock()
	toNewCredential(t, m, time.Hour)
	if got := m.dialog.form.value("name"); got != "github-2" {
		t.Fatalf("name = %q, want the first free one", got)
	}
}

// A request fills the form with the name, kind and routes it asks for; the token is typed, masked, and an empty one
// is refused on the form with the request still waiting.
func TestANewCredentialForAnyOtherAskIsFilledFromIt(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	toNewCredential(t, m, lifetime.Day)
	f := m.dialog.form
	if f.chosen("kind") != "token" || f.value("name") != "github" || f.value(secretRouteKey+"0") != "api.github.com" || f.chosen("source") != "value" {
		t.Fatalf("form = kind %q name %q route %q source %q, want the request's", f.chosen("kind"), f.value("name"), f.value(secretRouteKey+"0"), f.chosen("source"))
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
	if len(ds.createdSecrets) != 1 || ds.createdSecrets[0].Name != "github" || len(ds.createdSecrets[0].Routes) != 1 || ds.createdSecrets[0].Routes[0].Hosts[0] != "api.github.com" ||
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
	m, _ := sourceWithRequest(t)
	send(t, m, sizeMsg(90, 60))
	toNewCredential(t, m, time.Hour)
	if frame := plainFrame(m); !strings.Contains(frame, "enter stores and approves") || !strings.Contains(frame, "esc back, nothing stored") {
		t.Fatalf("key line is cut:\n%s", frame)
	}
}
