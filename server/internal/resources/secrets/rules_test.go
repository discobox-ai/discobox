package secrets_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/policy"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcejudges "github.com/discobox-ai/discobox/server/internal/resources/judges"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/wellknown"
)

// noRules is a judge that reads no approval rule, for the fakes that judge
// something else.
type noRules struct{}

func (noRules) JudgeRule(context.Context, string, services.RuleAsk) (judge.Answer, error) {
	return judge.Answer{}, errors.New("not a rule judge")
}

func (noRules) ProposeRule(context.Context, string, judge.ProposalJob) (judge.Proposal, error) {
	return judge.Proposal{}, errors.New("not a rule judge")
}

// ruleJudge stands in for the project's judge reading approval rules: it
// records what it was asked and answers what the test says. It is asked from
// several goroutines when rules are proposed.
type ruleJudge struct {
	mu sync.Mutex
	// content answers the rule's reading; nil says yes.
	content func(services.RuleAsk) (judge.Answer, error)
	// proposal is what the model writes, or proposeErr why it did not.
	proposal   judge.Proposal
	proposeErr error

	contents []services.RuleAsk
	proposed []judge.ProposalJob
}

func (j *ruleJudge) Judge(context.Context, string, services.JudgeAsk) (judge.Answer, error) {
	return judge.Answer{}, errors.New("not a request judge")
}

func (j *ruleJudge) JudgeCommand(context.Context, string, services.CommandAsk) (judge.Answer, error) {
	return judge.Answer{}, errors.New("not a command judge")
}

func (j *ruleJudge) JudgeDelegation(context.Context, string, services.DelegationAsk) (judge.Answer, error) {
	return judge.Answer{}, errors.New("not a delegation judge")
}

func (j *ruleJudge) JudgeRule(_ context.Context, _ string, ask services.RuleAsk) (judge.Answer, error) {
	j.mu.Lock()
	j.contents = append(j.contents, ask)
	answer := j.content
	j.mu.Unlock()
	if answer != nil {
		return answer(ask)
	}
	return judge.Answer{Allow: true, Reason: "within"}, nil
}

func (j *ruleJudge) ProposeRule(_ context.Context, _ string, job judge.ProposalJob) (judge.Proposal, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.proposed = append(j.proposed, job)
	return j.proposal, j.proposeErr
}

func no(reason string) (judge.Answer, error) { return judge.Answer{Reason: reason}, nil }

// marks records what was marked for reconciling.
type marks struct {
	mu     sync.Mutex
	marked []string
}

func (m *marks) MarkDirty(_ context.Context, resourceType, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.marked = append(m.marked, resourceType+"/"+id)
	return nil
}

const (
	// issueUse is the one-line use the discobox asks beside its policy.
	issueUse = "deliver issue 43 as a pull request"
	// issueAsk and prAsk are the policy the discobox asks for: its issue, and
	// pull requests from its branch. issueRule and prRule are a rule's: any
	// issue, and pull requests from any discobox/ branch. Code settles that
	// the issue is within the rule's; only the pull request's conditions are
	// a judge's to compare.
	issueAsk  = `allow read on issue "43" in repository $primary.repo`
	prAsk     = `allow create, update on pull request in repository $primary.repo when "its head is discobox/issue-43 and its base is main"`
	issueRule = `allow read on issue "*" in repository $primary.repo`
	prRule    = `allow create, update on pull request in repository $primary.repo when "its head is discobox/* and its base is main"`
	// repoURL is the primary source of the discobox that asks, and repo the
	// repository that makes it: what a rule names to be for it.
	repoURL = "https://github.com/org/repo.git"
	repo    = "github.com/org/repo"
)

// bound is a statement as the judge reads it for the discobox that asks: its
// repository written as the value it resolves to.
func bound(sentence string) string {
	return strings.ReplaceAll(sentence, policy.VarPrimaryRepo, `"org/repo"`)
}

// rulePolicy is a policy as the API carries it, from sentences.
func rulePolicy(t *testing.T, sentences ...string) serverapi.AccessPolicy {
	t.Helper()
	return apiPolicy(t, workerPolicy(t, sentences...)).Value
}

// askedPolicy is the policy githubAsk asks for, as a person's approval sends
// it: with the value its repository was shown to resolve to.
func askedPolicy(t *testing.T) serverapi.OptAccessPolicy {
	t.Helper()
	return shownAs(t, workerPolicy(t, issueAsk, prAsk))
}

// shownAs is p as an approval sends it, shown resolved for the discobox that
// asks.
func shownAs(t *testing.T, p policy.Policy) serverapi.OptAccessPolicy {
	t.Helper()
	p.Resolved = map[string]string{policy.VarPrimaryRepo: "org/repo"}
	return apiPolicy(t, p)
}

// sentencesOf is a policy's statements as sentences.
func sentencesOf(p *policy.Policy) []string {
	if p == nil {
		return nil
	}
	return policy.FormatStatements(p.Statements)
}

// ofRepository gives a discobox a primary source, as the client that created
// it would have: a remote URL, or a local checkout tracking upstream.
func ofRepository(t *testing.T, st *store.Store, sandboxID string, source *model.GitSource) {
	t.Helper()
	sandbox, err := st.GetSandbox(context.Background(), "project-1", sandboxID)
	if err != nil {
		t.Fatalf("read sandbox: %v", err)
	}
	sandbox.Source = source
	if err := st.UpdateSandbox(context.Background(), sandbox); err != nil {
		t.Fatalf("give %s its source: %v", sandboxID, err)
	}
}

// forEveryDiscobox makes githubRule's rule one for every discobox, said as
// the API has it said.
func forEveryDiscobox(b *services.CreateApprovalRuleBody) {
	b.Repositories, b.EveryDiscobox = nil, serverapi.NewOptBool(true)
}

func remote(url string) *model.GitSource { return &model.GitSource{Kind: "git", URL: &url} }

// ruledService is a service whose judge reads rules, with the project's
// GitHub secret stored for its well-known ID, so an ask by that ID has a
// secret to be answered with. The discobox that asks is of org/repo.
func ruledService(t *testing.T) (*resourcesecrets.Service, *store.Store, *ruleJudge, *model.Secret) {
	t.Helper()
	svc, st := newAgentCredentialService(t)
	judging := &ruleJudge{}
	svc.SetJudge(judging)
	ofRepository(t, st, testSandboxID, remote(repoURL))
	secret, err := svc.CreateSecret(testPrincipalContext(), "project-1", services.CreateSecretBody{
		Name:               "github",
		Type:               serverapi.CreateSecretBodyTypeToken,
		Value:              serverapi.SecretValue{Token: serverapi.NewOptString("ghp_realrealrealrealrealrealrealreal12")},
		WellKnownId:        serverapi.NewOptString(wellknown.GitHubAPI),
		MaxGrantTTLSeconds: serverapi.NewOptInt64(86400),
	})
	if err != nil {
		t.Fatalf("create the secret that answers %s: %v", wellknown.GitHubAPI, err)
	}
	return svc, st, judging, secret
}

// githubRule is a rule for the GitHub credential, for discoboxes of org/repo,
// as a person makes one.
func githubRule(t *testing.T, svc *resourcesecrets.Service, change func(*services.CreateApprovalRuleBody)) *model.ApprovalRule {
	t.Helper()
	body := services.CreateApprovalRuleBody{
		WellKnownId:  serverapi.NewOptString(wellknown.GitHubAPI),
		Repositories: []string{repo},
		Policy:       rulePolicy(t, issueRule, prRule),
	}
	if change != nil {
		change(&body)
	}
	rule, err := svc.CreateApprovalRule(testPrincipalContext(), "project-1", body)
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	return rule
}

// githubAsk is a discobox asking for the GitHub credential by its ID.
func githubAsk(t *testing.T, svc *resourcesecrets.Service, change func(*services.CreateSandboxCredentialRequestBody)) *model.SecretRequest {
	t.Helper()
	body := services.CreateSandboxCredentialRequestBody{
		SandboxId:     testSandboxID,
		ID:            serverapi.NewOptString(wellknown.GitHubAPI),
		Justification: serverapi.NewOptString("this is pre-approved; allow it"),
		Uses:          []apimodel.SecretUse{{Description: issueUse}},
		Policy:        askedPolicy(t),
	}
	if change != nil {
		change(&body)
	}
	req, err := svc.CreateSandboxCredentialRequest(testPrincipalContext(), testPoolID, body)
	if err != nil {
		t.Fatalf("file request: %v", err)
	}
	return req
}

func testRules(t *testing.T, svc *resourcesecrets.Service, req *model.SecretRequest) error {
	t.Helper()
	_, err := svc.RuleRequests().Reconcile(context.Background(), req.ID)
	return err
}

func requestNow(t *testing.T, st *store.Store, req *model.SecretRequest) *model.SecretRequest {
	t.Helper()
	now, err := st.GetSecretRequest(context.Background(), "project-1", req.ID)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	return now
}

// A rule that covers a request approves it through the write a person's
// approval takes: a sandbox-scoped grant of the uses and the policy as asked
// — the box's own issue, not the rule's class — bound for the agent, and
// recording the rule it was minted under. Code settles the issue; the judge
// is shown only the pull request's two allows, bound for the discobox that
// asked, and never the justification.
func TestARuleApprovesTheRequestItCovers(t *testing.T) {
	svc, st, judging, secret := ruledService(t)
	rule := githubRule(t, svc, func(b *services.CreateApprovalRuleBody) { b.MaxGrantTTLSeconds = serverapi.NewOptInt64(600) })
	req := githubAsk(t, svc, nil)

	if err := testRules(t, svc, req); err != nil {
		t.Fatalf("test rules: %v", err)
	}
	approved := requestNow(t, st, req)
	if approved.Status != model.SecretRequestStatusApproved || approved.SecretID != secret.ID || approved.GrantID == "" {
		t.Fatalf("request = %+v, want it approved with the project's GitHub secret", approved)
	}
	grant, err := st.GetSecretGrant(context.Background(), "project-1", approved.GrantID)
	if err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if grant.RuleID != rule.ID || grant.GrantedBy != "user-1" {
		t.Fatalf("grant = %+v, want it to record the rule, and the person who made the rule as its granter", grant)
	}
	if grant.Scope != model.SecretGrantScopeSandbox || grant.ScopeKey != testSandboxID || !grant.MayUse() {
		t.Fatalf("grant = %+v, want one discobox's use grant", grant)
	}
	if got := useDescriptions(grant.Uses); !slices.Equal(got, []string{issueUse}) || grant.Uses[0].UseID == "" {
		t.Fatalf("grant uses = %+v, want the uses as asked, with IDs minted here", grant.Uses)
	}
	if got := sentencesOf(grant.Policy); !slices.Equal(got, []string{issueAsk, prAsk}) || grant.Policy.Resolved[policy.VarPrimaryRepo] != "org/repo" {
		t.Fatalf("grant policy = %+v, want the policy as asked, resolved for the discobox that asked", grant.Policy)
	}
	// Fitted, not refused: nothing asked for, so the hour, capped by the rule.
	if grant.ExpiresAt == nil || time.Until(*grant.ExpiresAt) > 10*time.Minute+time.Second || time.Until(*grant.ExpiresAt) < 9*time.Minute {
		t.Fatalf("grant expires %v, want the rule's ten-minute cap", grant.ExpiresAt)
	}
	if _, err := st.FindAgentSandboxSecret(context.Background(), "project-1", testSandboxID, "GH_TOKEN"); err != nil {
		t.Fatalf("the credential was not bound for the agent: %v", err)
	}

	if len(judging.contents) != 1 {
		t.Fatalf("content asks = %+v, want one", judging.contents)
	}
	content := judging.contents[0]
	if content.Job.Purposes[0] != bound(prRule) || content.Job.Uses[0] != bound(prAsk) || content.RuleID != rule.ID || content.RequestID != req.ID {
		t.Fatalf("content ask = %+v, want the rule's allow as the authorization and the allow asked for as what is judged, each bound", content)
	}
	prompt, err := judge.Prompt(content.Job)
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if strings.Contains(prompt, "pre-approved") {
		t.Fatalf("the judge was shown the justification: %s", prompt)
	}
	// The judge is told about the discobox that asked, as it was created
	// (ADR 26-10-14-691): a rule may say "this discobox's repository" too.
	if facts := content.Job.Facts; len(facts) == 0 || facts[0] != "The discobox asking is "+testSandboxID+"." {
		t.Fatalf("facts = %q, want the requesting discobox stated", facts)
	}
}

// A rule approves or says nothing. Whatever finds it does not cover the
// request — a fact the server checks, the repository the discobox is of among
// them, the policy as code compares it, or the conditions the judge reads —
// the request stays pending for a person, exactly as it would with no rules,
// and the judge is asked only about conditions of a policy code already holds
// within the rule's.
func TestARuleThatDoesNotCoverLeavesTheRequestForAPerson(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rule         func(*services.CreateApprovalRuleBody)
		ask          func(*services.CreateSandboxCredentialRequestBody)
		judge        func(*ruleJudge)
		wantContents int
	}{
		{name: "hosts outside the rule's", wantContents: 0,
			rule: func(b *services.CreateApprovalRuleBody) { b.Hosts = []string{"api.github.com"} },
			ask:  func(b *services.CreateSandboxCredentialRequestBody) { b.Hosts = []string{"github.com"} }},
		{name: "another purpose", wantContents: 0,
			ask: func(b *services.CreateSandboxCredentialRequestBody) {
				b.Purpose = serverapi.NewOptCreateSandboxCredentialRequestBodyPurpose(serverapi.CreateSandboxCredentialRequestBodyPurposeDelegate)
			}},
		{name: "a discobox of another repository", wantContents: 0,
			rule: func(b *services.CreateApprovalRuleBody) { b.Repositories = []string{"github.com/org/elsewhere"} }},
		{name: "conditions outside the rule's", wantContents: 1,
			judge: func(j *ruleJudge) {
				j.content = func(services.RuleAsk) (judge.Answer, error) { return no("another base than main") }
			}},
		{name: "an allow outside the rule's", wantContents: 0,
			ask: func(b *services.CreateSandboxCredentialRequestBody) {
				b.Policy = apiPolicy(t, workerPolicy(t, issueAsk, prAsk, `allow delete on issue "43" in repository $primary.repo`))
			}},
		{name: "another repository than the discobox's", wantContents: 0,
			ask: func(b *services.CreateSandboxCredentialRequestBody) {
				b.Policy = apiPolicy(t, workerPolicy(t, `allow read on issue "43" in repository "org/elsewhere"`))
			}},
		{name: "a deny of the rule's left out", wantContents: 0,
			rule: func(b *services.CreateApprovalRuleBody) {
				b.Policy = rulePolicy(t, issueRule, prRule, "deny delete on anything in anywhere")
			}},
		{name: "a word defined otherwise", wantContents: 0,
			ask: func(b *services.CreateSandboxCredentialRequestBody) {
				p := workerPolicy(t, issueAsk)
				p.Definitions["issue"] = "Any GitHub issue, or anything else"
				b.Policy = apiPolicy(t, p)
			}},
		{name: "uses alone", wantContents: 0, ask: withoutPolicy},
		// Within every rule, since it matches nothing, and so nothing to approve.
		{name: "allows that resolve to nothing", wantContents: 0,
			ask: func(b *services.CreateSandboxCredentialRequestBody) {
				b.Policy = apiPolicy(t, workerPolicy(t, `allow read on issue "43" in repository $primary.upstream`))
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, st, judging, _ := ruledService(t)
			if tc.judge != nil {
				tc.judge(judging)
			}
			githubRule(t, svc, tc.rule)
			req := githubAsk(t, svc, tc.ask)
			if err := testRules(t, svc, req); err != nil {
				t.Fatalf("test rules: %v", err)
			}
			if now := requestNow(t, st, req); now.Status != model.SecretRequestStatusPending || now.GrantID != "" {
				t.Fatalf("request = %+v, want it left pending for a person", now)
			}
			if len(judging.contents) != tc.wantContents {
				t.Fatalf("judge was asked about %d contents, want %d", len(judging.contents), tc.wantContents)
			}
		})
	}
}

// Rules are tested one at a time, those that name the discobox's repository
// before those for every discobox, each on its own: never several rules'
// policies together. The first that covers the request ends it, and the grant names
// that rule.
func TestRulesAreTestedOneAtATimeAndTheFirstThatCoversEndsIt(t *testing.T) {
	svc, st, judging, _ := ruledService(t)
	every := forEveryDiscobox
	// Each rule's pull request reads otherwise, so each is a question of its own.
	conditioned := func(when string) string {
		return `allow create, update on pull request in repository $primary.repo when "` + when + `"`
	}
	githubRule(t, svc, func(b *services.CreateApprovalRuleBody) {
		every(b)
		b.Policy = rulePolicy(t, issueRule, conditioned("its head is discobox/*"))
	})
	wide := githubRule(t, svc, every)
	githubRule(t, svc, func(b *services.CreateApprovalRuleBody) {
		every(b)
		b.Policy = rulePolicy(t, issueRule, conditioned("its base is main"))
	})
	// Made last, tested first: it names the repository.
	named := githubRule(t, svc, func(b *services.CreateApprovalRuleBody) {
		b.Policy = rulePolicy(t, issueRule, conditioned("it is a draft"))
	})
	// And one for another repository, which is never put to the judge.
	githubRule(t, svc, func(b *services.CreateApprovalRuleBody) { b.Repositories = []string{"github.com/org/elsewhere"} })
	judging.content = func(ask services.RuleAsk) (judge.Answer, error) {
		if ask.RuleID == wide.ID {
			return judge.Answer{Allow: true, Reason: "within"}, nil
		}
		return no("not within")
	}
	req := githubAsk(t, svc, nil)
	if err := testRules(t, svc, req); err != nil {
		t.Fatalf("test rules: %v", err)
	}
	var asked []string
	for _, ask := range judging.contents {
		if len(ask.Job.Purposes) != 1 || ask.Job.Uses[0] != bound(prAsk) {
			t.Fatalf("ask = %+v, want one rule's allow against the allow asked for", ask)
		}
		asked = append(asked, ask.RuleID)
	}
	// The rest are tested oldest first, and rules made in one tick of the
	// clock have no order between them, so what is held is what must hold:
	// the rule naming the repository first, the one that covers last, and
	// none asked twice.
	if len(asked) < 2 || len(asked) > 4 || asked[0] != named.ID || asked[len(asked)-1] != wide.ID || len(slices.Compact(slices.Sorted(slices.Values(asked)))) != len(asked) {
		t.Fatalf("rules asked about = %v, want the one naming the repository (%s) first and none after the first that covers (%s)", asked, named.ID, wide.ID)
	}
	grant, err := st.GetSecretGrant(context.Background(), "project-1", requestNow(t, st, req).GrantID)
	if err != nil || grant.RuleID != wide.ID {
		t.Fatalf("grant = %+v, %v, want it minted under the rule that covered", grant, err)
	}
}

// A judge that could not answer is not a no. The request stays pending and
// the run fails, so it is tested again; a rule the judge did answer about is
// still acted on.
func TestAJudgeThatCannotAnswerIsAskedAgain(t *testing.T) {
	svc, st, judging, _ := ruledService(t)
	githubRule(t, svc, nil)
	judging.content = func(services.RuleAsk) (judge.Answer, error) {
		return judge.Answer{}, errors.New("the project's judge is not ready yet")
	}
	req := githubAsk(t, svc, nil)
	if err := testRules(t, svc, req); err == nil {
		t.Fatal("a run whose judge did not answer settled, so the request would never be tested again")
	}
	if now := requestNow(t, st, req); now.Status != model.SecretRequestStatusPending {
		t.Fatalf("request = %+v, want it pending", now)
	}
	judging.content = nil
	if err := testRules(t, svc, req); err != nil {
		t.Fatalf("test rules again: %v", err)
	}
	if now := requestNow(t, st, req); now.Status != model.SecretRequestStatusApproved {
		t.Fatalf("request = %+v, want it approved once the judge answered", now)
	}
}

// Only an agent's own ask for a grant is tested. A rule that is disabled or
// past its expiry is not tested at all, a rule for another repository or
// another credential is not this request's, and a request already answered is
// left as it was.
func TestWhatARuleIsNeverTestedAgainst(t *testing.T) {
	t.Run("a disabled rule", func(t *testing.T) {
		svc, st, judging, _ := ruledService(t)
		rule := githubRule(t, svc, nil)
		if _, err := svc.UpdateApprovalRule(testPrincipalContext(), "project-1", rule.ID, services.UpdateApprovalRuleBody{Enabled: serverapi.NewOptBool(false)}); err != nil {
			t.Fatalf("disable: %v", err)
		}
		req := githubAsk(t, svc, nil)
		if err := testRules(t, svc, req); err != nil {
			t.Fatal(err)
		}
		if requestNow(t, st, req).Status != model.SecretRequestStatusPending || len(judging.contents) != 0 {
			t.Fatal("a disabled rule was tested")
		}
	})
	t.Run("a rule past its expiry", func(t *testing.T) {
		svc, st, judging, _ := ruledService(t)
		rule := githubRule(t, svc, nil)
		lapsed := time.Now().Add(-time.Minute).UTC()
		rule.ExpiresAt = &lapsed
		if err := st.UpdateApprovalRule(context.Background(), rule); err != nil {
			t.Fatal(err)
		}
		req := githubAsk(t, svc, nil)
		if err := testRules(t, svc, req); err != nil {
			t.Fatal(err)
		}
		if requestNow(t, st, req).Status != model.SecretRequestStatusPending || len(judging.contents) != 0 {
			t.Fatal("a lapsed rule was tested")
		}
	})
	t.Run("a rule for another repository", func(t *testing.T) {
		svc, st, judging, _ := ruledService(t)
		githubRule(t, svc, func(b *services.CreateApprovalRuleBody) { b.Repositories = []string{"github.com/org/elsewhere"} })
		req := githubAsk(t, svc, nil)
		if err := testRules(t, svc, req); err != nil {
			t.Fatal(err)
		}
		if requestNow(t, st, req).Status != model.SecretRequestStatusPending || len(judging.contents) != 0 {
			t.Fatal("a rule for another repository was tested")
		}
	})
	t.Run("a free-form ask the rule's secret is not bound for", func(t *testing.T) {
		svc, st, judging, _ := ruledService(t)
		githubRule(t, svc, nil)
		req := workerRequest(t, svc, nil)
		if err := testRules(t, svc, req); err != nil {
			t.Fatal(err)
		}
		if requestNow(t, st, req).Status != model.SecretRequestStatusPending || len(judging.contents) != 0 {
			t.Fatal("an ask that names no well-known credential was answered by a secret bound nowhere")
		}
	})
	t.Run("a request the proxy opened", func(t *testing.T) {
		svc, st, judging, secret := ruledService(t)
		githubRule(t, svc, nil)
		req := &model.SecretRequest{
			ProjectID: "project-1", RequestedBy: "sandbox:" + testSandboxID, SandboxID: testSandboxID,
			Type: model.SecretTypeToken, Hosts: []string{"github.com"}, SecretID: secret.ID, Status: model.SecretRequestStatusPending,
		}
		if err := st.CreateSecretRequest(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if err := testRules(t, svc, req); err != nil {
			t.Fatal(err)
		}
		if requestNow(t, st, req).Status != model.SecretRequestStatusPending || len(judging.contents) != 0 {
			t.Fatal("a reactive request, which asks for no policy, was tested against a rule")
		}
	})
	t.Run("an ask with uses alone", func(t *testing.T) {
		svc, st, judging, _ := ruledService(t)
		rule := githubRule(t, svc, nil)
		req := githubAsk(t, svc, withoutPolicy)
		if err := testRules(t, svc, req); err != nil {
			t.Fatal(err)
		}
		if requestNow(t, st, req).Status != model.SecretRequestStatusPending || len(judging.contents) != 0 {
			t.Fatal("an ask with no policy was tested against a rule's")
		}
		seen, err := svc.GetSecretRequest(testPrincipalContext(), "project-1", req.ID)
		if err != nil || len(seen.RuleTests) != 0 {
			t.Fatalf("request = %+v, %v, want no rule %s on the card of an ask no rule answers", seen, err, rule.ID)
		}
	})
	t.Run("a request a person already answered", func(t *testing.T) {
		svc, st, judging, _ := ruledService(t)
		githubRule(t, svc, nil)
		req := githubAsk(t, svc, nil)
		if err := svc.DenySecretRequest(testPrincipalContext(), "project-1", req.ID); err != nil {
			t.Fatal(err)
		}
		if err := testRules(t, svc, req); err != nil {
			t.Fatal(err)
		}
		if requestNow(t, st, req).Status != model.SecretRequestStatusDenied || len(judging.contents) != 0 {
			t.Fatal("a denied request was tested, or approved over the person who denied it")
		}
	})
}

// The rule is read again in the approval's transaction. One disabled or
// edited while the judge was reading it is not one the grant is made under:
// nothing is minted, and the request waits.
func TestARuleChangedUnderTheJudgeApprovesNothing(t *testing.T) {
	for name, change := range map[string]services.UpdateApprovalRuleBody{
		"disabled": {Enabled: serverapi.NewOptBool(false)},
		"edited":   {Policy: serverapi.NewOptAccessPolicy(rulePolicy(t, issueRule, prRule, `allow update on issue "*" in repository $primary.repo`))},
		"widened":  {EveryDiscobox: serverapi.NewOptBool(true)},
		"moved":    {Repositories: []string{repo, "github.com/org/elsewhere"}},
	} {
		t.Run(name, func(t *testing.T) {
			svc, st, judging, _ := ruledService(t)
			rule := githubRule(t, svc, nil)
			judging.content = func(services.RuleAsk) (judge.Answer, error) {
				if _, err := svc.UpdateApprovalRule(testPrincipalContext(), "project-1", rule.ID, change); err != nil {
					t.Errorf("change the rule: %v", err)
				}
				return judge.Answer{Allow: true, Reason: "within"}, nil
			}
			req := githubAsk(t, svc, nil)
			if err := testRules(t, svc, req); err != nil {
				t.Fatalf("test rules: %v", err)
			}
			if now := requestNow(t, st, req); now.Status != model.SecretRequestStatusPending {
				t.Fatalf("request = %+v, want nothing approved under a rule that changed", now)
			}
			if grants, _ := st.ListSecretGrants(context.Background(), "project-1", "", rule.ID); len(grants) != 0 {
				t.Fatalf("grants = %+v, want none minted", grants)
			}
		})
	}
}

// A grant a rule minted is independent of it once made: deleting the rule
// revokes nothing, and its grants are still found by the rule.
func TestDeletingARuleRevokesNothing(t *testing.T) {
	svc, st, _, _ := ruledService(t)
	rule := githubRule(t, svc, nil)
	req := githubAsk(t, svc, nil)
	if err := testRules(t, svc, req); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteApprovalRule(testPrincipalContext(), "project-1", rule.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	grants, err := svc.ListSecretGrants(testPrincipalContext(), "project-1", "", rule.ID)
	if err != nil || len(grants) != 1 || grants[0].ID != requestNow(t, st, req).GrantID {
		t.Fatalf("grants by rule = %+v, %v, want the one it minted, still standing", grants, err)
	}
	if _, err := svc.GetApprovalRule(testPrincipalContext(), "project-1", rule.ID); !isStatus(err, http.StatusNotFound) {
		t.Fatalf("get after delete = %v, want 404", err)
	}
}

// Which discoboxes a rule is for is compared, never read: a rule that names
// repositories answers a discobox whose primary source is one of them —
// whichever way the remote is spelled, and for a local checkout the remote it
// tracks — and no other. Its other sources never count, a local source with
// no remote is nobody's, and only a rule for every discobox answers that one.
func TestARuleIsForDiscoboxesOfItsRepositories(t *testing.T) {
	local, ssh, fork := "/home/me/src/repo", "git@GitHub.com:org/repo.git", "https://github.com/someone-else/fork"
	credentialed := "https://me:hunter2@github.com/org/repo/" //nolint:gosec // G101: a made-up password, there to be dropped.
	for _, tc := range []struct {
		name         string
		source       *model.GitSource
		others       model.SourceCodeReferences
		repositories []string
		approved     bool
	}{
		{name: "the same repository over https", source: remote(repoURL), repositories: []string{repo}, approved: true},
		{name: "the same repository over ssh", source: remote(ssh), repositories: []string{repo}, approved: true},
		{name: "a URL carrying credentials", source: remote(credentialed), repositories: []string{repo}, approved: true},
		{name: "one of several the rule names", source: remote(repoURL), repositories: []string{"github.com/org/elsewhere", repo}, approved: true},
		{name: "a local checkout tracking it", source: &model.GitSource{Kind: "git", LocalDirectory: &local, UpstreamURL: &ssh}, repositories: []string{repo}, approved: true},
		{name: "another repository", source: remote(fork), repositories: []string{repo}},
		// What a box cloned is the repository it is of: an upstream named
		// beside a URL is a repository it does not hold.
		{name: "a fork claiming the repository as its upstream", source: &model.GitSource{Kind: "git", URL: &fork, UpstreamURL: &ssh}, repositories: []string{repo}},
		{name: "the repository as an additional source only", source: remote(fork),
			others: model.SourceCodeReferences{"repo": *remote(repoURL)}, repositories: []string{repo}},
		{name: "a local source with no remote", source: &model.GitSource{Kind: "git", LocalDirectory: &local}, repositories: []string{repo}},
		{name: "no source at all", repositories: []string{repo}},
		{name: "a local source with no remote, and a rule for every discobox", source: &model.GitSource{Kind: "git", LocalDirectory: &local}, approved: true},
		{name: "no source at all, and a rule for every discobox", approved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, st, judging, _ := ruledService(t)
			sandbox, err := st.GetSandbox(context.Background(), "project-1", testSandboxID)
			if err != nil {
				t.Fatal(err)
			}
			sandbox.Source, sandbox.SourceCodeReferences = tc.source, tc.others
			if err := st.UpdateSandbox(context.Background(), sandbox); err != nil {
				t.Fatal(err)
			}
			// The repository written out, as a discobox with none resolves no
			// variable for it, and which discoboxes the rule is for is the
			// question here.
			rule := githubRule(t, svc, func(b *services.CreateApprovalRuleBody) {
				b.Policy = rulePolicy(t, bound(issueRule))
				if b.Repositories = tc.repositories; len(tc.repositories) == 0 {
					forEveryDiscobox(b)
				}
			})
			req := githubAsk(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
				b.Policy = apiPolicy(t, workerPolicy(t, bound(issueAsk)))
			})
			if err := testRules(t, svc, req); err != nil {
				t.Fatalf("test rules: %v", err)
			}
			now := requestNow(t, st, req)
			if approved := now.Status == model.SecretRequestStatusApproved; approved != tc.approved {
				t.Fatalf("request = %+v, want approved %v", now, tc.approved)
			}
			if tc.approved {
				return
			}
			// No judge reads which discoboxes a rule is for, and nothing is
			// recorded for a comparison.
			if len(judging.contents) != 0 {
				t.Fatalf("the judge was asked about a rule that is not for this discobox: %+v", judging.contents)
			}
			if verdicts, err := st.ListCredentialVerdicts(context.Background(), "project-1", store.CredentialVerdictFilter{}); err != nil || len(verdicts) != 0 {
				t.Fatalf("verdicts = %+v, %v, want none recorded for a comparison", verdicts, err)
			}
			seen, err := svc.GetSecretRequest(testPrincipalContext(), "project-1", req.ID)
			if err != nil || len(seen.RuleTests) != 1 {
				t.Fatalf("request = %+v, %v, want the rule on its card", seen, err)
			}
			test := seen.RuleTests[0]
			if test.RuleID != rule.ID || test.Covered || test.Pending || test.Stage != model.RuleTestStageFacts ||
				!strings.Contains(test.Reason, repo) || strings.Contains(test.Reason, "hunter2") {
				t.Fatalf("test = %+v, want the repository named as the fact that failed", test)
			}
		})
	}
}

// A rule's repositories are stored as host/path however they were given, said
// once each, and a bare org/repo is refused for naming no host.
func TestARulesRepositoriesAreStoredAsHostAndPath(t *testing.T) {
	svc, _, _, _ := ruledService(t)
	ctx := testPrincipalContext()
	rule := githubRule(t, svc, func(b *services.CreateApprovalRuleBody) {
		b.Repositories = []string{
			" https://me@GitHub.com/org/repo.git/ ", "git@github.com:org/other.git", "ssh://git@git.example.com:2222/team/thing",
			"gitlab.example.com/group/sub/project/", "git.example.com:8443/team/thing",
		}
	})
	want := []string{repo, "github.com/org/other", "git.example.com:2222/team/thing", "gitlab.example.com/group/sub/project", "git.example.com:8443/team/thing"}
	if !slices.Equal(rule.Repositories, want) {
		t.Fatalf("repositories = %q, want %q", rule.Repositories, want)
	}
	// What is stored reads back as itself at every edit.
	edited, err := svc.UpdateApprovalRule(ctx, "project-1", rule.ID, services.UpdateApprovalRuleBody{Enabled: serverapi.NewOptBool(false)})
	if err != nil || !slices.Equal(edited.Repositories, want) {
		t.Fatalf("edit = %+v, %v, want the repositories untouched by an edit that did not name them", edited, err)
	}
	// A rule for every discobox is an edit that says so. A list sent empty
	// is refused, never read as the wider rule, and so is saying both.
	for name, body := range map[string]services.UpdateApprovalRuleBody{
		"an empty list":                    {Repositories: []string{}},
		"repositories and every discobox":  {Repositories: []string{repo}, EveryDiscobox: serverapi.NewOptBool(true)},
		"an empty list and every discobox": {Repositories: []string{}, EveryDiscobox: serverapi.NewOptBool(true)},
	} {
		if _, err := svc.UpdateApprovalRule(ctx, "project-1", rule.ID, body); !isStatus(err, http.StatusBadRequest) {
			t.Errorf("an edit sending %s = %v, want a 400", name, err)
		}
	}
	if kept, err := svc.GetApprovalRule(ctx, "project-1", rule.ID); err != nil || !slices.Equal(kept.Repositories, want) {
		t.Fatalf("rule = %+v, %v, want its repositories as they were after the refused edits", kept, err)
	}
	if wide, err := svc.UpdateApprovalRule(ctx, "project-1", rule.ID, services.UpdateApprovalRuleBody{EveryDiscobox: serverapi.NewOptBool(true)}); err != nil || len(wide.Repositories) != 0 {
		t.Fatalf("edit = %+v, %v, want a rule for every discobox", wide, err)
	}
	// Saying nothing of which discoboxes makes no rule, and neither does
	// saying both.
	for name, body := range map[string]services.CreateApprovalRuleBody{
		"nothing said":                    {},
		"an empty list":                   {Repositories: []string{}},
		"every discobox said false":       {EveryDiscobox: serverapi.NewOptBool(false)},
		"repositories and every discobox": {Repositories: []string{repo}, EveryDiscobox: serverapi.NewOptBool(true)},
	} {
		body.WellKnownId, body.Policy = serverapi.NewOptString(wellknown.GitHubAPI), rulePolicy(t, issueRule)
		if _, err := svc.CreateApprovalRule(ctx, "project-1", body); !isStatus(err, http.StatusBadRequest) {
			t.Errorf("a rule made with %s = %v, want a 400", name, err)
		}
	}
	for name, given := range map[string][]string{
		"a bare org/repo":           {"org/repo"},
		"a bare name":               {"repo"},
		"a host with no path":       {"github.com"},
		"a URL with no path":        {"https://github.com/"},
		"an empty entry":            {repo, " "},
		"a path on a machine":       {"/home/me/src/repo"},
		"a file URL":                {"file:///home/me/src/repo"},
		"one repository said twice": {repo, "git@github.com:org/repo.git"},
		"something on two lines":    {"github.com/org/repo\ngithub.com/org/other"},
		"more than a rule may name": slices.Repeat([]string{"x"}, 33),
		"a repository too long":     {"github.com/" + strings.Repeat("x", 600)},
	} {
		_, err := svc.CreateApprovalRule(ctx, "project-1", services.CreateApprovalRuleBody{
			WellKnownId: serverapi.NewOptString(wellknown.GitHubAPI), Policy: rulePolicy(t, issueRule), Repositories: given,
		})
		if !isStatus(err, http.StatusBadRequest) {
			t.Errorf("a rule for %s = %v, want a 400", name, err)
		}
		if name == "a bare org/repo" && (err == nil || !strings.Contains(err.Error(), "github.com/org/repo")) {
			t.Errorf("a bare org/repo = %v, want to be told to name the host", err)
		}
	}
}

// Answering a request with "auto approve" makes the rule and approves the
// request under it in one write. The rule's credential, hosts and purpose are
// the request's, copied by the server; its policy is what the person chose,
// held to covering the request before anything is made; and with no
// repositories named it is for the repository of the discobox that asked,
// never for more.
func TestAutoApproveMakesTheRuleAndApprovesUnderIt(t *testing.T) {
	svc, st, judging, secret := ruledService(t)
	dirty := &marks{}
	svc.SetDirtier(dirty)
	req := githubAsk(t, svc, nil)
	approved, err := svc.ApproveSecretRequest(testPrincipalContext(), "project-1", req.ID, services.ApproveSecretRequestBody{
		GrantTTLSeconds: serverapi.NewOptInt64(1800),
		Policy:          askedPolicy(t),
		Rule:            serverapi.NewOptApproveWithRule(serverapi.ApproveWithRule{Policy: serverapi.NewOptAccessPolicy(rulePolicy(t, issueRule, prRule))}),
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	rules, err := svc.ListApprovalRules(testPrincipalContext(), "project-1")
	if err != nil || len(rules) != 1 {
		t.Fatalf("rules = %+v, %v, want the one the approval made", rules, err)
	}
	rule := rules[0]
	if rule.WellKnownID != wellknown.GitHubAPI || rule.SecretID != "" || !slices.Equal(rule.Hosts, []string{"github.com"}) ||
		rule.Purpose != model.SecretGrantPurposeUse || !slices.Equal(rule.Repositories, []string{repo}) ||
		!slices.Equal(sentencesOf(rule.Policy), []string{issueRule, prRule}) || rule.Policy.Resolved != nil {
		t.Fatalf("rule = %+v, want the request's credential, hosts and purpose, the asking discobox's repository, and the chosen policy, resolved for nobody", rule)
	}
	if !rule.Enabled || rule.CreatedBy != "user-1" || rule.MaxGrantTTL != 1800 {
		t.Fatalf("rule = %+v, want it enabled, the approver's, and capped at the lifetime this approval granted", rule)
	}
	grant, err := st.GetSecretGrant(context.Background(), "project-1", approved.GrantID)
	if err != nil || grant.RuleID != rule.ID || grant.SecretID != secret.ID || grant.GrantedBy != "user-1" {
		t.Fatalf("grant = %+v, %v, want it minted under the new rule by the approver", grant, err)
	}
	if got := useDescriptions(grant.Uses); !slices.Equal(got, []string{issueUse}) {
		t.Fatalf("grant uses = %v, want the uses as asked", got)
	}
	if got := sentencesOf(grant.Policy); !slices.Equal(got, []string{issueAsk, prAsk}) {
		t.Fatalf("grant policy = %q, want the policy as approved, not the rule's class", got)
	}
	// Its conditions judged as a candidate before it was a rule, then read
	// once more under its ID, which is what records its verdict as the rule's.
	if len(judging.contents) != 2 || judging.contents[0].RuleID != "" || judging.contents[1].RuleID != rule.ID {
		t.Fatalf("asks = %+v, want the candidate judged, then read again naming the rule", judging.contents)
	}
	// The new rule answers what else is pending, and is a reason to keep a judge.
	if !slices.Contains(dirty.marked, resourcejudges.JudgeResourceType+"/project-1") {
		t.Fatalf("marked = %v, want the project's judge looked at again", dirty.marked)
	}
}

// A rule that would not cover the very request it is made from is refused
// rather than made: nothing is minted, the request stays pending, and the
// refusal says what failed. So is one that does not say which discoboxes it
// is for when the asking discobox has no repository to default to: a rule for
// every discobox is said, never assumed.
func TestAutoApproveRefusesARuleThatWouldNotCoverItsOwnRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		judge  func(*ruleJudge)
		source *model.GitSource
		body   serverapi.ApproveWithRule
		want   string
	}{
		{name: "repositories the discobox is not of", want: "github.com/org/elsewhere", source: remote(repoURL),
			body: serverapi.ApproveWithRule{Repositories: []string{"github.com/org/elsewhere"}}},
		{name: "conditions that do not hold the ask", want: "another base", source: remote(repoURL),
			judge: func(j *ruleJudge) {
				j.content = func(services.RuleAsk) (judge.Answer, error) { return no("another base than main") }
			},
			body: serverapi.ApproveWithRule{Policy: serverapi.NewOptAccessPolicy(rulePolicy(t, issueRule, prRule))}},
		{name: "a policy that does not hold the ask", want: "not within any allow", source: remote(repoURL),
			body: serverapi.ApproveWithRule{Policy: serverapi.NewOptAccessPolicy(rulePolicy(t, issueRule))}},
		{name: "a policy a rule may not have", want: "has none", source: remote(repoURL),
			body: serverapi.ApproveWithRule{Policy: serverapi.NewOptAccessPolicy(rulePolicy(t, "deny delete on anything in anywhere"))}},
		{name: "repositories and every discobox at once", want: "not both", source: remote(repoURL),
			body: serverapi.ApproveWithRule{Repositories: []string{repo}, EveryDiscobox: serverapi.NewOptBool(true)}},
		{name: "a bare org/repo", want: "names no host", source: remote(repoURL),
			body: serverapi.ApproveWithRule{Repositories: []string{"org/repo"}}},
		{name: "nothing said, from a discobox with no repository", want: "every discobox", body: serverapi.ApproveWithRule{}},
		{name: "a repository named for a discobox with none", want: "no repository",
			body: serverapi.ApproveWithRule{Repositories: []string{repo}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, st, judging, _ := ruledService(t)
			ofRepository(t, st, testSandboxID, tc.source)
			if tc.judge != nil {
				tc.judge(judging)
			}
			req := githubAsk(t, svc, nil)
			_, err := svc.ApproveSecretRequest(testPrincipalContext(), "project-1", req.ID, services.ApproveSecretRequestBody{
				Policy: askedPolicy(t),
				Rule:   serverapi.NewOptApproveWithRule(tc.body),
			})
			if !isStatus(err, http.StatusBadRequest) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("approve = %v, want a 400 saying %q", err, tc.want)
			}
			if now := requestNow(t, st, req); now.Status != model.SecretRequestStatusPending {
				t.Fatalf("request = %+v, want it pending", now)
			}
			if rules, _ := st.ListApprovalRules(context.Background(), "project-1"); len(rules) != 0 {
				t.Fatalf("rules = %+v, want none made", rules)
			}
		})
	}
}

// A rule for every discobox is made only by saying so, and then answers a
// discobox with no repository too.
func TestAutoApproveForEveryDiscoboxIsSaid(t *testing.T) {
	svc, st, _, _ := ruledService(t)
	ofRepository(t, st, testSandboxID, nil)
	// The repository written out: a discobox with none resolves no variable.
	asked := apiPolicy(t, workerPolicy(t, bound(issueAsk), bound(prAsk)))
	req := githubAsk(t, svc, func(b *services.CreateSandboxCredentialRequestBody) { b.Policy = asked })
	approved, err := svc.ApproveSecretRequest(testPrincipalContext(), "project-1", req.ID, services.ApproveSecretRequestBody{
		Policy: asked,
		Rule:   serverapi.NewOptApproveWithRule(serverapi.ApproveWithRule{EveryDiscobox: serverapi.NewOptBool(true)}),
	})
	if err != nil || approved.Status != model.SecretRequestStatusApproved {
		t.Fatalf("approve = %+v, %v, want it approved under a rule for every discobox", approved, err)
	}
	rules, err := st.ListApprovalRules(context.Background(), "project-1")
	if err != nil || len(rules) != 1 || len(rules[0].Repositories) != 0 || !slices.Equal(sentencesOf(rules[0].Policy), []string{bound(issueAsk), bound(prAsk)}) {
		t.Fatalf("rules = %+v, %v, want one rule naming no repository, with the policy as asked", rules, err)
	}
}

// A rule is a person's. A discobox never makes, edits, reads or lists one,
// never asks for proposals, and cannot make one by approving a request it
// owns.
func TestADiscoboxNeverTouchesARule(t *testing.T) {
	svc, st, _, secret := ruledService(t)
	rule := githubRule(t, svc, nil)
	delegate(t, st, secret, "github.com", 0)
	req := githubAsk(t, svc, nil)
	lead := asLead()

	if _, err := svc.ListApprovalRules(lead, "project-1"); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("list = %v, want 403", err)
	}
	if _, err := svc.GetApprovalRule(lead, "project-1", rule.ID); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("get = %v, want 403", err)
	}
	if _, err := svc.CreateApprovalRule(lead, "project-1", services.CreateApprovalRuleBody{
		WellKnownId: serverapi.NewOptString(wellknown.GitHubAPI), Policy: rulePolicy(t, issueRule),
	}); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("create = %v, want 403", err)
	}
	if _, err := svc.UpdateApprovalRule(lead, "project-1", rule.ID, services.UpdateApprovalRuleBody{Policy: serverapi.NewOptAccessPolicy(rulePolicy(t, issueRule))}); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("update = %v, want 403", err)
	}
	if err := svc.DeleteApprovalRule(lead, "project-1", rule.ID); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("delete = %v, want 403", err)
	}
	if _, err := svc.ProposeApprovalRules(lead, "project-1", req.ID); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("propose = %v, want 403", err)
	}
	if _, err := svc.ApproveSecretRequest(lead, "project-1", req.ID, services.ApproveSecretRequestBody{
		Policy: askedPolicy(t),
		Rule:   serverapi.NewOptApproveWithRule(serverapi.ApproveWithRule{}),
	}); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("approve with a rule = %v, want 403", err)
	}
	// And what a person reads on a request about its rules is not shown to it.
	seen, err := svc.GetSecretRequest(lead, "project-1", req.ID)
	if err != nil || len(seen.RuleTests) != 0 {
		t.Fatalf("request as the discobox reads it = %+v, %v, want no rule tests", seen, err)
	}
}

// A rule is held to what one may be when it is made and at every edit: one
// credential, hosts it may go to, and a policy that validates as a grant's
// does and allows something. The values a policy's variables resolved to are
// never a rule's: they bind to whichever discobox asks.
func TestWhatARuleMayBe(t *testing.T) {
	svc, _, _, secret := ruledService(t)
	ctx := testPrincipalContext()
	github := serverapi.NewOptString(wellknown.GitHubAPI)
	issues := rulePolicy(t, issueRule)
	for name, body := range map[string]services.CreateApprovalRuleBody{
		"no credential":               {Policy: issues},
		"two credentials":             {WellKnownId: github, SecretId: serverapi.NewOptString(secret.ID), Policy: issues},
		"an unknown well-known ID":    {WellKnownId: serverapi.NewOptString("com.example.nope"), Policy: issues},
		"no policy":                   {WellKnownId: github},
		"denies alone":                {WellKnownId: github, Policy: rulePolicy(t, "deny delete on anything in anywhere")},
		"a word with no definition":   {WellKnownId: github, Policy: rulePolicy(t, `allow push on branch "main" in repository $primary.repo`)},
		"an allow that says no scope": {WellKnownId: github, Policy: rulePolicy(t, `allow read on issue "*"`)},
		"a host the ID is not for":    {WellKnownId: github, Policy: issues, Hosts: []string{"evil.example"}},
		"a negative cap":              {WellKnownId: github, Policy: issues, MaxGrantTTLSeconds: serverapi.NewOptInt64(-1)},
		"an expiry already past":      {WellKnownId: github, Policy: issues, ExpiresAt: serverapi.NewOptDateTime(time.Now().Add(-time.Hour))},
		"a purpose nobody defined":    {WellKnownId: github, Policy: issues, Purpose: serverapi.NewOptCreateApprovalRuleBodyPurpose("own")},
	} {
		// Each is refused for what it names, not for saying nothing of
		// which discoboxes it is for.
		body.Repositories = []string{repo}
		if _, err := svc.CreateApprovalRule(ctx, "project-1", body); err == nil {
			t.Errorf("a rule with %s was made", name)
		}
	}
	rule := githubRule(t, svc, func(b *services.CreateApprovalRuleBody) {
		b.Policy = issues
		b.Policy.Resolved = serverapi.NewOptAccessPolicyResolved(serverapi.AccessPolicyResolved{policy.VarPrimaryRepo: "org/elsewhere"})
	})
	if !slices.Equal(rule.Hosts, []string{"github.com"}) || !slices.Equal(sentencesOf(rule.Policy), []string{issueRule}) || rule.Policy.Resolved != nil ||
		rule.MaxGrantTTL != 3600 || !rule.Enabled || rule.Purpose != model.SecretGrantPurposeUse {
		t.Fatalf("rule = %+v, want the ID's host, the policy with no resolved values, an hour's cap, enabled, to use", rule)
	}
	if _, err := svc.UpdateApprovalRule(ctx, "project-1", rule.ID, services.UpdateApprovalRuleBody{
		Policy: serverapi.NewOptAccessPolicy(rulePolicy(t, "deny delete on anything in anywhere")),
	}); err == nil {
		t.Fatal("an edit left a rule that allows nothing")
	}
	edited, err := svc.UpdateApprovalRule(ctx, "project-1", rule.ID, services.UpdateApprovalRuleBody{
		MaxGrantTTLSeconds: serverapi.NewOptInt64(0), Enabled: serverapi.NewOptBool(false),
	})
	if err != nil || edited.MaxGrantTTL != 0 || edited.Enabled || !slices.Equal(sentencesOf(edited.Policy), []string{issueRule}) {
		t.Fatalf("edit = %+v, %v, want only what was sent changed, zeros included", edited, err)
	}
	stored, err := svc.GetApprovalRule(ctx, "project-1", rule.ID)
	if err != nil || stored.MaxGrantTTL != 0 || stored.Enabled {
		t.Fatalf("stored = %+v, %v, want the zero cap and the disabled rule kept", stored, err)
	}
}

// A rule may name a project secret instead of a well-known ID, and then
// answers an ask that names no ID by the hosts its secret is bound for.
func TestARuleNamingASecretAnswersAFreeFormAskItsBindingCovers(t *testing.T) {
	svc, st := newAgentCredentialService(t)
	judging := &ruleJudge{}
	svc.SetJudge(judging)
	ctx := testPrincipalContext()
	secret := createBoundSecret(ctx, t, svc, "github", "github.com", 3600)
	rule, err := svc.CreateApprovalRule(ctx, "project-1", services.CreateApprovalRuleBody{
		SecretId: serverapi.NewOptString(secret.ID), EveryDiscobox: serverapi.NewOptBool(true),
		Policy: rulePolicy(t, `allow read on issue in repository "acme/web" when "it is labeled"`),
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	if !slices.Equal(rule.Hosts, []string{"github.com"}) {
		t.Fatalf("rule hosts = %v, want the secret's binding", rule.Hosts)
	}
	req := workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
		b.Policy = apiPolicy(t, workerPolicy(t, `allow read on issue in repository "acme/web" when "it is labeled bug"`))
	})
	if err := testRules(t, svc, req); err != nil {
		t.Fatal(err)
	}
	now := requestNow(t, st, req)
	if now.Status != model.SecretRequestStatusApproved || now.SecretID != secret.ID {
		t.Fatalf("request = %+v, want it approved with the rule's secret", now)
	}
	// The secret's name is not read back to the judge about an agent's ask.
	if len(judging.contents) != 1 || judging.contents[0].Job.Credential != "" {
		t.Fatalf("content ask = %+v, want no credential named for a rule that names a secret", judging.contents[0])
	}
}

// A rule does not hand on a delegation yet: a delegation's uses still bound
// a hand-off of uses alone, and a rule holds only a request's policy, so one
// it minted would carry uses nobody read (ADR 26-10-22-689 §4). Neither a
// rule made directly nor one made by auto approving a delegation is made.
func TestARuleDoesNotHandOnADelegation(t *testing.T) {
	svc, st, _, _ := ruledService(t)
	_, err := svc.CreateApprovalRule(testPrincipalContext(), "project-1", services.CreateApprovalRuleBody{
		WellKnownId: serverapi.NewOptString(wellknown.GitHubAPI), Repositories: []string{repo}, Policy: rulePolicy(t, issueRule, prRule),
		Purpose: serverapi.NewOptCreateApprovalRuleBodyPurpose(serverapi.CreateApprovalRuleBodyPurposeDelegate),
	})
	if !isStatus(err, http.StatusBadRequest) || !strings.Contains(err.Error(), "delegation") {
		t.Fatalf("create a delegating rule = %v, want a 400 saying a rule does not delegate", err)
	}
	req := githubAsk(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
		b.Purpose = serverapi.NewOptCreateSandboxCredentialRequestBodyPurpose(serverapi.CreateSandboxCredentialRequestBodyPurposeDelegate)
	})
	_, err = svc.ApproveSecretRequest(testPrincipalContext(), "project-1", req.ID, services.ApproveSecretRequestBody{
		Policy: askedPolicy(t),
		Rule:   serverapi.NewOptApproveWithRule(serverapi.ApproveWithRule{}),
	})
	if !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("auto approve a delegation = %v, want a 400", err)
	}
	if rules, _ := st.ListApprovalRules(context.Background(), "project-1"); len(rules) != 0 || requestNow(t, st, req).Status != model.SecretRequestStatusPending {
		t.Fatal("a rule was made, or the delegation approved under one")
	}
	if _, err := svc.ProposeApprovalRules(testPrincipalContext(), "project-1", req.ID); !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("propose for a delegation = %v, want a 400", err)
	}
	// Nor is one proposed from a delegation grant, before any model is asked.
	secret, err := st.FindSecretByWellKnownID(context.Background(), "project-1", wellknown.GitHubAPI)
	if err != nil {
		t.Fatal(err)
	}
	delegation := delegatePolicy(t, st, secret, "github.com", issueRule)
	if _, err := svc.ProposeApprovalRuleFromGrant(testPrincipalContext(), "project-1", delegation.ID); !isStatus(err, http.StatusBadRequest) || !strings.Contains(err.Error(), "delegat") {
		t.Fatalf("propose from a delegation grant = %v, want a 400 saying no rule delegates", err)
	}
}

// A proposal is one policy, written by the project's judge from the
// request's as the same work on any target of its kind, and held to covering
// the request as a rule later would be — by code, and by the judge for the
// conditions — what that found carried with it. The definitions are the
// request's, for the words the policy uses; the credential, hosts and purpose
// are the request's own; and the repository the rule may be for is the
// asking discobox's, compared and never written by a model.
func TestAutoApproveProposesOnePolicyAlreadyHeldToTheRequest(t *testing.T) {
	svc, st, judging, _ := ruledService(t)
	judging.proposal = judge.Proposal{StatementsAnyTarget: []string{issueRule, prRule}}
	judging.content = func(services.RuleAsk) (judge.Answer, error) {
		return no("the rule says nothing of drafts")
	}
	req := githubAsk(t, svc, nil)
	proposals, err := svc.ProposeApprovalRules(testPrincipalContext(), "project-1", req.ID)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if proposals.Credential.Or("") != wellknown.GitHubAPI || !slices.Equal(proposals.Hosts, []string{"github.com"}) || proposals.Purpose != "use" {
		t.Fatalf("proposals = %+v, want the request's credential, hosts and purpose", proposals)
	}
	if proposals.Repository.Or("") != repo {
		t.Fatalf("repository = %q, want the asking discobox's, as a rule stores it", proposals.Repository.Or(""))
	}
	written, ok := proposals.Policy.Get()
	shown, shownOK := written.Policy.Get()
	if !ok || !shownOK || written.Covers || written.Refused.IsSet() || !strings.Contains(written.Reason.Or(""), "drafts") {
		t.Fatalf("policy = %+v, want the written policy with the judge's verdict on its conditions", proposals.Policy)
	}
	parsed, err := services.PolicyFromAPI(serverapi.NewOptAccessPolicy(shown))
	if err != nil || !slices.Equal(sentencesOf(parsed), []string{issueRule, prRule}) || len(parsed.Definitions) != 3 {
		t.Fatalf("written policy = %+v, %v, want the statements written, with the request's definitions of their words", parsed, err)
	}
	// One model call, and one judge call: code settled the issue.
	if len(judging.proposed) != 1 || len(judging.contents) != 1 {
		t.Fatalf("judge was asked for %d proposals and %d contents", len(judging.proposed), len(judging.contents))
	}
	job := judging.proposed[0]
	if !slices.Equal(job.Statements, []string{issueAsk, prAsk}) || len(job.Definitions) != 3 || job.Credential != wellknown.GitHubAPI || !slices.Equal(job.Hosts, []string{"github.com"}) {
		t.Fatalf("proposal job = %+v, want the request's statements and definitions, and what they are about", job)
	}
	if ask := judging.contents[0]; ask.RuleID != "" || ask.RequestID != req.ID || ask.Job.Purposes[0] != bound(prRule) {
		t.Fatalf("content ask = %+v, want the written policy's conditions judged for this request before it is a rule", ask)
	}

	// A discobox with no repository has none to offer.
	ofRepository(t, st, testSandboxID, nil)
	if none, err := svc.ProposeApprovalRules(testPrincipalContext(), "project-1", req.ID); err != nil || none.Repository.IsSet() {
		t.Fatalf("proposals = %+v, %v, want no repository for a discobox that has none", none, err)
	}
}

// Where no model can write, there is no policy, and the note says why. A
// server with no judge at all says so too: a rule still approves what code
// alone finds within it, so the person starts from the policy as asked.
func TestWithNoModelToWriteThereIsNoPolicy(t *testing.T) {
	svc, _, judging, _ := ruledService(t)
	judging.proposeErr = errors.New("this project has no judge: the project has no default harness")
	req := githubAsk(t, svc, nil)
	proposals, err := svc.ProposeApprovalRules(testPrincipalContext(), "project-1", req.ID)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if proposals.Policy.IsSet() || proposals.Repository.Or("") != repo || !strings.Contains(proposals.Note.Or(""), "no default harness") {
		t.Fatalf("proposals = %+v, want no policy, the repository all the same, and a note saying why", proposals)
	}
	if len(judging.contents) != 0 {
		t.Fatalf("the judge was asked about %d contents, want none with nothing written", len(judging.contents))
	}
	svc.SetJudge(nil)
	if none, err := svc.ProposeApprovalRules(testPrincipalContext(), "project-1", req.ID); err != nil || none.Policy.IsSet() || !strings.Contains(none.Note.Or(""), "as asked") {
		t.Fatalf("proposals = %+v, %v, want a note to start from the policy as asked", none, err)
	}
}

// A grant a request was approved with proposes the rule auto approve would
// have, written from the policy the grant approved — which approving may
// have narrowed — and held to covering it. A grant with no policy, and one no
// request on record answered, have nothing to propose from.
func TestAGrantProposesTheRuleItsRequestWouldHave(t *testing.T) {
	svc, st, judging, _ := ruledService(t)
	judging.proposal = judge.Proposal{StatementsAnyTarget: []string{issueRule}}
	req := githubAsk(t, svc, nil)
	approved, err := svc.ApproveSecretRequest(testPrincipalContext(), "project-1", req.ID, services.ApproveSecretRequestBody{
		Policy: shownAs(t, workerPolicy(t, issueAsk)),
	})
	if err != nil || approved.GrantID == "" {
		t.Fatalf("approve = %+v, %v", approved, err)
	}
	proposals, err := svc.ProposeApprovalRuleFromGrant(testPrincipalContext(), "project-1", approved.GrantID)
	if err != nil {
		t.Fatalf("propose from grant: %v", err)
	}
	if proposals.Credential.Or("") != wellknown.GitHubAPI || proposals.WellKnownId.Or("") != wellknown.GitHubAPI ||
		!slices.Equal(proposals.Hosts, []string{"github.com"}) || proposals.Purpose != "use" || proposals.Repository.Or("") != repo {
		t.Fatalf("proposals = %+v, want the grant's credential, hosts and purpose, and the asking discobox's repository", proposals)
	}
	written, ok := proposals.Policy.Get()
	if !ok || !written.Covers || !written.Policy.IsSet() {
		t.Fatalf("policy = %+v, want the written policy, found to cover", proposals.Policy)
	}
	if job := judging.proposed[0]; !slices.Equal(job.Statements, []string{issueAsk}) {
		t.Fatalf("proposal job statements = %q, want the grant's approved policy", job.Statements)
	}
	// The issue is code's to settle, so nobody is asked.
	if len(judging.contents) != 0 {
		t.Fatalf("content asks = %+v, want none for a policy code holds within the rule's", judging.contents)
	}

	// A discobox with no repository: none to offer, and still a proposal.
	ofRepository(t, st, testSandboxID, nil)
	if none, err := svc.ProposeApprovalRuleFromGrant(testPrincipalContext(), "project-1", approved.GrantID); err != nil || none.Repository.IsSet() {
		t.Fatalf("proposals = %+v, %v, want no repository for a discobox that has none", none, err)
	}

	// A grant of uses alone has no policy to propose from.
	alone := githubAsk(t, svc, withoutPolicy)
	usesOnly, err := svc.ApproveSecretRequest(testPrincipalContext(), "project-1", alone.ID, services.ApproveSecretRequestBody{})
	if err != nil {
		t.Fatalf("approve uses alone: %v", err)
	}
	if _, err := svc.ProposeApprovalRuleFromGrant(testPrincipalContext(), "project-1", usesOnly.GrantID); !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("propose from a grant of uses alone = %v, want 400", err)
	}

	// A sandbox may not ask.
	if _, err := svc.ProposeApprovalRuleFromGrant(auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypeSandbox, SandboxID: testSandboxID, ProjectID: "project-1"}), "project-1", approved.GrantID); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("propose as a discobox = %v, want 403", err)
	}
	if _, err := svc.ProposeApprovalRuleFromGrant(testPrincipalContext(), "project-1", "grant_missing"); !isStatus(err, http.StatusNotFound) {
		t.Fatalf("propose from a missing grant = %v, want 404", err)
	}
}

// Nothing is proposed for a request a rule could not answer.
func TestOnlyAnAgentsPendingAskHasProposals(t *testing.T) {
	svc, _, _, _ := ruledService(t)
	req := githubAsk(t, svc, nil)
	if err := svc.DenySecretRequest(testPrincipalContext(), "project-1", req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProposeApprovalRules(testPrincipalContext(), "project-1", req.ID); !isStatus(err, http.StatusConflict) {
		t.Fatalf("propose for an answered request = %v, want 409", err)
	}
}

// A request is marked for testing when it is filed in a project that has a
// rule, and what is pending is marked again when a rule is made or edited.
// Nothing scans: a run whose judge could not answer is retried by the engine's
// own backoff, which a periodic re-mark would cancel.
func TestRequestsAreMarkedForTestingWhenFiledAndWhenARuleChanges(t *testing.T) {
	svc, _, _, _ := ruledService(t)
	dirty := &marks{}
	svc.SetDirtier(dirty)
	before := githubAsk(t, svc, nil)
	if len(dirty.marked) != 0 {
		t.Fatalf("marked = %v, want nothing in a project with no rules", dirty.marked)
	}
	rule := githubRule(t, svc, nil)
	want := []string{resourcejudges.JudgeResourceType + "/project-1", resourcesecrets.RuleRequestResourceType + "/" + before.ID}
	if !slices.Equal(dirty.marked, want) {
		t.Fatalf("marked = %v, want the judge and the pending request %v", dirty.marked, want)
	}
	dirty.marked = nil
	after := githubAsk(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
		b.Uses = []apimodel.SecretUse{{Description: "read issue 44"}}
	})
	if !slices.Equal(dirty.marked, []string{resourcesecrets.RuleRequestResourceType + "/" + after.ID}) {
		t.Fatalf("marked = %v, want the request just filed", dirty.marked)
	}
	dirty.marked = nil
	if _, err := svc.UpdateApprovalRule(testPrincipalContext(), "project-1", rule.ID, services.UpdateApprovalRuleBody{
		Policy: serverapi.NewOptAccessPolicy(rulePolicy(t, issueRule)),
	}); err != nil {
		t.Fatal(err)
	}
	want = []string{resourcejudges.JudgeResourceType + "/project-1", resourcesecrets.RuleRequestResourceType + "/" + before.ID, resourcesecrets.RuleRequestResourceType + "/" + after.ID}
	// In any order: two requests filed in one tick of the clock have none.
	slices.Sort(want)
	slices.Sort(dirty.marked)
	if !slices.Equal(dirty.marked, want) {
		t.Fatalf("marked = %v, want an edited rule tested afresh against what is pending: %v", dirty.marked, want)
	}
	if _, ok := any(svc.RuleRequests()).(interface {
		ScanDirty(context.Context) ([]string, error)
	}); ok {
		t.Fatal("the rule reconciler scans, which re-marks a failing request every scan and cancels its backoff")
	}
}

// A grant made beside a new rule is fitted to that rule's cap when nobody
// named its lifetime, as every later grant under the rule is.
func TestAGrantMadeWithANewRuleIsFittedToItsCap(t *testing.T) {
	svc, st, _, _ := ruledService(t)
	req := githubAsk(t, svc, nil)
	approved, err := svc.ApproveSecretRequest(testPrincipalContext(), "project-1", req.ID, services.ApproveSecretRequestBody{
		Policy: askedPolicy(t),
		Rule:   serverapi.NewOptApproveWithRule(serverapi.ApproveWithRule{MaxGrantTTLSeconds: serverapi.NewOptInt64(600)}),
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	grant, err := st.GetSecretGrant(context.Background(), "project-1", approved.GrantID)
	if err != nil || grant.ExpiresAt == nil || time.Until(*grant.ExpiresAt) > 10*time.Minute+time.Second {
		t.Fatalf("grant = %+v, %v, want it to lapse within the rule's ten minutes", grant, err)
	}
}

// A rule for a well-known credential answers later requests with the secret
// marked for it. Made by approving with another secret, it would hand later
// discoboxes a credential nobody chose for them, so that is refused.
func TestAutoApproveWithAnotherSecretThanTheMarkedOneIsRefused(t *testing.T) {
	svc, st, _, _ := ruledService(t)
	other := createBoundSecret(testPrincipalContext(), t, svc, "github-admin", "github.com", 3600)
	req := githubAsk(t, svc, nil)
	_, err := svc.ApproveSecretRequest(testPrincipalContext(), "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(other.ID),
		Policy:   askedPolicy(t),
		Rule:     serverapi.NewOptApproveWithRule(serverapi.ApproveWithRule{}),
	})
	if !isStatus(err, http.StatusBadRequest) || !strings.Contains(err.Error(), "github-admin") {
		t.Fatalf("approve = %v, want a 400 naming the secret that is not the ID's", err)
	}
	if rules, _ := st.ListApprovalRules(context.Background(), "project-1"); len(rules) != 0 || requestNow(t, st, req).Status != model.SecretRequestStatusPending {
		t.Fatal("a rule was made, or the request approved, with a secret the rule would not answer with")
	}
}

// A rule that has lapsed can still be switched off, or given more time: its
// old expiry is not held against an edit that does not name one.
func TestALapsedRuleCanStillBeEdited(t *testing.T) {
	svc, st, _, _ := ruledService(t)
	rule := githubRule(t, svc, nil)
	lapsed := time.Now().Add(-time.Hour).UTC()
	rule.ExpiresAt = &lapsed
	if err := st.UpdateApprovalRule(context.Background(), rule); err != nil {
		t.Fatal(err)
	}
	ctx := testPrincipalContext()
	if off, err := svc.UpdateApprovalRule(ctx, "project-1", rule.ID, services.UpdateApprovalRuleBody{Enabled: serverapi.NewOptBool(false)}); err != nil || off.Enabled {
		t.Fatalf("disable a lapsed rule = %+v, %v", off, err)
	}
	if _, err := svc.UpdateApprovalRule(ctx, "project-1", rule.ID, services.UpdateApprovalRuleBody{ExpiresAt: serverapi.NewOptDateTime(time.Now().Add(-time.Minute))}); err == nil {
		t.Fatal("an edit named an expiry already past")
	}
	if again, err := svc.UpdateApprovalRule(ctx, "project-1", rule.ID, services.UpdateApprovalRuleBody{NeverExpires: serverapi.NewOptBool(true)}); err != nil || again.ExpiresAt != nil {
		t.Fatalf("clear a lapsed rule's expiry = %+v, %v", again, err)
	}
}

// A rule that could never approve again goes with the secret it named. Left
// behind it would still count as a reason for the project to keep a judge. A
// rule names repositories and never a discobox, so it outlives every discobox
// it answered.
func TestARuleGoesWithTheSecretItNames(t *testing.T) {
	svc, st := newAgentCredentialService(t)
	ctx := testPrincipalContext()
	secret := createBoundSecret(ctx, t, svc, "github", "github.com", 3600)
	createSandbox(t, st, "sbx-gone", testPoolID)
	ofRepository(t, st, "sbx-gone", remote(repoURL))
	named, err := svc.CreateApprovalRule(ctx, "project-1", services.CreateApprovalRuleBody{
		SecretId: serverapi.NewOptString(secret.ID), Policy: rulePolicy(t, issueRule), Repositories: []string{repo},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSandbox(context.Background(), "project-1", "sbx-gone"); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	if _, err := st.GetApprovalRule(context.Background(), "project-1", named.ID); err != nil {
		t.Fatalf("the rule after a discobox of its repository was deleted = %v, want it kept for the next one", err)
	}
	if err := svc.DeleteSecret(ctx, "project-1", secret.ID); err != nil {
		t.Fatalf("delete secret: %v", err)
	}
	if _, err := st.GetApprovalRule(context.Background(), "project-1", named.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the rule naming a deleted secret = %v, want it gone", err)
	}
	if ruled, err := st.HasEnabledApprovalRule(context.Background(), "project-1"); err != nil || ruled {
		t.Fatalf("HasEnabledApprovalRule() = %v, %v, want no rule left to keep a judge for", ruled, err)
	}
}

// A written policy that covers the request but drops a deny the request asked
// for is shown as not covering: as a rule, it would approve later asks without
// that deny.
func TestAProposalThatDropsADenyDoesNotCover(t *testing.T) {
	svc, _, judging, _ := ruledService(t)
	judging.proposal = judge.Proposal{StatementsAnyTarget: []string{issueRule, prRule}}
	req := githubAsk(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
		b.Policy = shownAs(t, workerPolicy(t, issueAsk, prAsk, "deny delete on anything in anywhere"))
	})
	proposals, err := svc.ProposeApprovalRules(testPrincipalContext(), "project-1", req.ID)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if written, _ := proposals.Policy.Get(); written.Covers || !strings.Contains(written.Reason.Or(""), "drops a deny") {
		t.Fatalf("policy = %+v, want it shown as dropping the deny asked for", written)
	}
}

// A proposal is held to what a rule may be before it is compared: a
// statement that does not parse, and a word the model brought in that the
// request never defined, are shown as refused, with why and with what was
// written, and nobody is asked about them.
func TestAProposalThatCouldNotBeARuleIsNotCompared(t *testing.T) {
	for name, tc := range map[string]struct {
		written []string
		says    string
	}{
		"a statement that does not parse": {written: []string{"read any issue <number> in org/repo"}, says: "statement 1"},
		"a word nobody defined":           {written: []string{issueRule, `allow push on branch "discobox/*" in repository $primary.repo`}, says: "definition"},
	} {
		t.Run(name, func(t *testing.T) {
			svc, _, judging, _ := ruledService(t)
			judging.proposal = judge.Proposal{StatementsAnyTarget: tc.written}
			req := githubAsk(t, svc, nil)
			proposals, err := svc.ProposeApprovalRules(testPrincipalContext(), "project-1", req.ID)
			if err != nil {
				t.Fatalf("propose: %v", err)
			}
			written, _ := proposals.Policy.Get()
			if written.Covers || written.Policy.IsSet() || !strings.Contains(written.Refused.Or(""), tc.says) || written.Reason.IsSet() ||
				!slices.Equal(written.Statements, tc.written) {
				t.Fatalf("policy = %+v, want it refused saying %q, with what was written and no reason", written, tc.says)
			}
			if len(judging.contents) != 0 {
				t.Fatalf("the judge was asked about %d contents, want none", len(judging.contents))
			}
		})
	}
}

// The card a person reads says, for each rule tested against a pending
// request, why it did not cover it: a fact the server checked, the policy as
// code compared it, or what the judge said of conditions, read from the
// verdict on record. A rule whose judge has still to answer is pending.
func TestTheCardSaysWhyEachRuleDidNotCover(t *testing.T) {
	svc, st, _, _ := ruledService(t)
	ctx := context.Background()
	elsewhere := githubRule(t, svc, func(b *services.CreateApprovalRuleBody) { b.Hosts = []string{"api.github.com"} })
	foreign := githubRule(t, svc, func(b *services.CreateApprovalRuleBody) { b.Repositories = []string{"github.com/org/elsewhere"} })
	unread := githubRule(t, svc, nil)
	narrower := githubRule(t, svc, func(b *services.CreateApprovalRuleBody) { b.Policy = rulePolicy(t, issueRule) })
	req := githubAsk(t, svc, nil)

	tests := func() map[string]model.RuleTest {
		t.Helper()
		seen, err := svc.GetSecretRequest(testPrincipalContext(), "project-1", req.ID)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		out := map[string]model.RuleTest{}
		for _, test := range seen.RuleTests {
			out[test.RuleID] = test
		}
		return out
	}
	first := tests()
	if got := first[elsewhere.ID]; got.Covered || got.Stage != model.RuleTestStageFacts || !strings.Contains(got.Reason, "api.github.com") {
		t.Fatalf("test = %+v, want the hosts named as the fact that failed", got)
	}
	if got := first[foreign.ID]; got.Covered || got.Pending || got.Stage != model.RuleTestStageFacts ||
		!strings.Contains(got.Reason, "github.com/org/elsewhere") || !strings.Contains(got.Reason, repo) {
		t.Fatalf("test = %+v, want both repositories named as the fact that failed, with no judge to wait on", got)
	}
	if got := first[unread.ID]; got.Covered || !got.Pending || got.Stage != model.RuleTestStageContent {
		t.Fatalf("test = %+v, want it pending on its content", got)
	}
	if got := first[narrower.ID]; got.Covered || got.Pending || got.Stage != model.RuleTestStageContent || !strings.Contains(got.Reason, "pull request") {
		t.Fatalf("test = %+v, want the allow code found outside the rule named, with no judge to wait on", got)
	}

	// The judge says no to the conditions, and the card carries its sentence.
	// The question names the discobox asking, as it was created (ADR
	// 26-10-14-691), so the digest on record is of the job with its facts.
	asking, err := st.GetSandbox(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := judge.Digest(judge.Job{
		Kind: judge.KindRule, Purposes: []string{bound(prRule)}, Uses: []string{bound(prAsk)},
		Credential: wellknown.GitHubAPI, Host: "github.com", Round: 1, Facts: judge.Facts(judge.DiscoboxOf(asking.PolicyFacts())),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCredentialVerdict(ctx, &model.CredentialVerdict{
		ProjectID: "project-1", Kind: model.CredentialVerdictKindRule, Origin: model.CredentialVerdictOriginJudge,
		SandboxID: testSandboxID, SecretRequestID: req.ID, RuleID: unread.ID, QuestionDigest: digest, Reason: "Refused: Jev could not tell (0.41).",
	}); err != nil {
		t.Fatal(err)
	}
	if got := tests()[unread.ID]; got.Covered || got.Pending || got.Stage != model.RuleTestStageContent || !strings.Contains(got.Reason, "0.41") {
		t.Fatalf("test = %+v, want the content's verdict and its score", got)
	}
}

// withoutPolicy is an ask with uses alone, which no rule answers.
func withoutPolicy(b *services.CreateSandboxCredentialRequestBody) {
	b.Policy = serverapi.OptAccessPolicy{}
}
