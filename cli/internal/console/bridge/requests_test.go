package bridge

import (
	"errors"
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
	id := a.command(Command{Command: "dismiss", Context: ctx})
	// The requests snapshot can arrive before the dismissal's result.
	var got *Result
	r = nil
	for got == nil || r == nil {
		switch m := a.message(); m.Type {
		case TypeResult:
			if m.Result.ID == id {
				got = m.Result
			}
		case TypeSnapshot:
			if m.Snapshot.Sub == 2 {
				r = m.Snapshot.Requests
			}
		}
	}
	if got.Status != StatusDone {
		t.Fatalf("dismissed: %+v", got)
	}
	if len(r.Notices) != 0 {
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

// collect reads messages until one of each kind wanted has come, in whatever
// order they come: a command's question or result and its follower's snapshot
// race (REVIEW.md).
func (a *app) collect(want map[string]func(Message) bool) map[string]Message {
	a.t.Helper()
	got := map[string]Message{}
	for len(got) < len(want) {
		m := a.message()
		for name, is := range want {
			if _, ok := got[name]; !ok && is(m) {
				got[name] = m
				break
			}
		}
	}
	return got
}

func isQuestion(m Message) bool { return m.Type == TypeQuestion }

func isResult(id uint64) func(Message) bool {
	return func(m Message) bool { return m.Type == TypeResult && m.Result.ID == id }
}

func isRequests(sub uint32, want func(*Requests) bool) func(Message) bool {
	return func(m Message) bool {
		return m.Type == TypeSnapshot && m.Snapshot.Sub == sub && m.Snapshot.Requests != nil && want(m.Snapshot.Requests)
	}
}

// A project that owes the harness changes explainer has the requests topic
// name the first listed box with a change to save. The review config command
// with Explain asks the core's explainer once, the topic no longer naming the
// box, and records it on the project once it is answered; Review changes
// opens the review, and asking again is canceled unasked.
func TestTheExplainerIsAskedOnceAndMarksTheProject(t *testing.T) {
	plain, pending := running("b1"), running("b2")
	pending.HarnessName, pending.HarnessConfigPending = "Claude Code", true
	core := newFakeCore("home", plain, pending)
	core.explain = true
	core.changes = console.HarnessChanges{CandidateID: "cand_1", State: "pending", Credentials: []string{"claude login"}}
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicListing}})
	a.expect(TypeSnapshot)
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 2, Topic: TopicRequests}})
	if r := a.expect(TypeSnapshot).Snapshot.Requests; r.Explain != "b2" {
		t.Fatalf("explain %q, want the box with a change to save", r.Explain)
	}

	id := a.command(Command{Command: "review config", Targets: []string{"b2"}, Args: Args{Explain: true}})
	got := a.collect(map[string]func(Message) bool{
		"question": isQuestion,
		"requests": isRequests(2, func(r *Requests) bool { return r.Explain == "" }),
	})
	q := got["question"].Question
	want := console.HarnessChangesExplainer(pending)
	if q.Title != want.Title || q.Body != want.Body || q.Subject != "b2" || len(q.Choices) != 2 || q.Choices[0].Key != console.ExplainReview {
		t.Fatalf("question %+v", q)
	}
	if slices.Contains(core.called(), "explained") {
		t.Fatalf("the project was marked before the explainer was answered: %q", core.called())
	}
	a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Key: console.ExplainReview}})
	if r := a.result(id); r.Status != StatusEnter || r.Box != "b2" || r.Changes == nil || r.Changes.Candidate != "cand_1" {
		t.Fatalf("review %+v", r)
	}
	if !slices.Contains(core.called(), "explained") {
		t.Fatalf("the project was not marked once the explainer was answered: %q", core.called())
	}

	// Asked once: a second asking, from this window, is canceled unasked.
	if r := a.result(a.command(Command{Command: "review config", Targets: []string{"b2"}, Args: Args{Explain: true}})); r.Status != StatusCanceled {
		t.Fatalf("asked again: %+v", r)
	}
}

// Leaving it is answering it, and a record that does not land is said in the
// requests topic, in the core's words.
func TestTheExplainerLeftSaysAMarkThatFailed(t *testing.T) {
	pending := running("b1")
	pending.HarnessConfigPending = true
	core := newFakeCore("home", pending)
	core.explain, core.explainErr = true, errors.New("server down")
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicListing}})
	a.expect(TypeSnapshot)
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 2, Topic: TopicRequests}})
	a.expect(TypeSnapshot)

	id := a.command(Command{Command: "review config", Targets: []string{"b1"}, Args: Args{Explain: true}})
	q := a.collect(map[string]func(Message) bool{
		"question": isQuestion,
		"requests": isRequests(2, func(r *Requests) bool { return r.Explain == "" }),
	})["question"].Question
	a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Declined: true}})
	got := a.collect(map[string]func(Message) bool{
		"result":   isResult(id),
		"requests": isRequests(2, func(r *Requests) bool { return len(r.Said) > 0 }),
	})
	if r := got["result"].Result; r.Status != StatusCanceled {
		t.Fatalf("left: %+v", r)
	}
	said := got["requests"].Snapshot.Requests.Said
	if len(said) != 1 || !said[0].Warning || !strings.Contains(said[0].Text, "server down") {
		t.Fatalf("said %+v, want the failed record said", said)
	}
	if slices.Contains(core.called(), "changes b1") {
		t.Fatalf("leaving it read the changes: %q", core.called())
	}
}

// A project that has explained it already is never named, nor asked.
func TestNoExplainerOnAProjectThatHasExplained(t *testing.T) {
	pending := running("b1")
	pending.HarnessConfigPending = true
	a := dial(t, newFakeCore("home", pending))
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicListing}})
	a.expect(TypeSnapshot)
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 2, Topic: TopicRequests}})
	if r := a.expect(TypeSnapshot).Snapshot.Requests; r.Explain != "" {
		t.Fatalf("explain %q on a project that has explained it", r.Explain)
	}
	if r := a.result(a.command(Command{Command: "review config", Targets: []string{"b1"}, Args: Args{Explain: true}})); r.Status != StatusCanceled {
		t.Fatalf("asked: %+v", r)
	}
}
