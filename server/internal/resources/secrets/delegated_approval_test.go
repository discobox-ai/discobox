package secrets_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// delegationJudge stands in for the project's judge: it records what it was
// asked about a delegation and answers as it is told. A create's grants are
// asked about at once, so what it records is read with asks.
type delegationJudge struct {
	allow bool
	err   error
	// within, when set, answers instead: yes when it finds the uses within
	// the delegation's.
	within func(services.DelegationAsk) bool
	mu     sync.Mutex
	asked  []services.DelegationAsk
}

func (j *delegationJudge) asks() []services.DelegationAsk {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.asked)
}

func (j *delegationJudge) Judge(context.Context, string, services.JudgeAsk) (judge.Answer, error) {
	return judge.Answer{}, errors.New("not a request judge")
}

func (j *delegationJudge) JudgeCommand(context.Context, string, services.CommandAsk) (judge.Answer, error) {
	return judge.Answer{}, errors.New("not a command judge")
}

func (j *delegationJudge) JudgeDelegation(_ context.Context, _ string, ask services.DelegationAsk) (judge.Answer, error) {
	j.mu.Lock()
	j.asked = append(j.asked, ask)
	j.mu.Unlock()
	if j.err != nil {
		return judge.Answer{}, j.err
	}
	if j.within != nil {
		if j.within(ask) {
			return judge.Answer{Allow: true, Reason: "within"}, nil
		}
		return judge.Answer{Reason: "not within " + strings.Join(ask.Delegated, ", ")}, nil
	}
	return judge.Answer{Allow: j.allow, Reason: "decided"}, nil
}

// The lead discobox answering its workers' requests in these tests.
const leadID = "sbx-lead"

func asLead() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: leadID, ProjectID: "project-1", UserID: "user-1",
	})
}

// delegate gives the lead a delegation grant of secret to host, lapsing after
// lifetime (none when zero), as a person approving its ask to delegate does.
func delegate(t *testing.T, st *store.Store, secret *model.Secret, host string, lifetime time.Duration) *model.SecretGrant {
	t.Helper()
	return delegateFor(t, st, secret, host, lifetime, "read issues, for the discoboxes I create")
}

// delegateFor is delegate, for the uses given.
func delegateFor(t *testing.T, st *store.Store, secret *model.Secret, host string, lifetime time.Duration, uses ...string) *model.SecretGrant {
	t.Helper()
	grant := &model.SecretGrant{
		ProjectID: "project-1", SecretID: secret.ID, Scope: model.SecretGrantScopeSandbox, ScopeKey: leadID,
		Hosts: []string{host}, GrantedBy: "user-1", Purpose: model.SecretGrantPurposeDelegate,
	}
	for i, use := range uses {
		grant.Uses = append(grant.Uses, model.SecretUse{UseID: fmt.Sprintf("use-delegated-%d", i), Description: use})
	}
	if lifetime > 0 {
		expires := time.Now().UTC().Add(lifetime)
		grant.ExpiresAt = &expires
	}
	if err := st.CreateSecretGrant(context.Background(), grant); err != nil {
		t.Fatalf("create delegation grant: %v", err)
	}
	return grant
}

// workerRequest is a worker's ask, as discobox-access files it.
func workerRequest(t *testing.T, svc *resourcesecrets.Service, change func(*services.CreateSandboxCredentialRequestBody)) *model.SecretRequest {
	t.Helper()
	body := services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"},
		Uses: []apimodel.SecretUse{{Description: "read issue 43"}},
	}
	if change != nil {
		change(&body)
	}
	req, err := svc.CreateSandboxCredentialRequest(testPrincipalContext(), testPoolID, body)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	return req
}

func approveAsLead(svc *resourcesecrets.Service, req *model.SecretRequest, input services.ApproveSecretRequestBody) (*model.SecretRequest, error) {
	return svc.ApproveSecretRequest(asLead(), "project-1", req.ID, input)
}

// A discobox approves within a delegation grant it holds, with that grant's
// secret — it names none — for no longer than the delegation lasts
// (ADR 26-09-30-782 §3).
func TestADiscoboxApprovesWithinItsDelegation(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	svc.SetJudge(&delegationJudge{allow: true})
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegate(t, st, secret, "github.com", 30*time.Minute)

	// The agent asked for two hours; the delegation has half an hour left.
	req := workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
		b.GrantTTLSeconds = serverapi.NewOptInt64(7200)
	})
	approved, err := approveAsLead(svc, req, services.ApproveSecretRequestBody{})
	if err != nil {
		t.Fatalf("approve within the delegation: %v", err)
	}
	grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	if grant.SecretID != secret.ID || grant.GrantedBy != leadID || grant.Purpose != model.SecretGrantPurposeUse {
		t.Fatalf("grant = %+v, want a use of the delegated secret, granted by the lead", grant)
	}
	if grant.ExpiresAt == nil || time.Until(*grant.ExpiresAt) > 30*time.Minute {
		t.Fatalf("grant expires %v, want no later than the delegation", grant.ExpiresAt)
	}

	// A lifetime the lead names that outlasts the delegation is refused, not
	// shortened: it is what the lead said, and it is not the lead's to give.
	again := workerRequest(t, svc, nil)
	_, err = approveAsLead(svc, again, services.ApproveSecretRequestBody{GrantTTLSeconds: serverapi.NewOptInt64(3600)})
	requireStatus(t, err, http.StatusForbidden)
}

// A delegation grant that never lapses bounds nothing in time; the grant takes
// the lifetime it would have from a person.
func TestADelegationThatNeverLapsesBoundsNoLifetime(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	svc.SetJudge(&delegationJudge{allow: true})
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegate(t, st, secret, "github.com", 0)
	req := workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
		b.GrantTTLSeconds = serverapi.NewOptInt64(7200)
	})
	approved, err := approveAsLead(svc, req, services.ApproveSecretRequestBody{})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	grant, _ := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if grant == nil || grant.ExpiresAt == nil || time.Until(*grant.ExpiresAt) < 2*time.Hour-time.Minute {
		t.Fatalf("grant = %+v, want the two hours asked for", grant)
	}
}

// What a discobox was not delegated it does not hand on: no delegation at all,
// one for another host, one that has lapsed, or another secret than the one
// it names.
func TestADiscoboxHandsOnNothingItWasNotDelegated(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, st *store.Store, delegated, other *model.Secret)
		input func(other *model.Secret) services.ApproveSecretRequestBody
	}{
		{name: "no delegation"},
		{name: "a delegation to another host", setup: func(t *testing.T, st *store.Store, delegated, _ *model.Secret) {
			delegate(t, st, delegated, "gitlab.com", time.Hour)
		}},
		{name: "a lapsed delegation", setup: func(t *testing.T, st *store.Store, delegated, _ *model.Secret) {
			grant := delegate(t, st, delegated, "github.com", time.Hour)
			lapsed := time.Now().UTC().Add(-time.Minute)
			grant.ExpiresAt = &lapsed
			if err := st.UpdateSecretGrant(context.Background(), grant); err != nil {
				t.Fatalf("lapse delegation: %v", err)
			}
		}},
		{name: "naming a secret it was not delegated",
			setup: func(t *testing.T, st *store.Store, delegated, _ *model.Secret) {
				delegate(t, st, delegated, "github.com", time.Hour)
			},
			input: func(other *model.Secret) services.ApproveSecretRequestBody {
				return services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(other.ID)}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testPrincipalContext()
			svc, st := newAgentCredentialService(t)
			svc.SetJudge(&delegationJudge{allow: true})
			delegated := createBoundSecret(ctx, t, svc, "github", "", 86400)
			other := createBoundSecret(ctx, t, svc, "github-admin", "", 86400)
			if tc.setup != nil {
				tc.setup(t, st, delegated, other)
			}
			input := services.ApproveSecretRequestBody{}
			if tc.input != nil {
				input = tc.input(other)
			}
			_, err := approveAsLead(svc, workerRequest(t, svc, nil), input)
			requireStatus(t, err, http.StatusForbidden)
		})
	}
}

// Delegated more than one secret that fits, a discobox names which — and only
// among them.
func TestADiscoboxDelegatedTwoSecretsNamesWhich(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	svc.SetJudge(&delegationJudge{allow: true})
	first := createBoundSecret(ctx, t, svc, "github", "", 86400)
	second := createBoundSecret(ctx, t, svc, "github-bot", "", 86400)
	delegate(t, st, first, "github.com", time.Hour)
	delegate(t, st, second, "github.com", time.Hour)

	_, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{})
	requireStatus(t, err, http.StatusBadRequest)

	approved, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(second.ID)})
	if err != nil {
		t.Fatalf("approve naming a delegated secret: %v", err)
	}
	if approved.SecretID != second.ID {
		t.Fatalf("approved with %s, want the one named", approved.SecretID)
	}
}

// A well-known credential is answered by the secret marked for it, and a
// delegation of any other secret does not hand it on.
func TestADiscoboxHandsOnAWellKnownCredentialOnlyByItsSecret(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	svc.SetJudge(&delegationJudge{allow: true})
	unmarked := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegate(t, st, unmarked, "github.com", time.Hour)
	wellKnown := func(b *services.CreateSandboxCredentialRequestBody) {
		b.ID = serverapi.NewOptString("com.github.api")
		b.Name, b.EnvVar, b.Hosts = "", "", nil
	}
	_, err := approveAsLead(svc, workerRequest(t, svc, wellKnown), services.ApproveSecretRequestBody{})
	requireStatus(t, err, http.StatusForbidden)

	if err := st.MarkSecretWellKnown(ctx, "project-1", unmarked.ID, "com.github.api"); err != nil {
		t.Fatalf("mark secret: %v", err)
	}
	if _, err := approveAsLead(svc, workerRequest(t, svc, wellKnown), services.ApproveSecretRequestBody{}); err != nil {
		t.Fatalf("approve the well-known credential by its delegated secret: %v", err)
	}
}

// A discobox never approves a request to delegate, nor one that names no uses,
// however much it was delegated: both are a person's to answer.
func TestADiscoboxNeverApprovesADelegationOrAnUnnamedUse(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	svc.SetJudge(&delegationJudge{allow: true})
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegate(t, st, secret, "github.com", time.Hour)

	toDelegate := workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
		b.Purpose = serverapi.NewOptCreateSandboxCredentialRequestBodyPurpose(serverapi.CreateSandboxCredentialRequestBodyPurposeDelegate)
	})
	_, err := approveAsLead(svc, toDelegate, services.ApproveSecretRequestBody{})
	requireStatus(t, err, http.StatusForbidden)

	reactive := &model.SecretRequest{
		ID: "sreq-reactive", ProjectID: "project-1", SandboxID: testSandboxID, RequestedBy: "pool:" + testPoolID,
		Type: "token", Hosts: []string{"api.github.com"}, Status: model.SecretRequestStatusPending,
	}
	if err := st.CreateSecretRequest(ctx, reactive); err != nil {
		t.Fatalf("create reactive request: %v", err)
	}
	_, err = approveAsLead(svc, reactive, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)})
	requireStatus(t, err, http.StatusForbidden)
}

// Of delegations that hold the uses alike, an approval is made under the one
// that lets the grant last longest, not the newest: a short-lived delegation
// granted last does not cut short a grant an older one that never lapses
// allows. Named or not, the lifetime is held to that one delegation.
func TestADiscoboxApprovesUnderItsLongestLivedDelegation(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	svc.SetJudge(&delegationJudge{allow: true})
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegate(t, st, secret, "github.com", 0)
	time.Sleep(10 * time.Millisecond) // granted after it, so newest first
	delegate(t, st, secret, "github.com", 5*time.Minute)

	asked := workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
		b.GrantTTLSeconds = serverapi.NewOptInt64(7200)
	})
	approved, err := approveAsLead(svc, asked, services.ApproveSecretRequestBody{})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	grant, _ := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
	if grant == nil || grant.ExpiresAt == nil || time.Until(*grant.ExpiresAt) < 2*time.Hour-time.Minute {
		t.Fatalf("grant = %+v, want the two hours the lasting delegation allows", grant)
	}

	if _, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{GrantTTLSeconds: serverapi.NewOptInt64(3600)}); err != nil {
		t.Fatalf("approve an hour named: %v", err)
	}
}

// A discobox listing secrets sees only those it holds a live delegation grant
// of — what it may hand on, and so may name — and a person sees every one
// (ADR 26-09-30-782 §3).
func TestADiscoboxListsOnlyTheSecretsItWasDelegated(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	svc.SetJudge(&delegationJudge{allow: true})
	delegated := createBoundSecret(ctx, t, svc, "github", "", 86400)
	lapsed := createBoundSecret(ctx, t, svc, "gitlab", "", 86400)
	createBoundSecret(ctx, t, svc, "npm", "", 86400)
	delegate(t, st, delegated, "github.com", time.Hour)
	grant := delegate(t, st, lapsed, "gitlab.com", time.Hour)
	expired := time.Now().UTC().Add(-time.Minute)
	grant.ExpiresAt = &expired
	if err := st.UpdateSecretGrant(ctx, grant); err != nil {
		t.Fatalf("lapse delegation: %v", err)
	}

	listed, err := svc.ListSecrets(asLead(), "project-1")
	if err != nil || len(listed) != 1 || listed[0].ID != delegated.ID {
		t.Fatalf("listed as the lead = %+v, %v; want only the secret it holds a live delegation of", listed, err)
	}
	all, err := svc.ListSecrets(ctx, "project-1")
	if err != nil || len(all) != 3 {
		t.Fatalf("listed as a person = %d, %v; want every secret", len(all), err)
	}
}

// A secret a discobox was not delegated answers its --secret-id the same as one
// that does not exist, so naming IDs cannot tell it which the project holds.
func TestASecretNotDelegatedAnswersAsIfItDidNotExist(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	svc.SetJudge(&delegationJudge{allow: true})
	delegated := createBoundSecret(ctx, t, svc, "github", "", 86400)
	other := createBoundSecret(ctx, t, svc, "github-admin", "", 86400)
	delegate(t, st, delegated, "github.com", time.Hour)

	for _, named := range []string{other.ID, other.ID[:len(other.ID)-3], "sec_doesnotexist"} {
		_, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(named)})
		requireStatus(t, err, http.StatusForbidden)
	}
	// A prefix of the delegated secret's ID names it.
	if _, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(delegated.ID[:len(delegated.ID)-3])}); err != nil {
		t.Fatalf("approve naming the delegated secret by a prefix: %v", err)
	}
}

// Whether the uses handed on fall within the delegation's is the judge's to
// say, asked with the delegation it approves under and the uses it would grant
// — narrowed, when the approver narrowed them. A refusal, or no judge at all,
// refuses the approval (ADR 26-09-30-782 §3).
func TestADiscoboxHandsOnOnlyUsesTheJudgeFindsWithinItsDelegation(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegation := delegate(t, st, secret, "github.com", time.Hour)

	judging := &delegationJudge{allow: true}
	svc.SetJudge(judging)
	narrowed := []apimodel.SecretUse{{Description: "gh api GET repos/org/repo/issues/43"}}
	if _, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{
		Uses: serverapi.NewOptNilSecretUseArray(narrowed),
	}); err != nil {
		t.Fatalf("approve what the judge allows: %v", err)
	}
	if len(judging.asks()) != 1 {
		t.Fatalf("judge asked %d times, want once", len(judging.asks()))
	}
	asked := judging.asks()[0]
	if asked.ApproverID != leadID || asked.RequestID == "" || asked.DelegationGrantID != delegation.ID ||
		len(asked.Delegated) != 1 || asked.Delegated[0] != delegation.Uses[0].Description ||
		len(asked.Uses) != 1 || asked.Uses[0] != narrowed[0].Description || !slices.Equal(asked.Hosts, []string{"api.github.com"}) {
		t.Fatalf("asked = %+v, want the delegation it approves under and the narrowed use", asked)
	}

	svc.SetJudge(&delegationJudge{allow: false})
	_, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{})
	requireStatus(t, err, http.StatusForbidden)

	svc.SetJudge(nil)
	_, err = approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{})
	requireStatus(t, err, http.StatusForbidden)
}

// A delegation whose uses changed after the judge read them is not the one
// the approval was judged under, and the transaction refuses it.
func TestADelegationChangedAfterJudgingIsNotApprovedUnder(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegation := delegate(t, st, secret, "github.com", time.Hour)
	svc.SetJudge(judgeThen(func() {
		delegation.Uses = []model.SecretUse{{UseID: "use-wider", Description: "anything on github"}}
		if err := st.UpdateSecretGrant(context.Background(), delegation); err != nil {
			t.Errorf("widen delegation: %v", err)
		}
	}))
	_, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{})
	requireStatus(t, err, http.StatusForbidden)
}

// judgeThen allows, after doing something between the judge's answer and the
// approval's transaction.
func judgeThen(then func()) services.JudgeService {
	return &afterJudge{then: then}
}

type afterJudge struct{ then func() }

func (j *afterJudge) Judge(context.Context, string, services.JudgeAsk) (judge.Answer, error) {
	return judge.Answer{}, errors.New("not a request judge")
}

func (j *afterJudge) JudgeCommand(context.Context, string, services.CommandAsk) (judge.Answer, error) {
	return judge.Answer{}, errors.New("not a command judge")
}

func (j *afterJudge) JudgeDelegation(context.Context, string, services.DelegationAsk) (judge.Answer, error) {
	j.then()
	return judge.Answer{Allow: true, Reason: "within"}, nil
}

// The judge is asked last: an approval a cheaper check refuses — a lifetime
// past the secret's limit, here — never reaches it, so a delegation verdict is
// the decision about an approval that would otherwise go through.
func TestAnApprovalRefusedWithoutTheJudgeDoesNotAskIt(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 600)
	delegate(t, st, secret, "github.com", 0)
	judging := &delegationJudge{allow: true}
	svc.SetJudge(judging)

	_, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{GrantTTLSeconds: serverapi.NewOptInt64(3600)})
	if err == nil {
		t.Fatal("an approval past the secret's limit went through")
	}
	if len(judging.asks()) != 0 {
		t.Fatalf("the judge was asked %d times about an approval refused without it", len(judging.asks()))
	}
}

// The grants a discobox gives on a create are held to the delegations they
// were made under once more in the create's transaction: one revoked between
// preparing the grants and storing them refuses the create (ADR 26-09-30-782 §1).
func TestACreatesGrantsAreHeldToTheirDelegationsWhenStored(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	svc.SetJudge(&delegationJudge{allow: true})
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegation := delegate(t, st, secret, "github.com", time.Hour)

	grants, err := svc.PrepareSandboxGrants(asLead(), "project-1", "sbx-new", []apimodel.SandboxGrant{{
		SecretId: serverapi.NewOptString(secret.ID), EnvVar: serverapi.NewOptString("GH_TOKEN"),
		Hosts: []string{"api.github.com"}, Uses: []apimodel.SecretUse{{Description: "read issue 43"}},
	}})
	if err != nil {
		t.Fatalf("prepare grants: %v", err)
	}
	if len(grants.Delegations) != 1 || grants.Delegations[0].ID != delegation.ID || grants.Grantor != leadID {
		t.Fatalf("grants = %+v, want the one made under the lead's delegation", grants)
	}
	if err := resourcesecrets.HoldDelegations(ctx, st, grants); err != nil {
		t.Fatalf("hold a live delegation: %v", err)
	}

	lapsed := time.Now().UTC().Add(-time.Minute)
	delegation.ExpiresAt = &lapsed
	if err := st.UpdateSecretGrant(ctx, delegation); err != nil {
		t.Fatalf("lapse delegation: %v", err)
	}
	requireStatus(t, resourcesecrets.HoldDelegations(ctx, st, grants), http.StatusForbidden)
}

// A secret a discobox gives on a create is resolved among what it was
// delegated before anything reads it: one it was not delegated — by full ID,
// by prefix, or by a well-known ID whose secret is not one of them — answers
// the same as one that does not exist.
func TestACreatesSecretNotDelegatedAnswersAsIfItDidNotExist(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	svc.SetJudge(&delegationJudge{allow: true})
	delegated := createBoundSecret(ctx, t, svc, "github", "", 86400)
	other := createBoundSecret(ctx, t, svc, "github-admin", "api.github.com", 86400)
	delegate(t, st, delegated, "github.com", time.Hour)
	grant := func(secretID string) []apimodel.SandboxGrant {
		return []apimodel.SandboxGrant{{
			SecretId: serverapi.NewOptString(secretID), EnvVar: serverapi.NewOptString("GH_TOKEN"),
			Hosts: []string{"api.github.com"}, Uses: []apimodel.SecretUse{{Description: "read issue 43"}},
		}}
	}
	for _, named := range []string{other.ID, other.ID[:len(other.ID)-3], "sec_doesnotexist"} {
		_, err := svc.PrepareSandboxGrants(asLead(), "project-1", "sbx-new", grant(named))
		requireStatus(t, err, http.StatusForbidden)
	}
	wellKnown := []apimodel.SandboxGrant{{WellKnownId: serverapi.NewOptString("com.github.api"), Uses: []apimodel.SecretUse{{Description: "read issue 43"}}}}
	_, err := svc.PrepareSandboxGrants(asLead(), "project-1", "sbx-new", wellKnown)
	requireStatus(t, err, http.StatusForbidden)

	if _, err := svc.PrepareSandboxGrants(asLead(), "project-1", "sbx-new", grant(delegated.ID[:len(delegated.ID)-3])); err != nil {
		t.Fatalf("give the delegated secret named by a prefix: %v", err)
	}
}

// A delegation with less than a second left fits no lifetime: rounded down it
// is zero, which is forever, so it is refused rather than minted as a grant
// that never expires.
func TestADelegationWithNothingLeftHandsNothingOn(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	svc.SetJudge(&delegationJudge{allow: true})
	secret := createBoundSecret(ctx, t, svc, "github", "", 0)
	delegate(t, st, secret, "github.com", 1500*time.Millisecond)
	req := workerRequest(t, svc, nil)
	time.Sleep(700 * time.Millisecond) // under a second left, still live
	_, err := approveAsLead(svc, req, services.ApproveSecretRequestBody{})
	requireStatus(t, err, http.StatusForbidden)
}

// Every grant a create gives is put to the judge, at once rather than one
// after another, and one refusal refuses them all.
func TestEachGrantACreateGivesIsJudged(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 86400)
	delegate(t, st, secret, "github.com", time.Hour)
	grants := []apimodel.SandboxGrant{
		{SecretId: serverapi.NewOptString(secret.ID), EnvVar: serverapi.NewOptString("GH_TOKEN"),
			Hosts: []string{"api.github.com"}, Uses: []apimodel.SecretUse{{Description: "read issue 43"}}},
		{SecretId: serverapi.NewOptString(secret.ID), EnvVar: serverapi.NewOptString("GH_TOKEN"),
			Hosts: []string{"api.github.com"}, Uses: []apimodel.SecretUse{{Description: "read issue 44"}}},
	}
	judging := &countingJudge{allow: true}
	svc.SetJudge(judging)
	if _, err := svc.PrepareSandboxGrants(asLead(), "project-1", "sbx-new", grants); err != nil {
		t.Fatalf("prepare two grants: %v", err)
	}
	if judging.asked.Load() != 2 {
		t.Fatalf("judge asked %d times, want once per grant", judging.asked.Load())
	}
	svc.SetJudge(&countingJudge{allow: false})
	_, err := svc.PrepareSandboxGrants(asLead(), "project-1", "sbx-new", grants)
	requireStatus(t, err, http.StatusForbidden)
}

// countingJudge is safe to ask from several goroutines at once.
type countingJudge struct {
	allow bool
	asked atomic.Int32
}

func (j *countingJudge) Judge(context.Context, string, services.JudgeAsk) (judge.Answer, error) {
	return judge.Answer{}, errors.New("not a request judge")
}

func (j *countingJudge) JudgeCommand(context.Context, string, services.CommandAsk) (judge.Answer, error) {
	return judge.Answer{}, errors.New("not a command judge")
}

func (j *countingJudge) JudgeDelegation(context.Context, string, services.DelegationAsk) (judge.Answer, error) {
	j.asked.Add(1)
	return judge.Answer{Allow: j.allow, Reason: "decided"}, nil
}

// The issue's lead (#88): three delegations of one secret, each for other
// uses, the one lapsing last for the narrowest. Each approval is made under a
// delegation the judge finds its uses within — the longest-lived that holds
// them, not the longest-lived it holds — and its lifetime is fitted to that one.
func TestADiscoboxApprovesUnderADelegationThatHoldsTheUses(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 2*86400)
	push := delegateFor(t, st, secret, "github.com", time.Hour, "push a worker's discobox/issue-<n> branch")
	rerun := delegateFor(t, st, secret, "github.com", 2*time.Hour, "push a worker's discobox/issue-<n> branch", "gh run rerun --failed")
	copilot := delegateFor(t, st, secret, "github.com", 3*time.Hour, "request a Copilot review and read it")
	judging := &delegationJudge{within: func(ask services.DelegationAsk) bool {
		for _, use := range ask.Uses {
			if !slices.Contains(ask.Delegated, use) {
				return false
			}
		}
		return true
	}}
	svc.SetJudge(judging)
	asking := func(use string) *model.SecretRequest {
		return workerRequest(t, svc, func(b *services.CreateSandboxCredentialRequestBody) {
			b.Uses = []apimodel.SecretUse{{Description: use}}
			b.GrantTTLSeconds = serverapi.NewOptInt64(24 * 3600)
		})
	}
	// Each is asked about longest-lived first, and the first yes ends it, so
	// the one allow verdict an approval leaves is the delegation it was made
	// under.
	madeUnder := func(use string, delegation *model.SecretGrant, asked ...*model.SecretGrant) {
		t.Helper()
		before := len(judging.asks())
		approved, err := approveAsLead(svc, asking(use), services.ApproveSecretRequestBody{})
		var askedOf []string
		for _, ask := range judging.asks()[before:] {
			askedOf = append(askedOf, ask.DelegationGrantID)
		}
		var want []string
		for _, of := range asked {
			want = append(want, of.ID)
		}
		if !slices.Equal(askedOf, want) {
			t.Fatalf("approving %q asked about %v, want %v", use, askedOf, want)
		}
		if err != nil {
			t.Fatalf("approve %q: %v", use, err)
		}
		grant, err := st.GetSecretGrant(ctx, "project-1", approved.GrantID)
		if err != nil {
			t.Fatalf("get grant: %v", err)
		}
		if grant.ExpiresAt == nil || grant.ExpiresAt.After(*delegation.ExpiresAt) || delegation.ExpiresAt.Sub(*grant.ExpiresAt) > time.Minute {
			t.Fatalf("grant of %q expires %v, want fitted to the delegation it is made under, lapsing %v", use, grant.ExpiresAt, delegation.ExpiresAt)
		}
	}
	madeUnder("gh run rerun --failed", rerun, copilot, rerun)
	madeUnder("push a worker's discobox/issue-<n> branch", rerun, copilot, rerun)
	madeUnder("request a Copilot review and read it", copilot, copilot)

	// Each delegation asked about is asked of on its own: the uses are never
	// read against several together.
	for _, ask := range judging.asks() {
		var of *model.SecretGrant
		for _, delegation := range []*model.SecretGrant{push, rerun, copilot} {
			if delegation.ID == ask.DelegationGrantID {
				of = delegation
			}
		}
		if of == nil || !slices.Equal(ask.Delegated, useDescriptions(of.Uses)) {
			t.Fatalf("asked %+v, want one delegation's own uses", ask)
		}
	}

	// Uses none holds are refused, with why under each.
	_, err := approveAsLead(svc, asking("push to main"), services.ApproveSecretRequestBody{})
	requireStatus(t, err, http.StatusForbidden)
	for _, delegation := range []*model.SecretGrant{push, rerun, copilot} {
		if !strings.Contains(err.Error(), delegation.ID) {
			t.Fatalf("refusal %q does not say why under %s", err, delegation.ID)
		}
	}
}

// A lifetime the approver names is held to the delegations it fits: one it
// outlasts is not asked about, and the grant is made under one it fits that
// holds the uses — even when the one it outlasts would have held them too.
func TestANamedLifetimeIsJudgedOnlyUnderDelegationsItFits(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 2*86400)
	short := delegateFor(t, st, secret, "github.com", time.Hour, "read issue 43")
	long := delegateFor(t, st, secret, "github.com", 3*time.Hour, "request a Copilot review and read it")
	judging := &delegationJudge{within: func(ask services.DelegationAsk) bool { return ask.DelegationGrantID == short.ID }}
	svc.SetJudge(judging)

	_, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{GrantTTLSeconds: serverapi.NewOptInt64(2 * 3600)})
	requireStatus(t, err, http.StatusForbidden)
	for _, ask := range judging.asks() {
		if ask.DelegationGrantID != long.ID {
			t.Fatalf("asked about %s, which two hours outlasts", ask.DelegationGrantID)
		}
	}

	if _, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{GrantTTLSeconds: serverapi.NewOptInt64(1800)}); err != nil {
		t.Fatalf("approve half an hour under the delegation that holds the uses: %v", err)
	}
}

// Delegations whose uses say the same are one question: the judge is asked
// once, of the longest-lived, which is the one the grant is made under.
func TestDelegationsWithTheSameUsesAreJudgedOnce(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 2*86400)
	delegate(t, st, secret, "github.com", time.Hour)
	longest := delegate(t, st, secret, "github.com", 2*time.Hour)
	judging := &delegationJudge{allow: true}
	svc.SetJudge(judging)
	if _, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if asks := judging.asks(); len(asks) != 1 || asks[0].DelegationGrantID != longest.ID {
		t.Fatalf("asked %+v, want once, of the longest-lived", asks)
	}
}

// A judge that could not answer about one delegation does not refuse an
// approval another holds; when none holds it, that the judge could not
// answer is the refusal, since the one it could not answer about might have.
func TestADelegationTheJudgeCannotAnswerAboutLeavesTheOthers(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 2*86400)
	unreachable := delegateFor(t, st, secret, "github.com", 2*time.Hour, "request a Copilot review and read it")
	delegate(t, st, secret, "github.com", time.Hour)
	unavailable := errors.New("the judge did not answer")
	svc.SetJudge(&errorUnderJudge{delegationID: unreachable.ID, err: unavailable, allow: true})
	if _, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{}); err != nil {
		t.Fatalf("approve under the delegation the judge answered about: %v", err)
	}
	svc.SetJudge(&errorUnderJudge{delegationID: unreachable.ID, err: unavailable})
	if _, err := approveAsLead(svc, workerRequest(t, svc, nil), services.ApproveSecretRequestBody{}); !errors.Is(err, unavailable) {
		t.Fatalf("approve = %v, want the judge's failure to answer", err)
	}
}

// errorUnderJudge fails to answer about one delegation, and answers allow
// about every other.
type errorUnderJudge struct {
	delegationID string
	err          error
	allow        bool
}

func (j *errorUnderJudge) Judge(context.Context, string, services.JudgeAsk) (judge.Answer, error) {
	return judge.Answer{}, errors.New("not a request judge")
}

func (j *errorUnderJudge) JudgeCommand(context.Context, string, services.CommandAsk) (judge.Answer, error) {
	return judge.Answer{}, errors.New("not a command judge")
}

func (j *errorUnderJudge) JudgeDelegation(_ context.Context, _ string, ask services.DelegationAsk) (judge.Answer, error) {
	if ask.DelegationGrantID == j.delegationID {
		return judge.Answer{}, j.err
	}
	return judge.Answer{Allow: j.allow, Reason: "decided"}, nil
}

// A grant a discobox gives on a create is made under a delegation that holds
// its uses, as an approval is, and a lifetime it did not name is fitted to
// that one rather than to the longest-lived it holds.
func TestACreatesGrantIsMadeUnderADelegationThatHoldsItsUses(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoundSecret(ctx, t, svc, "github", "", 2*86400)
	holds := delegateFor(t, st, secret, "github.com", time.Hour, "read issue 43")
	delegateFor(t, st, secret, "github.com", 3*time.Hour, "request a Copilot review and read it")
	svc.SetJudge(&delegationJudge{within: func(ask services.DelegationAsk) bool { return ask.DelegationGrantID == holds.ID }})

	grants, err := svc.PrepareSandboxGrants(asLead(), "project-1", "sbx-new", []apimodel.SandboxGrant{{
		SecretId: serverapi.NewOptString(secret.ID), EnvVar: serverapi.NewOptString("GH_TOKEN"),
		Hosts: []string{"api.github.com"}, Uses: []apimodel.SecretUse{{Description: "read issue 43"}},
	}})
	if err != nil {
		t.Fatalf("prepare grants: %v", err)
	}
	if len(grants.Delegations) != 1 || grants.Delegations[0].ID != holds.ID {
		t.Fatalf("grants made under %+v, want the delegation that holds the uses", grants.Delegations)
	}
	grant := grants.Grants[0]
	if grant.ExpiresAt == nil || grant.ExpiresAt.After(*holds.ExpiresAt) {
		t.Fatalf("grant expires %v, want no later than its delegation, %v", grant.ExpiresAt, holds.ExpiresAt)
	}
	if err := resourcesecrets.HoldDelegations(ctx, st, grants); err != nil {
		t.Fatalf("hold the delegation it was made under: %v", err)
	}
}

// useDescriptions is what a delegation's uses say.
func useDescriptions(uses []model.SecretUse) []string {
	var out []string
	for _, use := range uses {
		out = append(out, use.Description)
	}
	return out
}
