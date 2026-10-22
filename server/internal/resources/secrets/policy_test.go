package secrets_test

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/policy"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

func strPtr(s string) *string { return &s }

// pullRequestPolicy is the ADR's example statement, in the sandbox's own
// repository, with a deny that applies anywhere.
func pullRequestPolicy() policy.Policy {
	return policy.Policy{
		Statements: []policy.Statement{
			{
				Effect: policy.Allow, Actions: []string{policy.ActionCreate, policy.ActionUpdate}, ResourceType: "pull request",
				CreatedBy: policy.VarThis,
				Scope:     policy.Scope{Type: "repository", Name: policy.VarPrimaryRepo},
				When:      strPtr("its base is main"),
			},
			{
				Effect: policy.Deny, Actions: []string{policy.ActionDelete}, ResourceType: policy.Anything,
				Scope: policy.Scope{Type: policy.Anywhere},
			},
		},
		Definitions: map[string]string{"pull request": "A GitHub pull request.", "repository": "A GitHub repository, owner/name."},
	}
}

func apiPolicy(t *testing.T, p policy.Policy) serverapi.OptAccessPolicy {
	t.Helper()
	out, err := services.APIPolicy(&p)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func wantStatus(t *testing.T, err error, status int, says string) {
	t.Helper()
	var statusErr interface{ StatusCode() int }
	if err == nil || !errors.As(err, &statusErr) || statusErr.StatusCode() != status || !strings.Contains(err.Error(), says) {
		t.Fatalf("err = %v, want %d saying %q", err, status, says)
	}
}

// A policy an agent asks for is stored on its request as asked, and approving
// the request puts it on the grant with each variable resolved from the
// record of the sandbox that asked — which is what the sandbox reads back.
func TestAPolicyRoundTripsFromRequestToGrant(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	asked := pullRequestPolicy()

	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"},
		Uses:   []apimodel.SecretUse{{Description: "open a pull request"}},
		Policy: apiPolicy(t, asked),
	})
	if err != nil {
		t.Fatalf("create credential request: %v", err)
	}
	stored, err := st.GetSecretRequest(ctx, "project-1", req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Policy == nil || !reflect.DeepEqual(*stored.Policy, asked) {
		t.Fatalf("stored request policy = %+v, want it as asked", stored.Policy)
	}

	// The approval names the policy it approves; one that does not is refused,
	// and approves nothing it was not shown.
	_, err = svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)})
	wantStatus(t, err, http.StatusBadRequest, "approve it by sending the policy you approve")
	want := map[string]string{policy.VarPrimaryRepo: "acme/web", policy.VarThis: testSandboxID}
	shown := asked
	shown.Resolved = want
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID), Policy: apiPolicy(t, shown),
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Policy == nil || !reflect.DeepEqual(grant.Policy.Resolved, want) || !reflect.DeepEqual(grant.Policy.Statements, asked.Statements) {
		t.Fatalf("grant policy = %+v, want the statements asked with %v resolved", grant.Policy, want)
	}

	_, polled, err := svc.GetSandboxCredentialRequest(ctx, testPoolID, testSandboxID, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if polled == nil || !reflect.DeepEqual(polled.Policy, grant.Policy) {
		t.Fatalf("polled grant policy = %+v, want the approved one", polled)
	}
	credentials, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if len(credentials) != 1 || !reflect.DeepEqual(credentials[0].Grant.Policy, grant.Policy) {
		t.Fatalf("listed credentials = %+v, want the grant's policy", credentials)
	}

	// What a command or a request under the grant is judged against is its
	// policy as approved, whichever use it names (ADR 26-10-20-782 §1).
	forCommand, err := svc.ApprovedCredentialUse(ctx, testPoolID, testSandboxID, grant.Uses[0].UseID)
	if err != nil {
		t.Fatal(err)
	}
	forRequest, err := svc.ApprovedUse(ctx, testPoolID, testSandboxID, grant.Uses[0].UseID, "api.github.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, use := range []services.ApprovedUse{forCommand, forRequest} {
		if use.Policy == nil || !reflect.DeepEqual(*use.Policy, *grant.Policy) {
			t.Fatalf("approved use policy = %+v, want the grant's", use.Policy)
		}
	}
}

// What a sandbox is shown as its facts is what a policy it writes resolves to
// at approval: both are read from the control plane's record of it, and only
// its own pool may read them.
func TestASandboxIsShownTheValuesItsPolicyResolvesTo(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)

	facts, err := svc.SandboxPolicyFacts(ctx, testPoolID, testSandboxID)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	shown := map[string]string{}
	for _, v := range facts.Variables() {
		if v.Resolved {
			shown[v.Name] = v.Value
		}
	}
	if shown[policy.VarPrimaryRepo] != "acme/web" {
		t.Fatalf("shown %v, want $primary.repo acme/web", shown)
	}

	asked := pullRequestPolicy()
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"},
		Uses:   []apimodel.SecretUse{{Description: "open a pull request"}},
		Policy: apiPolicy(t, asked),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The person approves the request as a read of it shows it, the values
	// its variables resolve to included.
	read, err := svc.GetSecretRequest(ctx, "project-1", req.ID)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID), Policy: apiPolicy(t, *read.Policy),
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	for _, variable := range asked.Variables() {
		if got, want := grant.Policy.Resolved[variable], shown[variable]; got != want {
			t.Errorf("%s resolved to %q at approval; the sandbox was shown %q", variable, got, want)
		}
	}

	if _, err := svc.SandboxPolicyFacts(ctx, "pool-other", testSandboxID); err == nil {
		t.Fatal("another pool read this sandbox's facts")
	}
}

// An ask with the same uses and another policy is another ask: folding it
// into the open one would drop the policy the agent was told it asked for.
func TestAnotherPolicyIsAnotherRequest(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	ask := func(p *policy.Policy) string {
		body := services.CreateSandboxCredentialRequestBody{
			SandboxId: testSandboxID, Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"},
			Uses: []apimodel.SecretUse{{Description: "open a pull request"}},
		}
		if p != nil {
			body.Policy = apiPolicy(t, *p)
		}
		req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, body)
		if err != nil {
			t.Fatal(err)
		}
		return req.ID
	}
	first := pullRequestPolicy()
	second := pullRequestPolicy()
	second.Statements[0].When = strPtr("its base is develop")
	a, again, b, none := ask(&first), ask(&first), ask(&second), ask(nil)
	if a != again {
		t.Fatal("the same ask twice made two requests")
	}
	if a == b || a == none || b == none {
		t.Fatalf("requests %s, %s, %s: asks with different policies were folded together", a, b, none)
	}
}

func TestARequestWithAnInvalidPolicyIsRefused(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	invalid := pullRequestPolicy()
	delete(invalid.Definitions, "pull request")
	_, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"},
		Uses:   []apimodel.SecretUse{{Description: "open a pull request"}},
		Policy: apiPolicy(t, invalid),
	})
	wantStatus(t, err, http.StatusBadRequest, `"pull request" is not a default word and has no definition`)
}

// A client cannot supply what a variable resolved to: the value is the
// server's, read from its own record.
func TestWhatAClientSaysAVariableResolvedToIsIgnored(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	claimed := pullRequestPolicy()
	claimed.Resolved = map[string]string{policy.VarPrimaryRepo: "victim/repo"}

	grant, err := svc.CreateSecretGrant(ctx, "project-1", services.CreateSecretGrantBody{
		SecretId: secret.ID, Scope: serverapi.CreateSecretGrantBodyScopeSandbox, ScopeKey: serverapi.NewOptString(testSandboxID),
		Hosts: []string{"api.github.com"}, EnvVar: serverapi.NewOptString("GITHUB_TOKEN"),
		Uses:   serverapi.NewOptNilSecretUseArray([]apimodel.SecretUse{{Description: "open a pull request"}}),
		Policy: apiPolicy(t, claimed),
	})
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	stored, err := st.GetSecretGrant(ctx, "project-1", grant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Policy.Resolved[policy.VarPrimaryRepo]; got != "acme/web" {
		t.Fatalf("$primary.repo resolved to %q, want the sandbox's own acme/web", got)
	}
}

// The approver may approve a policy other than the one asked for; theirs is
// what the grant carries.
func TestTheApproverMayEditThePolicy(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	asked := pullRequestPolicy()
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"},
		Uses:   []apimodel.SecretUse{{Description: "open a pull request"}},
		Policy: apiPolicy(t, asked),
	})
	if err != nil {
		t.Fatal(err)
	}
	narrowed := pullRequestPolicy()
	narrowed.Statements[0].Actions = []string{policy.ActionCreate}
	narrowed.Resolved = map[string]string{policy.VarPrimaryRepo: "acme/web", policy.VarThis: testSandboxID}
	// The edit names the repository rather than $primary.repo, which was
	// shown and no longer counts.
	narrowed.Statements[0].Scope.Name = "acme/web"
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID), Policy: apiPolicy(t, narrowed),
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(grant.Policy.Statements, narrowed.Statements) {
		t.Fatalf("grant statements = %+v, want the approver's", grant.Policy.Statements)
	}
}

// A grant wider than one sandbox has no discobox for a variable to resolve
// against, so it may carry a policy only if the policy uses none; and a
// policy rides beside uses.
func TestAGrantsPolicyNeedsASandboxForItsVariablesAndUses(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	body := func(scope serverapi.CreateSecretGrantBodyScope, p policy.Policy, uses bool) services.CreateSecretGrantBody {
		b := services.CreateSecretGrantBody{SecretId: secret.ID, Scope: scope, Hosts: []string{"api.github.com"}, Policy: apiPolicy(t, p)}
		if uses {
			b.EnvVar = serverapi.NewOptString("GITHUB_TOKEN")
			b.Uses = serverapi.NewOptNilSecretUseArray([]apimodel.SecretUse{{Description: "open a pull request"}})
		}
		return b
	}

	_, err := svc.CreateSecretGrant(ctx, "project-1", body(serverapi.CreateSecretGrantBodyScopeProject, pullRequestPolicy(), true))
	wantStatus(t, err, http.StatusBadRequest, "can only be granted to one sandbox")

	literal := pullRequestPolicy()
	literal.Statements[0].CreatedBy = ""
	literal.Statements[0].Scope.Name = "acme/web"
	grant, err := svc.CreateSecretGrant(ctx, "project-1", body(serverapi.CreateSecretGrantBodyScopeProject, literal, true))
	if err != nil {
		t.Fatalf("a project grant whose policy uses no variable: %v", err)
	}
	if grant.Policy == nil || grant.Policy.Resolved != nil {
		t.Fatalf("grant policy = %+v, want it with nothing resolved", grant.Policy)
	}

	_, err = svc.CreateSecretGrant(ctx, "project-1", body(serverapi.CreateSecretGrantBodyScopeProject, literal, false))
	wantStatus(t, err, http.StatusBadRequest, "only beside uses")
}

// A grant given to a discobox as it is created is for that discobox: $this is
// the one created, and $primary.repo its repository, not the creator's.
func TestAPolicyGivenOnCreateBindsToTheNewDiscobox(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	remote, creator := "git@github.com:acme/fork.git", testSandboxID
	created := &model.Sandbox{ID: "sbx-new", ProjectID: "project-1", CreatedBySandboxID: &creator, Source: &model.GitSource{Kind: "git", URL: &remote}}

	grants, err := svc.PrepareSandboxGrants(ctx, "project-1", created, []apimodel.SandboxGrant{{
		SecretId: serverapi.NewOptString(secret.ID), EnvVar: serverapi.NewOptString("GITHUB_TOKEN"), Hosts: []string{"api.github.com"},
		Uses:   []apimodel.SecretUse{{Description: "open a pull request"}},
		Policy: apiPolicy(t, pullRequestPolicy()),
	}})
	if err != nil {
		t.Fatalf("prepare grants: %v", err)
	}
	got := grants.Grants[0].Policy
	want := map[string]string{policy.VarThis: "sbx-new", policy.VarPrimaryRepo: "acme/fork"}
	if got == nil || !reflect.DeepEqual(got.Resolved, want) {
		t.Fatalf("grant policy = %+v, want %v resolved", got, want)
	}
}

// A request read for the person approving it carries what each variable of
// its policy resolves to for the discobox that asked — the values the form
// shows, from the same record the grant is resolved from — while the request
// keeps its policy as asked.
func TestAReadRequestShowsWhatItsVariablesResolveTo(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	asked := pullRequestPolicy()
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"},
		Uses:   []apimodel.SecretUse{{Description: "open a pull request"}},
		Policy: apiPolicy(t, asked),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{policy.VarPrimaryRepo: "acme/web", policy.VarThis: testSandboxID}

	got, err := svc.GetSecretRequest(ctx, "project-1", req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Policy == nil || !reflect.DeepEqual(got.Policy.Resolved, want) || !reflect.DeepEqual(got.Policy.Statements, asked.Statements) {
		t.Fatalf("read request policy = %+v, want the statements asked with %v resolved", got.Policy, want)
	}
	listed, err := svc.ListSecretRequests(ctx, "project-1", model.SecretRequestStatusPending)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Policy == nil || !reflect.DeepEqual(listed[0].Policy.Resolved, want) {
		t.Fatalf("listed requests = %+v, want one with %v resolved", listed, want)
	}
	stored, err := st.GetSecretRequest(ctx, "project-1", req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Policy == nil || stored.Policy.Resolved != nil {
		t.Fatalf("stored request policy = %+v, want it unresolved, as asked", stored.Policy)
	}
}

// A request that asks for uses alone is a person's to approve each time: no
// rule is proposed or made from it, since a rule's policy is what a
// request's is held to, and uses alone have none.
func TestNoRuleIsMadeFromARequestWithUsesAlone(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"},
		Uses: []apimodel.SecretUse{{Description: "open a pull request"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.ProposeApprovalRules(ctx, "project-1", req.ID)
	wantStatus(t, err, http.StatusBadRequest, "asks for uses alone")
	_, err = svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID),
		Rule:     serverapi.NewOptApproveWithRule(serverapi.ApproveWithRule{Policy: apiPolicy(t, pullRequestPolicy())}),
	})
	wantStatus(t, err, http.StatusBadRequest, "asks for uses alone")
	if rules, err := st.ListApprovalRules(ctx, "project-1"); err != nil || len(rules) != 0 {
		t.Fatalf("rules = %v, %v; want none made", rules, err)
	}
}

// A person approves the values a policy's variables were shown to resolve
// to: an approval shown other values, or shown a variable as matching
// nothing that resolves now, grants nothing.
func TestAnApprovalShownOtherValuesIsRefused(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	asked := pullRequestPolicy()
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"},
		Uses:   []apimodel.SecretUse{{Description: "open a pull request"}},
		Policy: apiPolicy(t, asked),
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, shown := range map[string]map[string]string{
		"another value":  {policy.VarPrimaryRepo: "victim/repo", policy.VarThis: testSandboxID},
		"matching none":  {policy.VarThis: testSandboxID},
		"nothing at all": nil,
	} {
		sent := asked
		sent.Resolved = shown
		_, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
			SecretId: serverapi.NewOptString(secret.ID), Policy: apiPolicy(t, sent),
		})
		if err == nil {
			t.Fatalf("%s: an approval shown %v was granted", name, shown)
		}
		wantStatus(t, err, http.StatusConflict, "read the request again")
	}
	if pending, err := st.GetSecretRequest(ctx, "project-1", req.ID); err != nil || pending.Status != model.SecretRequestStatusPending || pending.GrantID != "" {
		t.Fatalf("request = %+v, %v; want it still pending with no grant", pending, err)
	}

	// What a read of the request shows is what approving it grants.
	read, err := svc.GetSecretRequest(ctx, "project-1", req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID), Policy: apiPolicy(t, *read.Policy),
	}); err != nil {
		t.Fatalf("approving what the read showed: %v", err)
	}
}

// A rule holds a request's policy to its own, so only a request that carries
// a policy is put to one, and one with uses alone never is (ADR 26-10-22-689).
func TestARuleTestsOnlyARequestWithAPolicy(t *testing.T) {
	svc, _ := newAgentCredentialService(t)
	if req := workerRequest(t, svc, nil); !resourcesecrets.RuleTestable(req) {
		t.Fatal("a request carrying a policy is never put to a rule")
	}
	if req := workerRequest(t, svc, withoutPolicy); resourcesecrets.RuleTestable(req) {
		t.Fatal("a request with uses alone may be approved by a rule")
	}
}

// A policy that fits a judge job as written may not once its variables are
// bound: a repository's name is longer than $primary.repo. The grant is held
// to what a judge would be shown, so one every job under it would refuse is
// never minted.
func TestAGrantWhosePolicyBoundIsTooLargeToJudgeIsRefused(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	repo := "acme/" + strings.Repeat("a-repository-name-", 5)
	sandbox, err := st.GetSandbox(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	url := "https://github.com/" + repo
	sandbox.Source.URL = &url
	if err := st.UpdateSandbox(ctx, sandbox); err != nil {
		t.Fatal(err)
	}

	// Ten statements naming $primary.repo, their conditions as long as lets
	// the policy fit as written and not once bound.
	statements := func(condition string) policy.Policy {
		p := policy.Policy{Definitions: map[string]string{"issue": "A GitHub issue.", "repository": "A GitHub repository."}}
		for i := range 10 {
			when := fmt.Sprintf("%s %d", condition, i)
			p.Statements = append(p.Statements, policy.Statement{
				Effect: policy.Allow, Actions: []string{policy.ActionRead}, ResourceType: "issue",
				Scope: policy.Scope{Type: "repository", Name: policy.VarPrimaryRepo}, When: &when,
			})
		}
		return p
	}
	var asked policy.Policy
	for n := 500; n < policy.MaxTextBytes; n++ {
		p := statements(strings.Repeat("x", n))
		facts := sandbox.PolicyFacts()
		if judge.PolicyFits(p) == nil && judge.PolicyFits(p.Resolve(facts).Bound(facts)) != nil {
			asked = p
			break
		}
	}
	if asked.Statements == nil {
		t.Fatal("no policy fits as written and not once bound")
	}

	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"},
		Uses:   []apimodel.SecretUse{{Description: "read issues"}},
		Policy: apiPolicy(t, asked),
	})
	if err != nil {
		t.Fatalf("a policy that fits as written was refused as asked: %v", err)
	}
	shown := asked
	shown.Resolved = map[string]string{policy.VarPrimaryRepo: repo}
	_, err = svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID), Policy: apiPolicy(t, shown),
	})
	wantStatus(t, err, http.StatusBadRequest, "more than the 8192 a judge may be shown")
}
