package service_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/judge"
	"github.com/discobox-ai/discobox/pkg/policy"
	"github.com/discobox-ai/discobox/pkg/wellknown"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/resources/secrets"
	"github.com/discobox-ai/discobox/server/internal/service"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// githubGrant is a grant of the secret for a new discobox, pushing a branch.
func githubGrant(secretID string) serverapi.SandboxGrant {
	return serverapi.SandboxGrant{
		SecretId: serverapi.NewOptString(secretID),
		EnvVar:   serverapi.NewOptString("GH_TOKEN"),
		Hosts:    []string{"github.com"},
		Uses:     []serverapi.SecretUse{{Description: "push a branch to org/repo"}},
		Policy:   pushPolicy(givenPush),
	}
}

// delegatedPush is what the lead may hand on, and givenPush what it gives: the
// same allow under another condition, so the judge reads the two conditions
// (ADR 26-10-20-782 §11).
const (
	delegatedPush = `allow push on branch "*" in repository "org/repo" when "it is a discobox branch"`
	givenPush     = `allow push on branch "*" in repository "org/repo" when "it is the discobox branch of its issue"`
)

// pushPolicy is a policy of the sentences, with the words they use defined.
func pushPolicy(sentences ...string) serverapi.OptAccessPolicy {
	statements, err := policy.ParseStatements(sentences)
	if err != nil {
		panic(err)
	}
	out, err := services.APIPolicy(&policy.Policy{Statements: statements, Definitions: map[string]string{
		"push": "Add commits to a branch.", "branch": "A git branch.", "repository": "A GitHub repository, owner/name.",
	}})
	if err != nil {
		panic(err)
	}
	return out
}

func createGitHubSecret(ctx context.Context, t *testing.T, svc interface {
	CreateSecret(context.Context, string, services.CreateSecretBody) (*model.Secret, error)
}, projectID string) *model.Secret {
	t.Helper()
	secret, err := svc.CreateSecret(ctx, projectID, services.CreateSecretBody{
		Name:  "github",
		Type:  serverapi.CreateSecretBodyTypeToken,
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("ghp_realrealrealrealrealrealrealreal12")},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	return secret
}

// A discobox can be created already holding uses of project secrets: grants
// for it, and the bindings its agent takes them through, stored with it.
func TestADiscoboxIsCreatedWithTheUsesItIsGiven(t *testing.T) {
	ctx := context.Background()
	svc, _, _, projectID := newSandboxTestService(t, nil)
	secret := createGitHubSecret(ctx, t, svc, projectID)

	created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.SandboxCreateConfig{Name: "worker"},
		Grants:      []serverapi.SandboxGrant{githubGrant(secret.ID)},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	credentials, err := svc.ListSandboxCredentials(ctx, created.PoolID, created.ID)
	if err != nil {
		t.Fatalf("list credentials: %v", err)
	}
	if len(credentials) != 1 {
		t.Fatalf("credentials = %#v, want the use it was given", credentials)
	}
	got := credentials[0]
	if got.Assignment.EnvName != "GH_TOKEN" || !slices.Equal(got.Grant.Hosts, []string{"github.com"}) || len(got.Grant.Uses) != 1 ||
		got.Grant.Uses[0].Description != "push a branch to org/repo" || got.Grant.Purpose != model.SecretGrantPurposeUse {
		t.Fatalf("credential = %+v, want push to github.com in GH_TOKEN", got)
	}
}

// grantJudge stands in for the project's judge on the delegation a create's
// grants are made under, answering as it is told.
type grantJudge struct {
	allow bool
	asked []services.DelegationAsk
}

func (j *grantJudge) Judge(context.Context, string, services.JudgeAsk) (judge.Answer, error) {
	return judge.Answer{}, errors.New("not a request judge")
}

func (j *grantJudge) JudgeCommand(context.Context, string, services.CommandAsk) (judge.Answer, error) {
	return judge.Answer{}, errors.New("not a command judge")
}

func (j *grantJudge) JudgeDelegation(_ context.Context, _ string, ask services.DelegationAsk) (judge.Answer, error) {
	j.asked = append(j.asked, ask)
	return judge.Answer{Allow: j.allow, Reason: "decided"}, nil
}

func (j *grantJudge) JudgeRule(context.Context, string, services.RuleAsk) (judge.Answer, error) {
	return judge.Answer{}, errors.New("not a rule judge")
}

func (j *grantJudge) ProposeRule(context.Context, string, judge.ProposalJob) (judge.Proposal, error) {
	return judge.Proposal{}, errors.New("not a rule judge")
}

// judgedBy makes judging the service's grants that answer.
func judgedBy(svc *service.Service, judging *grantJudge) {
	svc.SecretService.(*secrets.Service).SetJudge(judging)
}

// ensureLead creates the lead discobox, sbx-lead, the first time it is asked
// for: a grant names a discobox that exists, and a create from the sandbox role
// is bounded by its creator's egress policy, read off its row
// (ADR 26-10-01-032 §4).
func ensureLead(t *testing.T, st *store.Store, projectID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.GetSandbox(ctx, projectID, "sbx-lead"); errors.Is(err, store.ErrNotFound) {
		project, err := st.GetProject(ctx, projectID)
		if err != nil {
			t.Fatalf("get project: %v", err)
		}
		if err := st.CreateSandbox(ctx, &model.Sandbox{
			ID: "sbx-lead", ProjectID: projectID, PoolID: project.DefaultPoolID, CreatedByUserID: "user-lead", Name: "lead",
		}); err != nil {
			t.Fatalf("create lead sandbox: %v", err)
		}
	} else if err != nil {
		t.Fatalf("get lead sandbox: %v", err)
	}
}

// delegateToLead gives the lead a delegation grant of secret to github.com,
// lapsing after lifetime (none when zero). The lead is created the first time:
// a grant names a discobox that exists.
func delegateToLead(t *testing.T, st *store.Store, projectID string, secret *model.Secret, lifetime time.Duration) *model.SecretGrant {
	t.Helper()
	ctx := context.Background()
	ensureLead(t, st, projectID)
	grant := &model.SecretGrant{
		ProjectID: projectID, SecretID: secret.ID, Scope: model.SecretGrantScopeSandbox, ScopeKey: "sbx-lead",
		Hosts: []string{"github.com"}, GrantedBy: "user-1", Purpose: model.SecretGrantPurposeDelegate,
		Uses: []model.SecretUse{{UseID: "use-delegated", Description: "push branches to org/repo, for the discoboxes I create"}},
	}
	delegated, err := services.PolicyFromAPI(pushPolicy(delegatedPush))
	if err != nil {
		t.Fatal(err)
	}
	resolved := delegated.Resolve(policy.Facts{Discobox: policy.Discobox{ID: "sbx-lead"}})
	grant.Policy = &resolved
	if lifetime > 0 {
		expires := time.Now().UTC().Add(lifetime)
		grant.ExpiresAt = &expires
	}
	if err := st.CreateSecretGrant(ctx, grant); err != nil {
		t.Fatalf("create delegation grant: %v", err)
	}
	return grant
}

// What a sandbox creates it creates as the user who created it, and the grants
// it gives are recorded as its own doing. It gives uses, never values, and only
// under a delegation it holds whose policy holds the one it gives — here with
// the conditions the judge reads (ADR 26-09-30-782 §1, ADR 26-10-20-782 §11).
func TestADiscoboxCreatedByASandboxIsItsUsersAndItsGrantsAreTheSandboxs(t *testing.T) {
	ctx := context.Background()
	svc, _, st, projectID := newSandboxTestService(t, nil)
	secret := createGitHubSecret(ctx, t, svc, projectID)
	delegation := delegateToLead(t, st, projectID, secret, time.Hour)
	judging := &grantJudge{allow: true}
	judgedBy(svc, judging)
	lead := auth.WithPrincipal(ctx, auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: projectID, UserID: "user-lead",
	})

	created, err := svc.CreateSandbox(lead, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.SandboxCreateConfig{Name: "worker"},
		Grants:      []serverapi.SandboxGrant{githubGrant(secret.ID)},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if created.CreatedByUserID != "user-lead" {
		t.Fatalf("created by %q, want the lead's user", created.CreatedByUserID)
	}
	if created.CreatedBySandboxID == nil || *created.CreatedBySandboxID != "sbx-lead" {
		t.Fatalf("created by sandbox %v, want the lead (ADR 26-09-24-630 §1)", created.CreatedBySandboxID)
	}
	grants, err := svc.ListSecretGrants(ctx, projectID, secret.ID, "")
	var given []model.SecretGrant
	for _, grant := range grants {
		if grant.Purpose == model.SecretGrantPurposeUse {
			given = append(given, grant)
		}
	}
	if err != nil || len(given) != 1 || given[0].GrantedBy != "sbx-lead" || given[0].ScopeKey != created.ID {
		t.Fatalf("grants = %+v, %v; want one for the worker, granted by the lead", given, err)
	}
	if given[0].ExpiresAt == nil || given[0].ExpiresAt.After(*delegation.ExpiresAt) {
		t.Fatalf("grant expires %v, want no later than the delegation it was made under", given[0].ExpiresAt)
	}
	if len(judging.asked) != 1 || judging.asked[0].DelegationGrantID != delegation.ID ||
		judging.asked[0].For == nil || judging.asked[0].For.ID != created.ID || judging.asked[0].RequestID != "" {
		t.Fatalf("judge asked %+v, want the delegation, for the new discobox, with no request", judging.asked)
	}

	_, err = svc.CreateSandbox(lead, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config: serverapi.SandboxCreateConfig{Name: "leaky", Secrets: []serverapi.SandboxSecretInput{{
			Env: "GH_TOKEN", SecretId: serverapi.NewOptString(secret.ID),
		}}},
	})
	requireStatus(t, err, http.StatusForbidden)
}

// A create whose grants cannot all be given creates nothing: a discobox that
// started without a use it was meant to have would be doing different work.
func TestACreateThatCannotGiveItsGrantsCreatesNothing(t *testing.T) {
	ctx := context.Background()
	svc, _, _, projectID := newSandboxTestService(t, nil)
	secret := createGitHubSecret(ctx, t, svc, projectID)

	noUses := githubGrant(secret.ID)
	noUses.Uses = nil
	noHost := githubGrant(secret.ID)
	noHost.Hosts = nil
	other := createSecretNamed(ctx, t, svc, projectID, "gitlab")
	twoForOneVariable := githubGrant(other.ID)

	for name, tc := range map[string]struct {
		grants []serverapi.SandboxGrant
		want   int
	}{
		"a grant without uses":            {[]serverapi.SandboxGrant{noUses}, http.StatusBadRequest},
		"a grant without a host":          {[]serverapi.SandboxGrant{noHost}, http.StatusBadRequest},
		"a secret that does not exist":    {[]serverapi.SandboxGrant{githubGrant("sec_nobody")}, http.StatusNotFound},
		"two credentials in one variable": {[]serverapi.SandboxGrant{githubGrant(secret.ID), twoForOneVariable}, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
				HarnessName: serverapi.NewOptString("shell"),
				Config:      serverapi.SandboxCreateConfig{Name: "worker"},
				Grants:      tc.grants,
			})
			requireStatus(t, err, tc.want)
			sandboxes, err := svc.ListSandboxes(ctx, projectID, "", nil, nil)
			if err != nil || len(sandboxes) != 0 {
				t.Fatalf("sandboxes = %d, %v; want none created", len(sandboxes), err)
			}
		})
	}
}

func createSecretNamed(ctx context.Context, t *testing.T, svc interface {
	CreateSecret(context.Context, string, services.CreateSecretBody) (*model.Secret, error)
}, projectID, name string) *model.Secret {
	t.Helper()
	secret, err := svc.CreateSecret(ctx, projectID, services.CreateSecretBody{
		Name:  name,
		Type:  serverapi.CreateSecretBodyTypeToken,
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("glpat-realrealrealrealreal12")},
	})
	if err != nil {
		t.Fatalf("create secret %s: %v", name, err)
	}
	return secret
}

func requireStatus(t *testing.T, err error, want int) {
	t.Helper()
	var statusErr apperrors.StatusError
	if !errors.As(err, &statusErr) || statusErr.Status != want {
		t.Fatalf("err = %v, want %d", err, want)
	}
}

// A grant may name a well-known credential by its ID, as an ask may: the ID
// carries the variable and host, and the secret is the one a person marked as
// answering it. Nobody but that person chooses which secret that is.
func TestADiscoboxIsGivenAWellKnownCredentialByID(t *testing.T) {
	ctx := context.Background()
	svc, _, st, projectID := newSandboxTestService(t, nil)
	byID := func(id, host string) serverapi.SandboxGrant {
		grant := serverapi.SandboxGrant{
			WellKnownId: serverapi.NewOptString(id),
			Uses:        []serverapi.SecretUse{{Description: "read issues in org/repo"}},
			Policy:      pushPolicy(givenPush),
		}
		if host != "" {
			grant.Hosts = []string{host}
		}
		return grant
	}
	create := func(name string, grants ...serverapi.SandboxGrant) (*model.Sandbox, error) {
		return svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
			HarnessName: serverapi.NewOptString("shell"),
			Config:      serverapi.SandboxCreateConfig{Name: name},
			Grants:      grants,
		})
	}

	// Nothing answers the ID until a person marks a secret for it.
	_, err := create("too-soon", byID(wellknown.GitHubAPI, ""))
	requireStatus(t, err, http.StatusBadRequest)

	secret := createGitHubSecret(ctx, t, svc, projectID)
	if err := st.MarkSecretWellKnown(ctx, projectID, secret.ID, wellknown.GitHubAPI); err != nil {
		t.Fatalf("mark: %v", err)
	}
	created, err := create("worker", byID(wellknown.GitHubAPI, "api.github.com"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	credentials, err := svc.ListSandboxCredentials(ctx, created.PoolID, created.ID)
	if err != nil || len(credentials) != 1 {
		t.Fatalf("credentials = %#v, %v; want the one it was given", credentials, err)
	}
	if got := credentials[0]; got.Assignment.EnvName != "GH_TOKEN" || got.Assignment.SecretID != secret.ID || !slices.Equal(got.Grant.Hosts, []string{"api.github.com"}) {
		t.Fatalf("credential = %+v, want the marked secret in GH_TOKEN, narrowed to api.github.com", got)
	}

	mixed := byID(wellknown.GitHubAPI, "")
	mixed.EnvVar = serverapi.NewOptString("GITHUB_TOKEN")
	for name, tc := range map[string]struct {
		grant serverapi.SandboxGrant
		want  int
	}{
		"an ID beside a variable of its own":      {mixed, http.StatusBadRequest},
		"a host the ID is not sent to":            {byID(wellknown.GitHubAPI, "gitlab.com"), http.StatusBadRequest},
		"an ID nothing knows":                     {byID("com.example.nothing", ""), http.StatusBadRequest},
		"the discobox API, which a person grants": {byID(wellknown.DiscoboxSandbox, ""), http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := create("refused", tc.grant)
			requireStatus(t, err, tc.want)
		})
	}
}

// The gate is refused however it is named: by its ID, or by its secret's ID,
// which a discobox holding it can read from the secret listing.
func TestTheDiscoboxAPIIsNotGivenAtCreateByItsSecret(t *testing.T) {
	ctx := context.Background()
	svc, _, _, projectID := newSandboxTestService(t, nil)
	workers, err := services.APIPolicy(&policy.Policy{
		Statements:  []policy.Statement{{Effect: policy.Allow, Actions: []string{policy.ActionCreate}, ResourceType: "discobox", Scope: policy.Scope{Type: "project", Name: "default"}}},
		Definitions: map[string]string{"discobox": "A Discobox sandbox.", "project": "A Discobox project, by its name."},
	})
	if err != nil {
		t.Fatal(err)
	}
	workersPolicy := workers
	lead, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.SandboxCreateConfig{Name: "lead"},
	})
	if err != nil {
		t.Fatalf("create lead: %v", err)
	}
	req, err := svc.CreateSandboxCredentialRequest(ctx, lead.PoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: lead.ID,
		ID:        serverapi.NewOptString(wellknown.DiscoboxSandbox),
		Uses:      []serverapi.SecretUse{{Description: "create workers"}},
		Policy:    workersPolicy,
	})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	approved, err := svc.ApproveSecretRequest(ctx, projectID, req.ID, services.ApproveSecretRequestBody{Policy: workersPolicy})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	_, err = svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.SandboxCreateConfig{Name: "worker"},
		Grants: []serverapi.SandboxGrant{{
			SecretId: serverapi.NewOptString(approved.SecretID),
			EnvVar:   serverapi.NewOptString("DISCOBOX_TOKEN"),
			Uses:     []serverapi.SecretUse{{Description: "create more workers"}},
			Policy:   workersPolicy,
		}},
	})
	requireStatus(t, err, http.StatusForbidden)
}

// A discobox a sandbox creates has its source delivered the way a person's
// is: by its client, into the pool's cache (ADR 26-10-10-545 §1). The origin a
// sandbox reports is its own claim, and so is the cache it names with it; that
// decides nothing a pool reads, since a sandbox reaches nothing in a cache but
// what it sends (§7). No source is ever read off the server's or a pool's own
// filesystem, so a URL that names a path there is only a name.
func TestASandboxsSourceIsDeliveredAsAPersonsIs(t *testing.T) {
	ctx := context.Background()
	svc, _, st, projectID := newSandboxTestService(t, nil)
	ensureLead(t, st, projectID)
	// A real host path, so it is absolute on the machine running the test.
	directory := t.TempDir()
	lead := auth.WithPrincipal(ctx, auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: projectID, UserID: service.DefaultUserID,
	})
	body := func(name string) services.CreateSandboxBody {
		return services.CreateSandboxBody{
			HarnessName: serverapi.NewOptString("shell"),
			Origin:      serverapi.NewOptOrigin(serverapi.Origin{HostId: "host-user"}),
			Config: serverapi.SandboxCreateConfig{
				Name: name,
				Source: serverapi.NewOptGitSource(serverapi.GitSource{
					Kind:           serverapi.GitSourceKindGit,
					LocalDirectory: serverapi.NewOptString(directory),
					Checkout:       serverapi.NewOptGitSourceCheckout(serverapi.GitSourceCheckout{Commit: serverapi.NewOptString("abc123")}),
				}),
			},
		}
	}

	mine, err := svc.CreateSandbox(ctx, projectID, body("from-the-user"))
	if err != nil {
		t.Fatalf("create sandbox as the user: %v", err)
	}
	forged, err := svc.CreateSandbox(lead, projectID, body("from-a-sandbox"))
	if err != nil {
		t.Fatalf("create sandbox as a sandbox: %v", err)
	}
	for name, sandbox := range map[string]*model.Sandbox{"the user's": mine, "the sandbox's": forged} {
		if sandbox.Source.Delivery != model.GitSourceDeliveryCache || sandbox.Source.CacheKey == nil || *sandbox.Source.CacheKey == "" {
			t.Fatalf("%s source = %+v, want delivered into a pool cache", name, sandbox.Source)
		}
	}

	remote := body("remote")
	u, err := url.Parse("https://github.com/org/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	remote.Config.Source = serverapi.NewOptGitSource(serverapi.GitSource{Kind: serverapi.GitSourceKindGit, URL: serverapi.NewOptURI(*u)})
	created, err := svc.CreateSandbox(lead, projectID, remote)
	if err != nil {
		t.Fatalf("create from a network URL: %v", err)
	}
	if got := *created.Source.CacheKey; got != "remote/github.com/org/repo" {
		t.Fatalf("remote source's cache = %q, want its remote's", got)
	}
}

// A discobox gives on a create only what it was delegated, and only uses the
// judge finds within it; a grant it may not give refuses the whole create,
// which then creates nothing (ADR 26-09-30-782 §1). A person's create is not
// held to any of it.
func TestADiscoboxGivesOnACreateOnlyWhatItWasDelegated(t *testing.T) {
	ctx := context.Background()
	svc, _, st, projectID := newSandboxTestService(t, nil)
	secret := createGitHubSecret(ctx, t, svc, projectID)
	ensureLead(t, st, projectID)
	lead := auth.WithPrincipal(ctx, auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: projectID, UserID: "user-lead",
	})
	create := func(as context.Context, name string) error {
		_, err := svc.CreateSandbox(as, projectID, services.CreateSandboxBody{
			HarnessName: serverapi.NewOptString("shell"),
			Config:      serverapi.SandboxCreateConfig{Name: name},
			Grants:      []serverapi.SandboxGrant{githubGrant(secret.ID)},
		})
		return err
	}
	created := func(name string) bool {
		list, err := st.ListSandboxes(ctx, projectID, "", nil)
		if err != nil {
			t.Fatalf("list sandboxes: %v", err)
		}
		for _, sandbox := range list {
			if sandbox.Name == name {
				return true
			}
		}
		return false
	}

	judgedBy(svc, &grantJudge{allow: true})
	requireStatus(t, create(lead, "undelegated"), http.StatusForbidden)
	if created("undelegated") {
		t.Fatal("a create refused for a grant it could not give created a discobox")
	}

	delegateToLead(t, st, projectID, secret, time.Hour)
	judgedBy(svc, &grantJudge{allow: false})
	requireStatus(t, create(lead, "judged-out"), http.StatusForbidden)
	if created("judged-out") {
		t.Fatal("a create whose grant the judge refused created a discobox")
	}

	if err := create(ctx, "a-persons"); err != nil {
		t.Fatalf("a person's create with a grant: %v", err)
	}
}

// A restricted discobox cannot create one that reaches more than it does
// (ADR 26-10-01-032 §4); a person can.
func TestARestrictedDiscoboxCannotCreateAWiderOne(t *testing.T) {
	ctx := context.Background()
	svc, _, _, projectID := newSandboxTestService(t, nil)
	restricted := func(allow ...string) serverapi.OptEgressPolicy {
		return serverapi.NewOptEgressPolicy(serverapi.EgressPolicy{
			Allow: serverapi.NewOptNilStringArray(allow), DenyByDefault: serverapi.NewOptBool(true),
		})
	}
	leadBox, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.SandboxCreateConfig{Name: "lead", Egress: restricted("github.com")},
	})
	if err != nil {
		t.Fatalf("create lead: %v", err)
	}
	lead := auth.WithPrincipal(ctx, auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: leadBox.ID, ProjectID: projectID, UserID: service.DefaultUserID,
	})
	create := func(ctx context.Context, name string, egress serverapi.OptEgressPolicy) error {
		_, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
			HarnessName: serverapi.NewOptString("shell"),
			Config:      serverapi.SandboxCreateConfig{Name: name, Egress: egress},
		})
		return err
	}

	var statusErr interface{ StatusCode() int }
	if err := create(lead, "open", serverapi.OptEgressPolicy{}); !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusForbidden {
		t.Fatalf("an unrestricted child: err = %v, want forbidden", err)
	}
	if err := create(lead, "wider", restricted("github.com", "evil.example")); !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusForbidden {
		t.Fatalf("a wider child: err = %v, want forbidden", err)
	}
	if err := create(lead, "within", restricted("github.com")); err != nil {
		t.Fatalf("a child within the lead's policy: %v", err)
	}
	if err := create(ctx, "a-persons", serverapi.OptEgressPolicy{}); err != nil {
		t.Fatalf("a person's create: %v", err)
	}
}
