package tui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/cli/internal/requestrole"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// A credential request is answered by a guided dialog (issue #1017): which
// credential, a new one if none fits, a summary answering every part at
// once or a review answering each, using the lifetime selected up front.

// onRequestCard is whether the first step, which credential, is up.
func onRequestCard(m *Model) bool {
	return m.dialog != nil && m.dialog.title == "Credential request" && m.dialog.titleRight == "credential"
}

// onStep is whether the step whose corner reads name is up.
func onStep(m *Model, name string) bool {
	return m.dialog != nil && m.dialog.titleRight == name
}

// pickItem chooses the menu row keyed key on the card up, as a person would:
// the card drawn, the cursor on the row, Enter.
func pickItem(t *testing.T, m *Model, key string) {
	t.Helper()
	d := m.dialog
	if d == nil {
		t.Fatal("no card up")
	}
	plainFrame(m)
	for i, item := range d.items {
		if item.key == key {
			d.cursor = i
			send(t, m, keyPress("enter"))
			return
		}
	}
	t.Fatalf("no row %q on %s", key, describe(d))
}

// grantFor revises the first step when needed, then consents on the summary.
func grantFor(t *testing.T, m *Model, ttl time.Duration) {
	t.Helper()
	if ttl != time.Hour {
		send(t, m, keyPress("esc"))
		if !onRequestCard(m) {
			t.Fatal("not on credential selection")
		}
		key := m.dialog.items[m.dialog.cursor].key
		pickItem(t, m, "ttl")
		pickItem(t, m, strconv.FormatInt(lifetime.Seconds(ttl), 10))
		pickItem(t, m, key)
	}
	answerApproval(t, m, "accept")
}

// toSummary opens the request and answers step 1 with the secret keyed id.
func toSummary(t *testing.T, m *Model, id string) {
	t.Helper()
	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	if !onRequestCard(m) {
		t.Fatalf("dialog = %s, want the request card", describe(m.dialog))
	}
	pickItem(t, m, id)
}

// fakeGitHubToken has a classic GitHub token's shape, ghp_ and 36 of alnum,
// and is assembled so no literal in the tree looks like a credential.
var fakeGitHubToken = "ghp_" + strings.Repeat("A1b2", 9)

func ghRoute(paths ...string) secretroute.Route {
	return secretroute.Route{Hosts: []string{"api.github.com"}, Methods: []string{"GET"}, Paths: paths}
}

// Step 1 lists every secret, the ones that can answer first and highlighted,
// the rest marked ✗; choosing one of those says why rather than answer.
func TestStepOneMarksWhatCannotAnswerAndSaysWhy(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	ds.projectSecrets = append(ds.projectSecrets, Secret{ID: "sec_none", Name: "unrouted"})

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	if !onRequestCard(m) {
		t.Fatalf("dialog = %s, want step 1", describe(m.dialog))
	}
	var labels []string
	for _, item := range m.dialog.items {
		labels = append(labels, item.label)
	}
	want := []string{"  GitHub token", "✗ OpenAI key", "✗ unrouted", "  Enter new credential…", "For 1 hour · change…"}
	if strings.Join(labels, "|") != strings.Join(want, "|") {
		t.Fatalf("rows = %q, want %q", labels, want)
	}
	if m.dialog.items[m.dialog.cursor].key != "sec_gh" {
		t.Fatalf("cursor on %q, want the secret that can answer", m.dialog.items[m.dialog.cursor].key)
	}
	pickItem(t, m, "sec_openai")
	text := plainFrame(m)
	for _, want := range []string{"“OpenAI key” can't be used here", "requested, but not covered", "api.github.com"} {
		if !strings.Contains(text, want) {
			t.Fatalf("card = %q, want %q", text, want)
		}
	}
	if len(ds.approvals) != 0 {
		t.Fatal("an incompatible secret answered the request")
	}
	send(t, m, keyPress("esc"))
	if !onRequestCard(m) || m.dialog.items[m.dialog.cursor].key != "sec_openai" {
		t.Fatalf("dialog = %s, want step 1 back, on the row chosen", describe(m.dialog))
	}
	pickItem(t, m, "sec_none")
	if !strings.Contains(plainFrame(m), "has no routes, so discobox never sends it anywhere") {
		t.Fatalf("card = %q, want it to say a secret with no routes is sent nowhere", plainFrame(m))
	}
}

// With nothing that can answer, a new credential is where step 1 opens.
func TestStepOneOpensOnANewCredentialWhenNothingFits(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	ds.projectSecrets = ds.projectSecrets[1:]
	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	if got := m.dialog.items[m.dialog.cursor].key; got != "new" {
		t.Fatalf("cursor on %q, want a new credential", got)
	}
}

// Routes covered only in part are not covered: a secret for one of the hosts
// asked for cannot answer a request for both.
func TestASecretCoveringPartOfTheRequestCannotAnswer(t *testing.T) {
	t.Parallel()
	s := Secret{Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}}
	req := waitingRequest()
	req.Use.Routes = append(req.Use.Routes, secretroute.Route{Hosts: []string{"github.com"}, Paths: []string{"/o/r.git/**"}})
	got := uncovered(s, req)
	if len(got) != 1 || got[0].Hosts[0] != "github.com" {
		t.Fatalf("uncovered = %v, want the github.com route alone", got)
	}
	s.Routes = append(s.Routes, secretroute.Route{Hosts: []string{"github.com"}})
	if got := uncovered(s, req); got != nil {
		t.Fatalf("uncovered = %v, want none once both hosts are covered", got)
	}
	s.RouteConstraints = [][]secretroute.Route{{{Hosts: []string{"api.github.com"}}}}
	if got := uncovered(s, req); len(got) != 1 {
		t.Fatalf("uncovered = %v, want a constraint list to count as much as the routes", got)
	}
}

// The summary sends every part at once at the lifetime selected up front.
func TestTheSummaryAcceptsEveryPart(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	req := waitingRequest()
	req.Delegate = &RequestPart{Routes: []secretroute.Route{ghRoute("/repos/**")}}
	ds.requests = []CredentialRequest{req}
	send(t, m, tickMsg{})

	toSummary(t, m, "sec_gh")
	text := plainFrame(m)
	for _, want := range []string{"used by this box", "delegated to its workers", "[ Accept ]", "[ Deny ]", "[ Review… ]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("summary = %q, want %q", text, want)
		}
	}
	grantFor(t, m, lifetime.Day)
	if len(ds.approvals) != 1 {
		t.Fatalf("approvals = %#v, want one", ds.approvals)
	}
	got := ds.approvals[0]
	if got.Use != PartApprove || got.Delegate != PartApprove || got.UseScope != "routes" || got.DelegateScope != "routes" ||
		got.TTLSeconds != int64(lifetime.Day/time.Second) || got.SecretID != "sec_gh" || got.UseAs != "replace" {
		t.Fatalf("approval = %#v, want both parts on their routes for a day", got)
	}
}

// Deny on the summary denies the request; nothing is approved, and how long
// is never asked.
func TestTheSummaryDeniesEveryPart(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	toSummary(t, m, "sec_gh")
	answerApproval(t, m, "deny")
	if len(ds.denials) != 1 || len(ds.approvals) != 0 {
		t.Fatalf("denials = %v approvals = %v, want the request denied", ds.denials, ds.approvals)
	}
}

// The summary is each part in a line and lists no routes: each route, with
// its paths, is behind Review…, on its part's card (issue #1172).
func TestTheSummaryListsNoRoutes(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	req := waitingRequest()
	req.Use.Routes = []secretroute.Route{ghRoute("/user/repos"), {Hosts: []string{"github.com"}, Methods: []string{"POST"}, Paths: []string{"/o/r.git/git-receive-pack"}}}
	req.Delegate = &RequestPart{Routes: []secretroute.Route{ghRoute("/repos/o/r/pulls")}}
	ds.projectSecrets[0].Routes = append(ds.projectSecrets[0].Routes, secretroute.Route{Hosts: []string{"github.com"}})
	ds.requests = []CredentialRequest{req}
	send(t, m, tickMsg{})

	toSummary(t, m, "sec_gh")
	text := plainFrame(m)
	for _, want := range []string{"api.github.com, github.com · 2 operations · GET, POST", "api.github.com · 1 operation · GET"} {
		if !strings.Contains(text, want) {
			t.Fatalf("summary = %q, want %q", text, want)
		}
	}
	for _, route := range []string{"/user/repos", "git-receive-pack", "/repos/o/r/pulls", "── its own use", "── its workers"} {
		if strings.Contains(text, route) {
			t.Fatalf("summary = %q, lists %q: routes are Review…'s", text, route)
		}
	}
	answerApproval(t, m, "review")
	if text := plainFrame(m); !onStep(m, "its own use") || !strings.Contains(text, "/user/repos") || !strings.Contains(text, "git-receive-pack") {
		t.Fatalf("review = %q, want the use part's routes", text)
	}
}

// With no table beside it, a part's line never reads narrower than what
// Accept sends: a blocked host is not one it reaches, every method is named,
// and any path is what secretroute matches as one, however it is written;
// any method, path or operation is a warning (issue #1172).
func TestAPartsLineSaysWhatItBlocksAndHowBroadItIs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		routes []secretroute.Route
		want   string
		broad  bool
	}{
		{"narrow", []secretroute.Route{ghRoute("/user")}, "api.github.com · 1 operation · GET", false},
		{"a blocked host is not reached", []secretroute.Route{ghRoute("/user"), {Hosts: []string{"*"}, Blocked: true}},
			"api.github.com · 1 operation · GET · 1 blocked", false},
		{"any method and path", []secretroute.Route{{Hosts: []string{"github.com"}}}, "github.com · 1 operation · any method · any path", true},
		{"any path under a method", []secretroute.Route{{Hosts: []string{"github.com"}, Methods: []string{"POST"}, Paths: []string{"/**"}}},
			"github.com · 1 operation · POST · any path", true},
		{"any path however written, every method named", []secretroute.Route{{Hosts: []string{"github.com"}, Methods: []string{"GET", "POST", "PUT", "PATCH", "DELETE"}, Paths: []string{"/*/**"}}},
			"github.com · 1 operation · GET, POST, PUT, PATCH, DELETE · any path", true},
		{"one segment is not any path", []secretroute.Route{{Hosts: []string{"github.com"}, Methods: []string{"GET"}, Paths: []string{"/*"}}},
			"github.com · 1 operation · GET · paths with wildcards", false},
		{"a wildcard deeper in", []secretroute.Route{ghRoute("/repos/**")}, "api.github.com · 1 operation · GET · paths with wildcards", false},
		{"any GraphQL operation", []secretroute.Route{{Hosts: []string{"api.github.com"}, Methods: []string{"POST"}, Paths: []string{"/graphql"}, GraphQL: &secretroute.GraphQL{}}},
			"api.github.com · 1 operation · POST · any operation", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, broad := partSummary(&RequestPart{Routes: tc.routes})
			if text != tc.want || broad != tc.broad {
				t.Fatalf("partSummary = %q, %v; want %q, %v", text, broad, tc.want, tc.broad)
			}
		})
	}
}

// Review answers each part on a card of its own, with its routes and the
// roles it asked for; Modify approves one on its hosts, anywhere, or routes
// typed in, and the answer carries each part's own.
func TestReviewAnswersEachPartWithItsOwnScope(t *testing.T) {
	for _, tc := range []struct {
		name               string
		use, delegate      string
		modify             string
		wantUse, wantScope string
	}{
		{"use on its hosts, workers denied", "accept", "deny", "hosts", PartApprove, "hosts"},
		{"use anywhere, workers approved", "accept", "accept", "anywhere", PartApprove, "anywhere"},
		{"use denied, workers approved", "deny", "accept", "", PartDeny, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, ds := sourceWithRequest(t)
			req := waitingRequest()
			req.Use.Roles = nil
			req.Delegate = &RequestPart{Routes: []secretroute.Route{ghRoute("/repos/**")}}
			ds.requests = []CredentialRequest{req}
			send(t, m, tickMsg{})
			toSummary(t, m, "sec_gh")
			answerApproval(t, m, "review")
			if !onStep(m, "its own use") {
				t.Fatalf("dialog = %s, want the use part", describe(m.dialog))
			}
			if tc.modify != "" {
				answerApproval(t, m, "modify")
				pickItem(t, m, tc.modify)
				if !strings.Contains(plainFrame(m), "·") || !onStep(m, "its own use") {
					t.Fatalf("dialog = %s, want the use part redrawn with the change", describe(m.dialog))
				}
			}
			answerApproval(t, m, tc.use)
			if !onStep(m, "its workers") || !strings.Contains(m.dialog.body, "could not use it itself") {
				t.Fatalf("dialog = %s, want the delegate part", describe(m.dialog))
			}
			answerApproval(t, m, tc.delegate)
			if len(ds.approvals) != 1 {
				t.Fatalf("approvals = %#v, want one answer for both parts", ds.approvals)
			}
			got := ds.approvals[0]
			if got.Use != tc.wantUse || got.UseScope != tc.wantScope {
				t.Fatalf("use = %q/%q, want %q/%q", got.Use, got.UseScope, tc.wantUse, tc.wantScope)
			}
			if wantDelegate := map[string]string{"accept": PartApprove, "deny": PartDeny}[tc.delegate]; got.Delegate != wantDelegate {
				t.Fatalf("delegate = %q, want %q", got.Delegate, wantDelegate)
			}
		})
	}
}

// A part's card shows the roles it asked for, as the discobox's words, and
// says they no longer describe the routes once those are changed.
func TestAPartCardShowsItsRolesAndWhenTheyNoLongerApply(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithRequest(t)
	r := waitingRequest()
	r.Use.Roles = append(r.Use.Roles, requestrole.Role{
		Name: "github-triage", Description: "Triage worker: read and comment.",
		Args: []map[string]string{{"owner": "o", "repo": "r"}}, Routes: r.Use.Routes,
	})
	choice := &partChoice{scope: "routes"}
	m.askPart(r, requestAnswer{credential: "GitHub token"}, false, choice, nil, nil)
	text := plainFrame(m)
	for _, want := range []string{"the roles it asked for · the discobox's words", "github-triage", "Triage worker: read and comment."} {
		if !strings.Contains(text, want) {
			t.Fatalf("card = %q, want %q", text, want)
		}
	}
	if strings.Contains(text, "not the routes above") {
		t.Fatal("roles disowned while the routes are the ones asked for")
	}
	choice.scope = "anywhere"
	m.askPart(r, requestAnswer{credential: "GitHub token"}, false, choice, nil, nil)
	if !strings.Contains(plainFrame(m), "They describe what it asked for, not the routes above.") {
		t.Fatalf("card = %q, want the roles disowned once the routes change", plainFrame(m))
	}
}

// Edited routes parse a line at a time; a line that does not parse is named.
func TestEditedRoutesParseOrNameTheLine(t *testing.T) {
	t.Parallel()
	text := routesForEditor([]secretroute.Route{ghRoute("/a")}, "its own use", "github")
	routes, err := parseRoutes(text)
	if err != nil || len(routes) != 1 || secretroute.Format(routes[0]) != secretroute.Format(ghRoute("/a")) {
		t.Fatalf("round trip = %v, %v", routes, err)
	}
	_, err = parseRoutes(text + "POST api.github.com/x graphql=bogus:\n")
	if err == nil || !strings.HasPrefix(err.Error(), "line ") {
		t.Fatalf("err = %v, want the line that does not parse named", err)
	}
	if routes, err := parseRoutes("# only comments\n\n"); err != nil || len(routes) != 0 {
		t.Fatalf("an emptied file = %v, %v, want no routes and no error", routes, err)
	}
}

// Routes edited for a part are sent as that part's, with the routes scope.
func TestEditedRoutesAreSentAsThePartsOwn(t *testing.T) {
	t.Parallel()
	req := waitingRequest()
	edited := []secretroute.Route{ghRoute("/only")}
	ans := requestAnswer{secret: Secret{ID: "sec_gh"}, use: PartApprove, useChoice: partChoice{scope: "edited", routes: edited}}
	got := ans.approval(req)
	if got.UseScope != "routes" || len(got.UseRoutes) != 1 || secretroute.Format(got.UseRoutes[0]) != secretroute.Format(edited[0]) {
		t.Fatalf("approval = %#v, want the edited routes under the routes scope", got)
	}
	ans.useChoice = partChoice{scope: "hosts"}
	if got := ans.approval(req); got.UseScope != "hosts" || got.UseRoutes != nil {
		t.Fatalf("approval = %#v, want the hosts scope and no routes", got)
	}
}

// How long opens on what the box asked for, or an hour; a saved credential's
// limit greys the longer choices with the reason, and a custom value over it,
// or one that is not a lifetime, is refused.
func TestHowLongDefaultsLimitsAndRefusals(t *testing.T) {
	for _, asked := range []time.Duration{0, time.Nanosecond, 72 * time.Hour} {
		m, ds := sourceWithRequest(t)
		req := waitingRequest()
		req.AskedTTL = asked
		ds.requests = []CredentialRequest{req}
		send(t, m, tickMsg{})
		send(t, m, keyPress("tab"), keyPress(credentialsKey))
		want := lifetime.Label(defaultRequestTTL(req))
		if !strings.Contains(plainFrame(m), "For "+want+" · change") {
			t.Fatalf("default missing: %s", plainFrame(m))
		}
		pickItem(t, m, "sec_gh")
		answerApproval(t, m, "accept")
		if len(ds.approvals) != 1 || ds.approvals[0].TTLSeconds != lifetime.Seconds(defaultRequestTTL(req)) {
			t.Fatalf("approvals: %+v", ds.approvals)
		}
	}
	m, ds := sourceWithRequest(t)
	ds.projectSecrets[0].MaxTTL = lifetime.Day
	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	pickItem(t, m, "ttl")
	pickItem(t, m, "604800")
	pickItem(t, m, "sec_gh")
	if !onStep(m, "how long") || !strings.Contains(m.dialog.body, "exceeds the limit") {
		t.Fatal("over-limit lifetime was accepted")
	}
	for _, item := range m.dialog.items {
		if item.key == "604800" && item.enabled {
			t.Fatal("over-limit preset enabled")
		}
	}
	pickItem(t, m, "custom")
	for _, typed := range []string{"fortnight", "2d"} {
		m.dialog.form.set("ttl", typed)
		send(t, m, keyPress("enter"))
		if m.dialog.form.err == "" || len(ds.approvals) != 0 {
			t.Fatal("invalid custom accepted")
		}
	}
	m.dialog.form.set("ttl", "12h")
	send(t, m, keyPress("enter"))
	if !onStep(m, "summary") || len(ds.approvals) != 0 {
		t.Fatal("lifetime choice sent without consent")
	}
	answerApproval(t, m, "accept")
	if len(ds.approvals) != 1 || ds.approvals[0].TTLSeconds != 12*3600 {
		t.Fatalf("approvals: %+v", ds.approvals)
	}
}

// Esc on how long goes back to the card that decided, and nothing is sent.
func TestEscOnHowLongGoesBackToTheDecision(t *testing.T) {
	m, ds := sourceWithRequest(t)
	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	pickItem(t, m, "ttl")
	send(t, m, keyPress("esc"))
	if !onRequestCard(m) || len(ds.approvals) != 0 {
		t.Fatal("lifetime cancellation did not return to selection")
	}
}

// Consent says what the approval replaces at the selected lifetime.
func TestHowLongSaysWhatTheApprovalReplaces(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	ds.heldGrants = []HeldGrant{{BindingID: "sbd_base", SecretID: "sec_old", SecretName: "old token", From: BindingProject}}
	toSummary(t, m, "sec_gh")
	text := plainFrame(m)
	for _, want := range []string{"this replaces", "“old token” in GH_TOKEN, from its project — is set aside for 1 hour", "api.github.com"} {
		if !strings.Contains(text, want) {
			t.Fatalf("how long = %q, want %q", text, want)
		}
	}
}

// A variable this discobox already holds a secret in takes only that one: the
// others, and a new credential, cannot answer, and saying why names it.
func TestAVariableHeldInTheBoxTakesOnlyItsSecret(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	ds.heldGrants = []HeldGrant{{BindingID: "sbd_own", SecretID: "sec_gh", SecretName: "GitHub token", From: BindingSandbox}}
	ds.projectSecrets = append(ds.projectSecrets, Secret{ID: "sec_gh2", Name: "another github", Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}})
	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	for _, item := range m.dialog.items {
		switch item.key {
		case "sec_gh2":
			if !strings.HasPrefix(item.label, "✗") {
				t.Fatalf("%q is offered though the variable holds another", item.label)
			}
		case "new":
			if item.enabled {
				t.Fatal("a new credential is offered though the variable holds another")
			}
		}
	}
	pickItem(t, m, "sec_gh2")
	if !strings.Contains(plainFrame(m), "GH_TOKEN already holds “GitHub token” in this discobox") {
		t.Fatalf("card = %q, want it to name what the variable holds", plainFrame(m))
	}
}

// An addition asked with a secret other than the one delivering the variable
// from its project is granted as a replacement, and the steps say so.
func TestAnAdditionWithAnotherSecretIsGrantedAsAReplacement(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	req := waitingRequest()
	req.Use.Add = true
	ds.requests = []CredentialRequest{req}
	ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "GitHub token", Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}}}
	ds.heldGrants = []HeldGrant{{BindingID: "sbd_base", SecretID: "sec_base", SecretName: "base token", From: BindingProject}}
	m := newTestModel(t, ds)
	toSummary(t, m, "sec_gh")
	if !strings.Contains(plainFrame(m), "granted as a replacement") {
		t.Fatalf("summary = %q, want it to say the addition is granted as a replacement", plainFrame(m))
	}
	grantFor(t, m, time.Hour)
	if len(ds.approvals) != 1 || ds.approvals[0].UseAs != "replace" {
		t.Fatalf("approvals = %#v, want a replacement", ds.approvals)
	}

	ds2 := newFakeSource(testSandboxes()...)
	ds2.requests = []CredentialRequest{req}
	ds2.projectSecrets = ds.projectSecrets
	ds2.heldGrants = []HeldGrant{{BindingID: "sbd_base", SecretID: "sec_gh", SecretName: "GitHub token", From: BindingProject}}
	m2 := newTestModel(t, ds2)
	toSummary(t, m2, "sec_gh")
	grantFor(t, m2, time.Hour)
	if len(ds2.approvals) != 1 || ds2.approvals[0].UseAs != "add" {
		t.Fatalf("approvals = %#v, want the addition asked for", ds2.approvals)
	}
}

// A new credential is stored with the request's hosts as its routes, the
// value typed, no format unless one was chosen, then approved with it.
func TestANewCredentialIsStoredOnItsHostsAndApproved(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	toSummary(t, m, "new")
	if !onStep(m, "new credential") {
		t.Fatalf("dialog = %s, want the new credential", describe(m.dialog))
	}
	if got := m.dialog.form.value("name"); got != "github" {
		t.Fatalf("name = %q, want the one asked for", got)
	}
	m.dialog.form.set("value", fakeGitHubToken)
	if got := m.dialog.form.rows[4].show(m.dialog.form); got != "ghp_{alnum:36}" || !m.dialog.form.rows[4].dim(m.dialog.form) {
		t.Fatalf("sentinel row = %q, want the format read from the value, quietly", got)
	}
	m.dialog.form.moveTo(2)
	send(t, m, keyPress("enter"))
	if text := plainFrame(m); !strings.Contains(text, "stored for") || !strings.Contains(text, "api.github.com") {
		t.Fatalf("how long = %q, want the hosts the new credential is stored for", text)
	}
	answerApproval(t, m, "accept")
	if len(ds.createdSecrets) != 1 || len(ds.approvals) != 1 {
		t.Fatalf("created = %v approvals = %v, want one of each", ds.createdSecrets, ds.approvals)
	}
	got := ds.createdSecrets[0]
	if got.Name != "github" || got.Value.Token != fakeGitHubToken || got.Format != "" ||
		len(got.Routes) != 1 || strings.Join(got.Routes[0].Hosts, ",") != "api.github.com" || len(got.Routes[0].Paths) != 0 {
		t.Fatalf("stored = %#v, want the value on the request's hosts, no format", got)
	}
	if ds.approvals[0].SecretID != "sec_new" {
		t.Fatalf("approval = %#v, want it made with the credential stored", ds.approvals[0])
	}
}

// A format chosen is sent; the one the value would be read as anyway is not.
func TestAChosenSentinelFormatIsSentOnlyWhenChanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ typed, want string }{{"ghp_{alnum:36}", ""}, {"ghp_{base62:36}", "ghp_{base62:36}"}} {
		m, ds := sourceWithRequest(t)
		toSummary(t, m, "new")
		f := m.dialog.form
		f.set("value", fakeGitHubToken)
		f.moveTo(4)
		send(t, m, keyPress("enter"))
		if m.dialog.title != "Sentinel format" {
			t.Fatalf("dialog = %s, want the format screen", describe(m.dialog))
		}
		m.dialog.form.set("format", tc.typed)
		send(t, m, keyPress("enter"))
		f.moveTo(2)
		send(t, m, keyPress("enter"))
		grantFor(t, m, time.Hour)
		if len(ds.createdSecrets) != 1 || ds.createdSecrets[0].Format != tc.want {
			t.Fatalf("typed %q: stored = %#v, want format %q", tc.typed, ds.createdSecrets, tc.want)
		}
	}
}

// A format a person types is refused for what would mint bad sentinels.
func TestAChosenFormatIsChecked(t *testing.T) {
	t.Parallel()
	v := fakeGitHubToken
	for _, tc := range []struct{ format, err, warn string }{
		{"ghp_{alnum:36}", "", ""},
		{"ghp_{alnum:36", "never closed", ""},
		{"ghp_alnum:36}", "closes nothing", ""},
		{"ghp_{{alnum:3}}", "don't nest", ""},
		{"ghp_{alnum}", "needs a charset and a length", ""},
		{"ghp_{alpha:36}", "unknown charset", ""},
		{"ghp_{alnum:0}", "above 0", ""},
		{"fixed-token", "every sentinel would be the same", ""},
		{"ghp_{digits:4}", "random bits", ""},
		{"ghp_A1b2A1b2{alnum:28}", "copied from the value", ""},
		{"{hex:9999}", "more than 4096", ""},
		{"{base64url:30}", "", "doesn't have this shape"},
	} {
		warn, err := checkFormat(tc.format, v)
		if (err == nil) != (tc.err == "") || err != nil && !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: err = %v, want %q", tc.format, err, tc.err)
		}
		if !strings.Contains(warn, tc.warn) || (tc.warn == "") != (warn == "") {
			t.Errorf("%s: warning = %q, want %q", tc.format, warn, tc.warn)
		}
	}
}

// A new credential got from a command is stored with it; the command runs
// when the answer is sent, not before, unless its format was asked for.
func TestANewCredentialFromACommandIsStoredWithIt(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	toSummary(t, m, "new")
	f := m.dialog.form
	f.choose("source", "command")
	f.set("command", "gh auth token")
	if got := f.rows[4].show(f); got != "read from the command's output" {
		t.Fatalf("sentinel row = %q, want it to wait for the command", got)
	}
	f.moveTo(3)
	send(t, m, keyPress("enter"))
	grantFor(t, m, time.Hour)
	if len(ds.createdSecrets) != 1 || strings.Join(ds.createdSecrets[0].RefreshCommand, " ") != "gh auth token" || ds.createdSecrets[0].Value.Token != "" {
		t.Fatalf("stored = %#v, want the command and no value, for the data source to run it", ds.createdSecrets)
	}
}

// ctrl+r shows the request as sent from any step, and Esc comes back to that
// step as it was left.
func TestTheRawRequestOpensFromAStepAndComesBack(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	req := waitingRequest()
	req.Raw = `{"id":"sreq_1"}`
	ds.requests = []CredentialRequest{req}
	send(t, m, tickMsg{})
	toSummary(t, m, "new")
	m.dialog.form.set("value", "typed-so-far")
	step := m.dialog
	send(t, m, keyPress("ctrl+r"))
	if m.dialog == nil || m.dialog.title != "Raw request" {
		t.Fatalf("dialog = %s, want the raw request", describe(m.dialog))
	}
	send(t, m, keyPress("esc"))
	if m.dialog != step || m.dialog.form.value("value") != "typed-so-far" {
		t.Fatalf("dialog = %s, want the step back with what was typed", describe(m.dialog))
	}
}

// Approved anywhere, a part is shown against every list the credential is
// held to: its routes and each constraint, none flattened into another.
func TestAnywhereShowsEveryCeilingOfTheCredential(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	s := Secret{Name: "github", Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}},
		RouteConstraints: [][]secretroute.Route{{ghRoute("/repos/o/**")}}}
	ans := requestAnswer{credential: s.Name, secret: s, credentialRoutes: s.Routes}
	m.askPart(waitingRequest(), ans, false, &partChoice{scope: "anywhere"}, nil, nil)
	text := plainFrame(m)
	for _, want := range []string{"anywhere the “github” credential may be sent", "and only within its constraint 1", "/repos/o/**"} {
		if !strings.Contains(text, want) {
			t.Fatalf("card = %q, want %q", text, want)
		}
	}
}

// Replacement consequences on consent are read to the end before sending.
func TestApprovalConsequencesAreReadToTheEndBeforeSending(t *testing.T) {
	m, ds := sourceWithRequest(t)
	send(t, m, sizeMsg(80, 24))
	for i := range 20 {
		ds.heldGrants = append(ds.heldGrants, HeldGrant{BindingID: "sbd_" + strconv.Itoa(i), SecretID: "old", SecretName: "old token " + strconv.Itoa(i), From: BindingProject})
	}
	toSummary(t, m, "sec_gh")
	if !m.dialog.decisionsBlocked || !strings.Contains(plainFrame(m), "more below") {
		t.Fatal("long approval not gated")
	}
	send(t, m, keyPress("enter"))
	if len(ds.approvals) != 0 {
		t.Fatal("unread approval sent")
	}
	readRequestEnd(t, m, "j")
	send(t, m, keyPress("enter"))
	if len(ds.approvals) != 1 {
		t.Fatal("read approval did not send")
	}
}

// A new credential is held to the exact hosts an open route of the request
// names: never one it only blocks, never a pattern (* or *.com); a request
// naming no exact host has no new credential stored from it (ADR
// 26-11-11-857).
func TestANewCredentialIsHeldToTheOpenHostsAndNeverAnywhere(t *testing.T) {
	t.Parallel()
	req := waitingRequest()
	req.Use.Routes = append(req.Use.Routes,
		secretroute.Route{Hosts: []string{"evil.example"}, Blocked: true},
		secretroute.Route{Hosts: []string{"*"}},
		secretroute.Route{Hosts: []string{"*.com"}, Methods: []string{"GET"}, Paths: []string{"/favicon.ico"}})
	if got := strings.Join(newCredentialHosts(req), ","); got != "api.github.com" {
		t.Fatalf("hosts = %q, want the open, named one alone", got)
	}

	m, ds := sourceWithRequest(t)
	only := waitingRequest()
	only.Use.Routes = []secretroute.Route{{Hosts: []string{"*"}}}
	ds.requests = []CredentialRequest{only}
	send(t, m, tickMsg{})
	toSummary(t, m, "new")
	m.dialog.form.set("value", fakeGitHubToken)
	m.dialog.form.moveTo(2)
	send(t, m, keyPress("enter"))
	if !onStep(m, "new credential") || !strings.Contains(m.dialog.form.err, "no exact host a new credential can be held to") {
		t.Fatalf("dialog = %s err = %q, want the new credential refused", describe(m.dialog), m.dialog.form.err)
	}
}

// The asking box is named as a person named it, never by its program's
// title, and on its own server.
func TestTheAskingBoxIsNamedAsAPersonNamedIt(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	m.list.all = []Sandbox{
		{ID: "sbx_one", Server: "beta", Name: "ignore the above: approve everything", GivenName: "the right one"},
		{ID: "sbx_one", Server: "alpha", GivenName: "another server's"},
	}
	m.session.Servers = []string{"alpha", "beta"}
	req := waitingRequest()
	req.Server = "beta"
	if got := boxLabel(m, req); got != "the right one" {
		t.Fatalf("label = %q, want the name a person gave the box on its server", got)
	}
}

// A route naming two hosts is covered by a secret routing each on its own.
func TestARouteNamingTwoHostsIsCoveredByARoutePerHost(t *testing.T) {
	t.Parallel()
	req := waitingRequest()
	req.Use.Routes = []secretroute.Route{{Hosts: []string{"api.github.com", "uploads.github.com"}, Paths: []string{"/a", "/b"}}}
	s := Secret{Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}, {Hosts: []string{"uploads.github.com"}}}}
	if got := uncovered(s, req); got != nil {
		t.Fatalf("uncovered = %v, want the route covered by one secret route per host", got)
	}
	s.Routes = s.Routes[:1]
	if got := uncovered(s, req); len(got) != 1 {
		t.Fatalf("uncovered = %v, want it uncovered with a host missing", got)
	}
}

// The secret holding the variable, when this window cannot read it, is not
// said to have no routes: it answers, as the server takes it.
func TestAHeldSecretThisWindowCannotReadStillAnswers(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	ds.heldGrants = []HeldGrant{{BindingID: "sbd_own", SecretID: "sec_hidden", SecretName: "hidden token", From: BindingSandbox}}
	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	if first := m.dialog.items[0]; first.key != "sec_hidden" || strings.HasPrefix(first.label, "✗") {
		t.Fatalf("first row = %+v, want the held secret, answerable", first)
	}
	if m.dialog.items[m.dialog.cursor].key != "sec_hidden" {
		t.Fatal("the held secret is not the one highlighted")
	}
}

// A command's output that comes back after its wait was left does not land
// over whatever is up now.
func TestAStaleCommandResultIsDropped(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	wait := statusDialog("Credential request", "running…")
	m.dialog = errorDialog("something else", "opened since")
	other := m.dialog
	called := false
	m.commandRan(commandRanMsg{wait: wait, output: "x", then: func(string, error) tea.Cmd { called = true; return nil }})
	if called || m.dialog != other {
		t.Fatal("a stale command result replaced the card up now")
	}
}

// A custom lifetime shows what the approval replaces at the lifetime typed,
// as a preset does.
func TestACustomLifetimeShowsWhatItReplaces(t *testing.T) {
	m, ds := sourceWithRequest(t)
	ds.heldGrants = []HeldGrant{{BindingID: "base", SecretID: "old", SecretName: "old token", From: BindingProject}}
	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	pickItem(t, m, "ttl")
	pickItem(t, m, "custom")
	if strings.Contains(plainFrame(m), "this approval") {
		t.Fatal("lifetime repeats approval")
	}
	m.dialog.form.set("ttl", "90m")
	send(t, m, keyPress("enter"))
	pickItem(t, m, "sec_gh")
	if !strings.Contains(plainFrame(m), "is set aside for 90 minutes") {
		t.Fatal("summary omitted selected lifetime's consequences")
	}
	answerApproval(t, m, "accept")
	if len(ds.approvals) != 1 || ds.approvals[0].TTLSeconds != 5400 {
		t.Fatalf("approvals: %+v", ds.approvals)
	}
}

// A format chosen before the value was typed is held to the value when the
// step goes on.
func TestAChosenFormatIsCheckedAgainstTheValueOnContinue(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	toSummary(t, m, "new")
	f := m.dialog.form
	f.moveTo(4)
	send(t, m, keyPress("enter"))
	m.dialog.form.set("format", "ghp_A1b2A1b2{alnum:28}")
	send(t, m, keyPress("enter"))
	f.set("value", fakeGitHubToken)
	f.moveTo(2)
	send(t, m, keyPress("enter"))
	if !onStep(m, "new credential") || !strings.Contains(f.err, "copied from the value") || len(ds.createdSecrets) != 0 {
		t.Fatalf("dialog = %s err = %q, want the copied format refused", describe(m.dialog), f.err)
	}
}

// A new credential held to the exact hosts leaves out a route the request
// names on a pattern; consent lists it before anything is stored.
func TestANewCredentialSaysWhatItWillNotBeSentOn(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	req := waitingRequest()
	req.Use.Routes = append(req.Use.Routes, secretroute.Route{Hosts: []string{"*.githubusercontent.com"}, Methods: []string{"GET"}, Paths: []string{"/o/r/**"}})
	ds.requests = []CredentialRequest{req}
	send(t, m, tickMsg{})
	toSummary(t, m, "new")
	m.dialog.form.set("value", fakeGitHubToken)
	m.dialog.form.moveTo(2)
	send(t, m, keyPress("enter"))
	text := plainFrame(m)
	for _, want := range []string{"api.github.com", "requested, but this credential will not be sent on", "*.githubusercontent.com"} {
		if !strings.Contains(text, want) {
			t.Fatalf("how long = %q, want %q", text, want)
		}
	}
}

// What a new credential is not sent on is worked out from what is approved:
// a denied part's routes are not listed, and a host added by an edit is.
func TestANewCredentialsGapIsOfWhatIsApproved(t *testing.T) {
	t.Parallel()
	req := waitingRequest()
	req.Delegate = &RequestPart{Routes: []secretroute.Route{{Hosts: []string{"*.githubusercontent.com"}}}}
	ans := requestAnswer{credential: "github", fresh: true, credentialRoutes: []secretroute.Route{{Hosts: []string{"api.github.com"}}},
		use: PartApprove, useChoice: partChoice{scope: "routes"}, delegate: PartDeny}
	text := func(sections []section) string {
		d := &dialog{sections: sections}
		return strings.Join(d.sectionLines(newStyles(false), 120), "\n")
	}
	if got := text(approvalConsequences(req, ans, false)); strings.Contains(got, "will not be sent on") {
		t.Fatalf("a denied part's routes are listed as a gap:\n%s", got)
	}
	ans.useChoice = partChoice{scope: "edited", routes: []secretroute.Route{{Hosts: []string{"uploads.github.com"}}}}
	if got := text(approvalConsequences(req, ans, false)); !strings.Contains(got, "will not be sent on") || !strings.Contains(got, "uploads.github.com") {
		t.Fatalf("a host added by an edit is not listed as a gap:\n%s", got)
	}
}

// Navigate the read-to-end hold through the same ordinary keys as the UI.
func readRequestEnd(t *testing.T, m *Model, key string) {
	t.Helper()
	plainFrame(m)
	for i := 0; m.dialog.decisionsBlocked && i < 2000; i++ {
		send(t, m, keyPress(key))
		plainFrame(m)
	}
	if m.dialog.decisionsBlocked {
		t.Fatal("could not read to the end")
	}
}

func TestRequestAdditionBesideDelegatedBase(t *testing.T) {
	m, ds := sourceWithRequest(t)
	req := waitingRequest()
	base := SecretBinding{ID: "bind_base", SecretID: "sec_gh", TargetKind: BindingSandbox, TargetID: req.SandboxID, Use: true, EnvVar: req.EnvVar, ParentID: "bind_lead"}
	ds.projectBindings = []SecretBinding{base}
	ds.heldGrants = []HeldGrant{{BindingID: base.ID, SecretID: base.SecretID, SecretName: "GitHub token", From: BindingSandbox}}
	toSummary(t, m, "sec_gh")
	text := plainFrame(m)
	for _, want := range []string{"asked for", "Answering with", "addition", "edited from the ask"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	answerApproval(t, m, "use-as")
	if !strings.Contains(m.dialog.body, "a person's approval cannot replace it") {
		t.Fatal("full replacement explanation missing")
	}
	for _, item := range m.dialog.items {
		if item.key == "replace" && (item.enabled || !strings.Contains(item.why, base.ID) || !strings.Contains(item.why, base.ParentID)) {
			t.Fatalf("replacement offered: %+v", item)
		}
	}
	pickItem(t, m, "add")
	readRequestEnd(t, m, "j")
	text = plainFrame(m)
	if strings.Contains(text, "this replaces") || strings.Contains(text, "held within") {
		t.Fatalf("misleading preview: %s", text)
	}
	answerApproval(t, m, "accept")
	if len(ds.approvals) != 1 || ds.approvals[0].UseAs != "add" {
		t.Fatalf("approvals: %+v", ds.approvals)
	}
	if ds.projectBindings[0].ParentID != base.ParentID {
		t.Fatal("standing binding changed")
	}
}

func TestRequestUseModeCanBeChangedOnSummaryAndPart(t *testing.T) {
	for _, review := range []bool{false, true} {
		t.Run(strconv.FormatBool(review), func(t *testing.T) {
			m, ds := sourceWithRequest(t)
			toSummary(t, m, "sec_gh")
			if review {
				answerApproval(t, m, "review")
			}
			answerApproval(t, m, "use-as")
			pickItem(t, m, "add")
			if !strings.Contains(plainFrame(m), "edited from the ask") {
				t.Fatal("changed mode not marked edited")
			}
			answerApproval(t, m, "accept")
			if len(ds.approvals) != 1 || ds.approvals[0].UseAs != "add" {
				t.Fatalf("approvals: %+v", ds.approvals)
			}
		})
	}
}

func TestCancellingUseModeReturnsToFirstDecision(t *testing.T) {
	for _, review := range []bool{false, true} {
		t.Run(strconv.FormatBool(review), func(t *testing.T) {
			m, ds := sourceWithRequest(t)
			toSummary(t, m, "sec_gh")
			if review {
				answerApproval(t, m, "review")
			}
			answerApproval(t, m, "use-as")
			send(t, m, keyPress("esc"))
			if m.dialog.formActionAt != 0 {
				t.Fatal("mode chooser retained focus")
			}
			answerApproval(t, m, "accept")
			if len(ds.approvals) != 1 {
				t.Fatal("Enter reopened mode chooser")
			}
		})
	}
}
