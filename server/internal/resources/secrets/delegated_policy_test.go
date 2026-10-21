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
	"github.com/discobox-ai/discobox/policy"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// delegatePolicy gives the lead a delegation of secret to host carrying the
// policy the sentences say, resolved for the lead, as a person approving its
// ask to delegate with that policy does.
func delegatePolicy(t *testing.T, st *store.Store, secret *model.Secret, host string, sentences ...string) *model.SecretGrant {
	t.Helper()
	statements, err := policy.ParseStatements(sentences)
	if err != nil {
		t.Fatal(err)
	}
	p := policy.Policy{Statements: statements, Definitions: definitionsFor(statements)}
	resolved := p.Resolve(policy.Facts{Discobox: policy.Discobox{ID: leadID}})
	grant := delegate(t, st, secret, host, time.Hour)
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
	glossary := map[string]string{"pull request": "a GitHub pull request", "repository": "a GitHub repository", "issue": "a GitHub issue"}
	out := map[string]string{}
	for _, s := range statements {
		for _, word := range []string{s.ResourceType, s.Scope.Type} {
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

// A delegation with no policy hands on no policy; it still hands on uses.
func TestADelegationWithoutAPolicyHandsOnNone(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	svc.SetJudge(&delegationJudge{allow: true})
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegate(t, st, secret, "github.com", time.Hour)

	asked := workerPolicy(t, `allow read on issue in repository "acme/web"`)
	req := workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) { b.Policy = apiPolicy(t, asked) })
	_, err := approveAsLead(svc, req, services.ApproveSecretRequestBody{Policy: apiPolicy(t, asked)})
	wantStatus(t, err, http.StatusForbidden, "carries no policy to hand on")

	if _, err := approveAsLead(svc, workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
		b.Uses = []apimodel.SecretUse{{Description: "read issue 44"}}
	}), services.ApproveSecretRequestBody{}); err != nil {
		t.Fatalf("uses under a delegation without a policy: %v", err)
	}
}

// Under a delegation with a policy, a worker asking with uses alone is judged
// against the delegation's uses, as before policies.
func TestUsesAloneAreStillJudgedAsUsesUnderAPolicyDelegation(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	judge := &delegationJudge{allow: true}
	svc.SetJudge(judge)
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegatePolicy(t, st, secret, "github.com", `allow read on issue in repository "acme/web"`)

	if _, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{}); err != nil {
		t.Fatalf("uses alone under a policy delegation: %v", err)
	}
	asks := judge.asks()
	if len(asks) != 1 || !slices.Equal(asks[0].Uses, []string{"read issue 43"}) {
		t.Fatalf("judge asked %+v, want the uses once", asks)
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
