package secrets_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/policy"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// delegatePolicy gives the lead a delegation of secret to host carrying the
// policy the sentences say, resolved for the lead, as a person approving its
// ask to delegate with that policy does.
func delegatePolicy(t *testing.T, st *store.Store, secret *model.Secret, host string, sentences ...string) *model.SecretGrant {
	t.Helper()
	return delegatePolicyFor(t, st, secret, host, time.Hour, sentences...)
}

// delegatePolicyFor is delegatePolicy, lapsing after lifetime.
func delegatePolicyFor(t *testing.T, st *store.Store, secret *model.Secret, host string, lifetime time.Duration, sentences ...string) *model.SecretGrant {
	t.Helper()
	p := workerPolicy(t, sentences...)
	resolved := p.Resolve(policy.Facts{Discobox: policy.Discobox{ID: leadID}})
	grant := delegate(t, st, secret, host, lifetime)
	grant.Policy = &resolved
	if err := st.UpdateSecretGrant(context.Background(), grant); err != nil {
		t.Fatalf("give the delegation its policy: %v", err)
	}
	return grant
}

// workerPolicy is a policy the worker asks for, from sentences.
func workerPolicy(t *testing.T, sentences ...string) policy.Policy {
	t.Helper()
	statements, err := policy.ParseStatements(sentences)
	if err != nil {
		t.Fatal(err)
	}
	return policy.Policy{Statements: statements, Definitions: definitionsFor(statements)}
}

// definitionsFor defines each word the statements use that needs one, and no
// other: a policy may not define a word it does not use.
func definitionsFor(statements []policy.Statement) map[string]string {
	glossary := map[string]string{
		"pull request": "A GitHub pull request.", "repository": handedOnPolicy().Definitions["repository"], "issue": handedOnPolicy().Definitions["issue"],
		"branch": "A git branch.", "workflow run": "A GitHub Actions run.", "review": "A review of a pull request.",
		"push": "Add commits to a branch.", "rerun": "Re-run a run's failed jobs.",
	}
	out := map[string]string{}
	for _, s := range statements {
		for _, word := range append([]string{s.ResourceType, s.Scope.Type}, s.Actions...) {
			if definition, ok := glossary[word]; ok {
				out[word] = definition
			}
		}
	}
	return out
}

// A lead approves a worker's policy within the policy it was delegated (ADR
// 26-10-20-782 §11): the grant carries the worker's policy, resolved for the
// worker. One wider, one that drops the delegation's deny, and one under a
// delegation with no policy are refused, saying why.
func TestALeadHandsOnAPolicyWithinItsOwn(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	svc.SetJudge(&delegationJudge{allow: true})
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegatePolicy(t, st, secret, "github.com",
		`allow read, update on pull request in repository "acme/web"`,
		`deny delete on anything in anywhere`)

	ask := func(p policy.Policy) *model.SecretRequest {
		return workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
			b.Policy = apiPolicy(t, p)
			b.Uses = []apimodel.SecretUse{{Description: "update pull request " + p.Statements[0].Actions[0]}}
		})
	}

	within := workerPolicy(t, `allow update on pull request in repository $primary.repo`, `deny delete on anything in anywhere`)
	req := ask(within)
	approved, err := approveAsLead(svc, req, services.ApproveSecretRequestBody{Policy: apiPolicy(t, within)})
	if err != nil {
		t.Fatalf("approve a policy within the delegation's: %v", err)
	}
	grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Policy == nil || grant.Policy.Resolved[policy.VarPrimaryRepo] != "acme/web" {
		t.Fatalf("grant policy = %+v, want the worker's, resolved for the worker", grant.Policy)
	}

	for _, tt := range []struct {
		name, says string
		p          policy.Policy
	}{
		{"an action not delegated", "not within any allow", workerPolicy(t, `allow create on pull request in repository $primary.repo`, `deny delete on anything in anywhere`)},
		{"the delegation's deny dropped", "it is not carried", workerPolicy(t, `allow read on pull request in repository $primary.repo`)},
		{"another repository", "not within any allow", workerPolicy(t, `allow read on pull request in repository "acme/other"`, `deny delete on anything in anywhere`)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := approveAsLead(svc, ask(tt.p), services.ApproveSecretRequestBody{Policy: apiPolicy(t, tt.p)})
			wantStatus(t, err, http.StatusForbidden, tt.says)
		})
	}
}

// A hand-off is bound by its policy alone (ADR 26-10-20-782 §11, #312): a
// policy within the delegation's is approved with no judge to ask, whatever
// the uses beside it say, and carries those uses as asked. A delegation with
// no policy hands nothing on.
func TestAHandOffIsBoundByItsPolicyNotItsUses(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	svc.SetJudge(nil)
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	bare := delegate(t, st, secret, "github.com", time.Hour)
	bare.Policy = nil
	if err := st.UpdateSecretGrant(context.Background(), bare); err != nil {
		t.Fatal(err)
	}
	asked := workerPolicy(t, `allow read on issue in repository "acme/web"`)
	_, err := approveAsLead(svc, workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) { b.Policy = apiPolicy(t, asked) }),
		services.ApproveSecretRequestBody{Policy: apiPolicy(t, asked)})
	wantStatus(t, err, http.StatusForbidden, "carries no policy to hand on")

	held := delegatePolicy(t, st, secret, "github.com", `allow read on issue in repository "acme/web"`)
	uses := []apimodel.SecretUse{{Description: "push main and delete every branch"}}
	req := workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
		b.Policy = apiPolicy(t, asked)
		b.Uses = uses
	})
	approved, err := approveAsLead(svc, req, services.ApproveSecretRequestBody{Policy: apiPolicy(t, asked)})
	if err != nil {
		t.Fatalf("approve a policy within the delegation's with no judge: %v", err)
	}
	grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(useDescriptions(grant.Uses), []string{uses[0].Description}) || grant.Policy == nil ||
		!slices.Equal(policy.FormatStatements(grant.Policy.Statements), policy.FormatStatements(asked.Statements)) {
		t.Fatalf("grant = %+v, want the uses and the policy as asked", grant)
	}
	// Code settled it and no judge was asked, so no verdict names the
	// delegation: the grant does.
	if grant.DelegationID != held.ID {
		t.Fatalf("grant delegation = %q, want the one it was handed on under, %s", grant.DelegationID, held.ID)
	}
}

// Conditions are the judge's: an allow that changes the delegated one's is
// handed on only when the judge finds them within, and the judge reads both
// statements as they resolved.
func TestALeadsConditionsAreTheJudges(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegatePolicy(t, st, secret, "github.com", `allow update on pull request in repository "acme/web" when "its base is main"`)
	asked := workerPolicy(t, `allow update on pull request in repository $primary.repo when "its base is main and it is ready"`)

	for _, tt := range []struct {
		name   string
		within bool
	}{{"within", true}, {"not within", false}} {
		t.Run(tt.name, func(t *testing.T) {
			judge := &delegationJudge{within: func(ask services.DelegationAsk) bool {
				return !strings.Contains(ask.Uses[0], "ready") || tt.within
			}}
			svc.SetJudge(judge)
			req := workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
				b.Policy = apiPolicy(t, asked)
				b.Uses = []apimodel.SecretUse{{Description: "update a pull request, " + tt.name}}
			})
			_, err := approveAsLead(svc, req, services.ApproveSecretRequestBody{Policy: apiPolicy(t, asked)})
			if tt.within && err != nil {
				t.Fatalf("approve: %v", err)
			}
			if !tt.within {
				wantStatus(t, err, http.StatusForbidden, "the judge did not find its conditions within")
			}
			first := judge.asks()[0]
			if !slices.Equal(first.Delegated, []string{`allow update on pull request in repository "acme/web" when "its base is main"`}) ||
				!slices.Equal(first.Uses, []string{`allow update on pull request in repository "acme/web" when "its base is main and it is ready"`}) {
				t.Fatalf("judge asked %+v, want the two statements as they resolved", first)
			}
		})
	}
}

// A lead gives the discobox it creates a policy within its own, and no other.
func TestALeadGivesAPolicyWithinItsOwnOnACreate(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	svc.SetJudge(&delegationJudge{allow: true})
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegatePolicy(t, st, secret, "github.com", `allow read on issue in repository "acme/web"`)
	remote := "https://github.com/acme/web.git"
	created := &model.Sandbox{ID: "sbx-new", ProjectID: "project-1", Source: &model.GitSource{Kind: "git", URL: &remote}}
	give := func(p policy.Policy) error {
		_, err := svc.PrepareSandboxGrants(asLead(), "project-1", created, []apimodel.SandboxGrant{{
			SecretId: serverapi.NewOptString(secret.ID), EnvVar: serverapi.NewOptString("GH_TOKEN"), Hosts: []string{"api.github.com"},
			Uses: []apimodel.SecretUse{{Description: "read issue 43"}}, Policy: apiPolicy(t, p),
		}})
		return err
	}
	grants, err := svc.PrepareSandboxGrants(asLead(), "project-1", created, []apimodel.SandboxGrant{{
		SecretId: serverapi.NewOptString(secret.ID), EnvVar: serverapi.NewOptString("GH_TOKEN"), Hosts: []string{"api.github.com"},
		Uses: []apimodel.SecretUse{{Description: "read issue 43"}}, Policy: apiPolicy(t, workerPolicy(t, `allow read on issue in repository $primary.repo`)),
	}})
	if err != nil {
		t.Fatalf("give a policy within the delegation's: %v", err)
	}
	if got := grants.Grants[0].Policy; got == nil || got.Resolved[policy.VarPrimaryRepo] != "acme/web" {
		t.Fatalf("given policy = %+v, want it resolved for the discobox created", got)
	}
	err = give(workerPolicy(t, `allow update on issue in repository $primary.repo`))
	wantStatus(t, err, http.StatusForbidden, "not within any allow")
}
