package tui

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// routesFixture is the secrets screen over one token carrying three routes and
// one carrying none.
func routesFixture(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.projectSecrets = []Secret{
		{ID: "sec_routed", Name: "copilot", Type: "token", MaxTTL: time.Hour, Routes: []secretroute.Route{
			{Hosts: []string{"githubcopilot.com"}},
			{Hosts: []string{"githubcopilot.com"}, Paths: []string{"/mcp"}, Blocked: true},
			{Hosts: []string{"github.com"}, Blocked: true},
		}},
		{ID: "sec_plain", Name: "github", Type: "token", MaxTTL: time.Hour},
	}
	ds.projectBindings = nil
	ds.mu.Unlock()
	send(t, m, keyPress("r"))
	return m, ds
}

// Opening a secret says where it may be sent and which of those are blocked.
func TestOpeningASecretShowsItsRoutes(t *testing.T) {
	t.Parallel()
	m, _ := routesFixture(t)

	send(t, m, keyPress("enter"))
	body := dialogText(m)
	for _, want := range []string{
		"githubcopilot.com · allowed",
		"githubcopilot.com/mcp · blocked",
		"github.com · blocked",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body = %q, want %q", body, want)
		}
	}
}

// Each route opens on its own row with an empty one after it; emptying a row
// drops that route and typing in the empty one adds another.
func TestRoutesAreEditedRowByRow(t *testing.T) {
	t.Parallel()
	m, ds := routesFixture(t)

	send(t, m, keyPress("e"))
	if got := formValue(t, m, secretRouteKey+"1"); got != "githubcopilot.com/mcp=blocked" {
		t.Fatalf("second route row = %q", got)
	}
	if got := formValue(t, m, secretRouteKey+"3"); got != "" {
		t.Fatalf("fourth route row = %q, want it empty for another", got)
	}
	clearRow(t, m, secretRouteKey+"0")
	clearRow(t, m, secretRouteKey+"2")
	typeInto(t, m, secretRouteKey+"3", "api.github.com/repos/ = blocked")
	drain(t, m, submitForm(t, m), 0)
	want := []secretroute.Route{
		{Hosts: []string{"githubcopilot.com"}, Paths: []string{"/mcp"}, Blocked: true},
		{Hosts: []string{"api.github.com"}, Paths: []string{"/repos"}, Blocked: true},
	}
	if len(ds.updated) != 1 || ds.updated[0].Routes == nil || !reflect.DeepEqual(*ds.updated[0].Routes, want) {
		t.Fatalf("updated = %#v, want routes %#v", ds.updated, want)
	}
}

// Emptying every route row clears them, said as an empty list rather than left
// out; an untouched card sends none.
func TestRoutesAreClearedAndAnUntouchedCardSendsNone(t *testing.T) {
	t.Parallel()
	m, ds := routesFixture(t)

	send(t, m, keyPress("down"), keyPress("e"))
	typeInto(t, m, "token", "ghp_rotated")
	drain(t, m, submitForm(t, m), 0)
	if len(ds.updated) != 1 || ds.updated[0].Routes != nil {
		t.Fatalf("updated = %#v, want the routes left alone", ds.updated)
	}

	send(t, m, keyPress("up"), keyPress("e"))
	for i := range 3 {
		clearRow(t, m, secretRouteKey+string(rune('0'+i)))
	}
	drain(t, m, submitForm(t, m), 0)
	if len(ds.updated) != 2 || ds.updated[1].Routes == nil || len(*ds.updated[1].Routes) != 0 {
		t.Fatalf("updated = %#v, want the routes cleared", ds.updated)
	}
}

// A row that is not a route is refused on the card, naming it, and nothing is
// sent: a use after "=" is no longer one.
func TestAMalformedRouteKeepsTheCardUp(t *testing.T) {
	t.Parallel()
	m, ds := routesFixture(t)

	send(t, m, keyPress("e"))
	typeInto(t, m, secretRouteKey+"3", "github.com/a=Read-only queries")
	send(t, m, keyPress("ctrl+s"))
	if m.dialog == nil || m.dialog.kind != dlgForm {
		t.Fatalf("dialog = %s, want the card kept up", describe(m.dialog))
	}
	if !strings.Contains(m.dialog.form.err, "github.com/a=Read-only queries") {
		t.Fatalf("err = %q, want the row named", m.dialog.form.err)
	}
	if len(ds.updated) != 0 {
		t.Fatalf("updated = %#v, want nothing sent", ds.updated)
	}
}

// A new secret may be stored with a route, in the whole typed syntax: methods,
// several hosts, paths and a protocol block.
func TestANewSecretTakesARoute(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("n"))
	typeInto(t, m, "name", "github")
	typeInto(t, m, "token", "ghp_example")
	typeInto(t, m, secretRouteKey+"0", "POST api.github.com,*.github.com/graphql graphql=mutation:createPullRequest")
	drain(t, m, submitForm(t, m), 0)
	want := []secretroute.Route{{
		Hosts:   []string{"*.github.com", "api.github.com"},
		Methods: []string{"POST"},
		Paths:   []string{"/graphql"},
		GraphQL: &secretroute.GraphQL{Operations: []string{"mutation"}, Fields: []string{"createPullRequest"}},
	}}
	if len(ds.createdSecrets) != 1 || !reflect.DeepEqual(ds.createdSecrets[0].Routes, want) {
		t.Fatalf("created = %#v, want the route sent with it", ds.createdSecrets)
	}
}
