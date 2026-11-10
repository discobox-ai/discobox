package bridge

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// githubRequest is b1's agent asking to use github in GH_TOKEN.
func githubRequest(id string) console.CredentialRequest {
	return console.CredentialRequest{
		ID: id, SandboxID: "b1", Name: "github", EnvVar: "GH_TOKEN", Created: time.Now().Add(-time.Minute),
		Use: &console.RequestPart{Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}},
	}
}

// inboxCore is one running box, b1, with a github secret, and requests.
func inboxCore(requests ...console.CredentialRequest) *fakeCore {
	core := newFakeCore("home", running("b1"))
	core.secrets = []console.Secret{{ID: "sec_github", Name: "github", Type: "token"}}
	core.requests = requests
	return core
}

// submitted answers a form with an action, changing nothing.
func (a *app) submitted(form *Form, action console.CardAction) {
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{}, Action: action}})
}

// A credential request is answered through the core's cards: the part's card
// says what the core says, the review follows it, and accepting that sends
// the one approval, its lifetime said.
func TestARequestIsApprovedThroughTheCoresCards(t *testing.T) {
	core := inboxCore(githubRequest("r1"))
	a := dial(t, core)
	a.hello()
	id := a.command(Command{Command: "requests", Targets: []string{"b1"}})

	part := a.expect(TypeForm).Form
	if part.Title != console.PartTitle(false) || part.Aside != "1 of 1" {
		t.Fatalf("part card %q %q", part.Title, part.Aside)
	}
	if keys := actionKeys(part); !slices.Equal(keys, []string{"accept", "deny"}) {
		t.Fatalf("part actions %v", keys)
	}
	if !cardSays(part.Card, "This box may use the credential.") {
		t.Fatalf("part card does not say the core's words: %+v", part.Card)
	}
	a.submitted(part, "accept")

	review := a.expect(TypeForm).Form
	if review.Title != console.ReviewTitle || !cardSays(review.Card, "the binding") {
		t.Fatalf("review %q %+v", review.Title, review.Card)
	}
	a.submitted(review, "accept")
	r := a.result(id)
	if r.Status != StatusDone || !strings.HasPrefix(r.Note, "approved github for ") {
		t.Fatalf("approved: %+v", r)
	}
	if calls := core.called(); !slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "approve r1 sec_github ") }) {
		t.Fatalf("calls %q, want the approval", calls)
	}
}

// A card's submit with no action, or one it does not offer, is refused on the
// card: it is never read as the accept, nor as the decline.
func TestACardTakesOnlyItsActions(t *testing.T) {
	core := inboxCore(githubRequest("r1"))
	a := dial(t, core)
	a.hello()
	id := a.command(Command{Command: "requests", Targets: []string{"b1"}})
	part := a.expect(TypeForm).Form
	for _, action := range []console.CardAction{"", console.ActionRefresh} {
		a.submitted(part, action)
		if e := a.expect(TypeFieldErrors).FieldErrors; e.Form != part.ID {
			t.Fatalf("%q: errors for %d", action, e.Form)
		}
	}
	if !part.Actions[1].Declines || part.Leave != console.LeaveWaiting {
		t.Fatalf("deny declines %v, leave %q", part.Actions[1].Declines, part.Leave)
	}
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: part.ID, Answers: map[string]string{}, Cancel: true}})
	if r := a.result(id); r.Status != StatusCanceled {
		t.Fatalf("left: %+v", r)
	}
	if calls := core.called(); slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "approve") || strings.HasPrefix(c, "deny") }) {
		t.Fatalf("calls %q, want the request left waiting", calls)
	}
}

// Denying the one part a request asks is the request denied, without a
// review: there is nothing to replace.
func TestDenyingTheOnlyPartDeniesTheRequest(t *testing.T) {
	core := inboxCore(githubRequest("r1"))
	a := dial(t, core)
	a.hello()
	id := a.command(Command{Command: "requests", Targets: []string{"b1"}})
	a.submitted(a.expect(TypeForm).Form, "deny")
	if r := a.result(id); r.Status != StatusDone || r.Note != "denied github" {
		t.Fatalf("denied: %+v", r)
	}
	if calls := core.called(); !slices.Contains(calls, "deny r1") {
		t.Fatalf("calls %q", calls)
	}
}

// A trust is answered with the certificate its card chose.
func TestATrustIsAnsweredWithItsPin(t *testing.T) {
	ca := console.TrustCertificate{Subject: "Corp CA", SHA256: "aaaa", IsCA: true}
	leaf := console.TrustCertificate{Subject: "git.corp", SHA256: "bbbb", SPKISHA256: "cccc"}
	core := inboxCore(console.CredentialRequest{ID: "t1", SandboxID: "b1", Host: "git.corp:443",
		Trust: &console.TrustAsk{Chain: []console.TrustCertificate{leaf, ca}, DefaultPin: console.TrustPin{Kind: console.TrustPinCA, SHA256: "aaaa"}}})
	a := dial(t, core)
	a.hello()
	id := a.command(Command{Command: "requests", Targets: []string{"b1"}})
	form := a.expect(TypeForm).Form
	if form.Title != "Trust git.corp:443?" || !cardSays(form.Card, "Selected certificate") {
		t.Fatalf("trust card %q %+v", form.Title, form.Card)
	}
	a.submitted(form, "accept")
	if r := a.result(id); r.Status != StatusDone {
		t.Fatalf("trusted: %+v", r)
	}
	if calls := core.called(); !slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "trust t1 ca:aaaa ") }) {
		t.Fatalf("calls %q", calls)
	}
}

// A box's notice is the core's, and dismissing it takes it down until what
// it is about changes.
func TestANoticeIsDismissedUntilItChanges(t *testing.T) {
	core := inboxCore(githubRequest("r1"))
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicListing}})
	a.expect(TypeSnapshot)
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 2, Topic: TopicRequests}})
	r := a.expect(TypeSnapshot).Snapshot.Requests
	if len(r.Notices) != 1 || r.Notices[0].Box != "b1" || r.Notices[0].Kind != console.NoticeRequest ||
		r.Notices[0].Command != "requests" || !r.Notices[0].Urgent || r.Notices[0].Subject != "credential request" {
		t.Fatalf("notices %+v", r.Notices)
	}
	if len(r.Arrived) != 0 {
		t.Fatalf("what was waiting as the window opened arrived: %+v", r.Arrived)
	}
	ctx := Context{Workspace: "b1", HasWorkspace: true, WorkspaceShown: true, PaneFocused: true}
	if got := a.result(a.command(Command{Command: "dismiss", Context: ctx})); got.Status != StatusDone {
		t.Fatalf("dismissed: %+v", got)
	}
	if r := a.expect(TypeSnapshot).Snapshot.Requests; len(r.Notices) != 0 {
		t.Fatalf("notices after a dismissal %+v", r.Notices)
	}
	if got := a.result(a.command(Command{Command: "dismiss", Context: ctx})); got.Status != StatusFailed || got.Why != console.NothingToDismiss {
		t.Fatalf("dismissed with nothing up: %+v", got)
	}

	// A request arriving brings it back, and is one to notify.
	core.mu.Lock()
	core.requests = append(core.requests, githubRequest("r2"))
	core.mu.Unlock()
	a.command(Command{Command: "dismiss", Context: Context{}})
	r = a.expect(TypeSnapshot).Snapshot.Requests
	if len(r.Notices) != 1 || r.Notices[0].Subject != "2 credential requests" {
		t.Fatalf("notices after one arrived %+v", r.Notices)
	}
	if len(r.Arrived) != 1 || r.Arrived[0].Request != "r2" || r.Arrived[0].Box != "b1" || r.Arrived[0].Title != "Credential request" ||
		!strings.Contains(r.Arrived[0].Body, "github") {
		t.Fatalf("arrived %+v", r.Arrived)
	}
}

// The secrets page lists the requests waiting on its server, and answers
// one of them by its ID.
func TestTheSecretsPageAnswersItsRequests(t *testing.T) {
	core := inboxCore(githubRequest("r1"))
	a := dial(t, core)
	a.hello()
	page := a.result(a.command(Command{Command: "credentials"}))
	if len(page.Requests) != 1 || page.Requests[0].ID != "r1" || page.Requests[0].Subject != "github" ||
		page.Requests[0].Age == "" || page.SecretWords.Waiting != "1 waiting" {
		t.Fatalf("page requests %+v %+v", page.Requests, page.SecretWords)
	}
	id := a.command(Command{Command: "credentials", Args: Args{SecretAction: console.SecretActionAnswer, Request: "r1"}})
	a.submitted(a.expect(TypeForm).Form, "deny")
	r := a.result(id)
	if r.Status != StatusDone || r.Note != "denied github" || len(r.Requests) != 0 || len(r.Secrets) != 1 {
		t.Fatalf("answered from the page: %+v", r)
	}
	if got := a.result(a.command(Command{Command: "credentials", Args: Args{SecretAction: console.SecretActionAnswer, Request: "r1"}})); got.Status != StatusFailed {
		t.Fatalf("a request answered already: %+v", got)
	}
}

// A refresh run for the session answers the next refresh of the same token
// and command without asking, and says so.
func TestARefreshAllowedForTheSessionRunsAgain(t *testing.T) {
	refresh := func(id string) console.CredentialRequest {
		return console.CredentialRequest{ID: id, SandboxID: "b1", Name: "npm", Refresh: &console.RefreshAsk{SecretID: "sec_npm", Cause: "stale"}}
	}
	core := inboxCore(refresh("f1"))
	core.secrets = append(core.secrets, console.Secret{ID: "sec_npm", Name: "npm", RefreshCommand: []string{"npm", "token"}})
	a := dial(t, core)
	a.hello()
	id := a.command(Command{Command: "requests", Targets: []string{"b1"}})
	form := a.expect(TypeForm).Form
	if keys := actionKeys(form); !slices.Equal(keys, []string{"refresh", "dismiss"}) {
		t.Fatalf("refresh actions %v", keys)
	}
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{"permission": "session"}, Action: "refresh"}})
	if r := a.result(id); r.Status != StatusDone || r.Note != "renewed npm" {
		t.Fatalf("renewed: %+v", r)
	}
	core.mu.Lock()
	core.requests = []console.CredentialRequest{refresh("f2")}
	core.mu.Unlock()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicRequests}})
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("no renewal said; calls %q", core.called())
		default:
		}
		r := a.expect(TypeSnapshot).Snapshot.Requests
		if len(r.Said) == 1 && r.Said[0].Text == "renewed npm" {
			break
		}
	}
	if calls := core.called(); !slices.Contains(calls, `renew f2 sec_npm "npm token" session=true`) {
		t.Fatalf("calls %q", calls)
	}
}

func actionKeys(f *Form) []string {
	var out []string
	for _, x := range f.Actions {
		out = append(out, string(x.Key))
	}
	return out
}

// cardSays is whether a card has a section, field or line saying text.
func cardSays(card []DetailSection, text string) bool {
	for _, s := range card {
		if s.Label == text {
			return true
		}
		for _, f := range s.Fields {
			if f.Value == text || f.Label == text {
				return true
			}
		}
		for _, l := range s.Lines {
			if l.Text == text {
				return true
			}
		}
	}
	return false
}
