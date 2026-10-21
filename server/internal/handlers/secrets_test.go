package handlers

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/policy"
	"github.com/discobox-ai/discobox/pool-agent/poolauth"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	svcapi "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func TestSecretHandlersDoNotReturnSecretValues(t *testing.T) {
	h := New(svcapi.Services{Secrets: fakeSecretService{}})

	listRes, err := h.ListSecrets(context.Background(), serverapi.ListSecretsParams{ProjectId: "project-1"})
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	assertResponseDoesNotContain(t, listRes, "clear-token")
	assertResponseDoesNotContain(t, listRes, "encrypted-token")
	assertResponseDoesNotContain(t, listRes, "value")

	getRes, err := h.GetSecret(context.Background(), serverapi.GetSecretParams{ProjectId: "project-1", SecretId: "secret-1"})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	assertResponseDoesNotContain(t, getRes, "clear-token")
	assertResponseDoesNotContain(t, getRes, "encrypted-token")
	assertResponseDoesNotContain(t, getRes, "value")
}

func TestSecretRequestHandlersDoNotReturnSecretValues(t *testing.T) {
	h := New(svcapi.Services{Secrets: fakeSecretService{}})

	getRes, err := h.GetSecretRequest(context.Background(), serverapi.GetSecretRequestParams{ProjectId: "project-1", RequestId: "request-1"})
	if err != nil {
		t.Fatalf("get secret request: %v", err)
	}
	assertResponseDoesNotContain(t, getRes, "clear-token")
	assertResponseDoesNotContain(t, getRes, "value")

	createRes, err := h.CreateSecretRequest(context.Background(), &serverapi.CreateSecretRequestBody{
		Type: serverapi.CreateSecretRequestBodyTypeToken,
	}, serverapi.CreateSecretRequestParams{ProjectId: "project-1"})
	if err != nil {
		t.Fatalf("create secret request: %v", err)
	}
	assertResponseDoesNotContain(t, createRes, "clear-token")
	assertResponseDoesNotContain(t, createRes, "value")
}

type fakeSecretService struct{}

func (fakeSecretService) ListSecrets(context.Context, string) ([]model.Secret, error) {
	return []model.Secret{fakeSecret()}, nil
}

func (fakeSecretService) CreateSecret(context.Context, string, svcapi.CreateSecretBody) (*model.Secret, error) {
	secret := fakeSecret()
	return &secret, nil
}

func (fakeSecretService) GetSecret(context.Context, string, string) (*model.Secret, error) {
	secret := fakeSecret()
	return &secret, nil
}

func (fakeSecretService) UpdateSecret(context.Context, string, string, svcapi.UpdateSecretBody) (*model.Secret, error) {
	secret := fakeSecret()
	return &secret, nil
}

func (fakeSecretService) RefreshSecret(context.Context, string, string, svcapi.RefreshSecretBody) (*model.Secret, error) {
	secret := fakeSecret()
	return &secret, nil
}

func (fakeSecretService) ListSecretRefreshEvents(context.Context, string, svcapi.SecretRefreshFilter) ([]model.SecretRefreshEvent, error) {
	return nil, nil
}

func (fakeSecretService) DeleteSecret(context.Context, string, string) error {
	return nil
}

func (fakeSecretService) ListSecretRequests(context.Context, string, string) ([]model.SecretRequest, error) {
	request := fakeSecretRequest()
	return []model.SecretRequest{request}, nil
}

func (fakeSecretService) CreateSecretRequest(context.Context, string, svcapi.CreateSecretRequestBody) (*model.SecretRequest, error) {
	request := fakeSecretRequest()
	return &request, nil
}

func (fakeSecretService) GetSecretRequest(context.Context, string, string) (*model.SecretRequest, error) {
	request := fakeSecretRequest()
	return &request, nil
}

func (fakeSecretService) ApproveSecretRequest(context.Context, string, string, svcapi.ApproveSecretRequestBody) (*model.SecretRequest, error) {
	request := fakeSecretRequest()
	return &request, nil
}

func (fakeSecretService) DenySecretRequest(context.Context, string, string) error {
	return nil
}

func (fakeSecretService) ListSecretGrants(context.Context, string, string, string) ([]model.SecretGrant, error) {
	return []model.SecretGrant{fakeSecretGrant()}, nil
}

func (fakeSecretService) ListApprovalRules(context.Context, string) ([]model.ApprovalRule, error) {
	return nil, nil
}

func (fakeSecretService) GetApprovalRule(context.Context, string, string) (*model.ApprovalRule, error) {
	rule := fakeApprovalRule()
	return &rule, nil
}

func (fakeSecretService) CreateApprovalRule(context.Context, string, svcapi.CreateApprovalRuleBody) (*model.ApprovalRule, error) {
	rule := fakeApprovalRule()
	return &rule, nil
}

func (fakeSecretService) UpdateApprovalRule(context.Context, string, string, svcapi.UpdateApprovalRuleBody) (*model.ApprovalRule, error) {
	rule := fakeApprovalRule()
	return &rule, nil
}

func (fakeSecretService) DeleteApprovalRule(context.Context, string, string) error { return nil }

func (fakeSecretService) ProposeApprovalRules(context.Context, string, string) (*svcapi.RuleProposals, error) {
	return &svcapi.RuleProposals{
		Hosts:      []string{"github.com"},
		Purpose:    "use",
		Repository: serverapi.NewOptString("github.com/org/repo"),
		Uses:       serverapi.NewOptRuleUsesProposal(serverapi.RuleUsesProposal{Uses: []string{"read any issue <number>"}, Covers: true}),
	}, nil
}

func (fakeSecretService) ProposeApprovalRuleFromGrant(context.Context, string, string) (*svcapi.RuleProposals, error) {
	return &svcapi.RuleProposals{Hosts: []string{"github.com"}, Purpose: "use"}, nil
}

func fakeApprovalRule() model.ApprovalRule {
	return model.ApprovalRule{
		ID: "rule_1", ProjectID: "project-1", WellKnownID: "com.github.api",
		Repositories: []string{"github.com/org/repo"}, Hosts: []string{"github.com"}, Purpose: "use",
		Uses: []string{"read any issue <number> in org/repo"}, MaxGrantTTL: 3600, Enabled: true,
	}
}

func (fakeSecretService) CreateSecretGrant(context.Context, string, svcapi.CreateSecretGrantBody) (*model.SecretGrant, error) {
	grant := fakeSecretGrant()
	return &grant, nil
}

func (fakeSecretService) RevokeSecretGrant(context.Context, string, string) error {
	return nil
}

func (fakeSecretService) ResolveSandboxSecret(context.Context, string, string, string, string) (*model.SandboxSecretResolution, error) {
	return &model.SandboxSecretResolution{Status: model.SecretRequestStatusPending}, nil
}

func (fakeSecretService) RecordSandboxSecretRejection(context.Context, string, string, string, string, string, string) error {
	return nil
}

func (fakeSecretService) ListSecretRejections(context.Context, string) ([]model.SecretRejection, error) {
	return nil, nil
}

func (fakeSecretService) ListSandboxCredentials(context.Context, string, string) ([]store.AgentCredential, error) {
	return nil, nil
}

func (fakeSecretService) SandboxPolicyFacts(context.Context, string, string) (policy.Facts, error) {
	return policy.Facts{}, nil
}

func (fakeSecretService) CreateSandboxCredentialRequest(context.Context, string, svcapi.CreateSandboxCredentialRequestBody) (*model.SecretRequest, error) {
	return &model.SecretRequest{ID: "sreq-1", Status: model.SecretRequestStatusPending}, nil
}

func (fakeSecretService) GetSandboxCredentialRequest(context.Context, string, string, string) (*model.SecretRequest, *model.SecretGrant, error) {
	return &model.SecretRequest{ID: "sreq-1", Status: model.SecretRequestStatusPending}, nil, nil
}

func (fakeSecretService) ListCredentialVerdicts(context.Context, string, store.CredentialVerdictFilter) ([]model.CredentialVerdict, error) {
	return nil, nil
}

func fakeSecret() model.Secret {
	now := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	return model.Secret{
		ID:             "secret-1",
		ProjectID:      "project-1",
		Name:           "github",
		Type:           model.SecretTypeToken,
		Host:           "github.com",
		MaxGrantTTL:    3600,
		EncryptedValue: []byte(`{"token":"encrypted-token"}`),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

func fakeSecretRequest() model.SecretRequest {
	now := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	return model.SecretRequest{
		ID:          "request-1",
		ProjectID:   "project-1",
		RequestedBy: "user-1",
		Type:        model.SecretTypeToken,
		Hosts:       []string{"api.github.com", "api.githubcopilot.com"},
		SecretID:    "secret-1",
		Status:      model.SecretRequestStatusApproved,
		GrantID:     "grant-1",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func fakeSecretGrant() model.SecretGrant {
	now := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	return model.SecretGrant{
		ID:        "grant-1",
		ProjectID: "project-1",
		SecretID:  "secret-1",
		Scope:     model.SecretGrantScopeProject,
		ScopeKey:  "project-1",
		Hosts:     []string{"api.github.com", "api.githubcopilot.com"},
		GrantedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func assertResponseDoesNotContain(t *testing.T, res any, needle string) {
	t.Helper()
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if strings.Contains(string(data), needle) {
		t.Fatalf("response = %s, did not expect %q", data, needle)
	}
}

// capturingVerdictService records the filter the handler built, so the test
// asserts what reached the service rather than what the handler meant.
type capturingVerdictService struct {
	fakeSecretService
	filter *store.CredentialVerdictFilter
	rows   []model.CredentialVerdict
}

func (c capturingVerdictService) ListCredentialVerdicts(_ context.Context, _ string, filter store.CredentialVerdictFilter) ([]model.CredentialVerdict, error) {
	*c.filter = filter
	return c.rows, nil
}

// allow is tri-state: absent is every verdict, false is denials only. An
// optional bool read with Or(false) would turn "denials only" into "everything"
// and nobody reading a trail of refusals would notice.
func TestListCredentialVerdictsKeepsAllowTriState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow serverapi.OptBool
		want  *bool
	}{
		{name: "absent", allow: serverapi.OptBool{}},
		{name: "denied only", allow: serverapi.NewOptBool(false), want: new(bool)},
		{name: "allowed only", allow: serverapi.NewOptBool(true), want: func() *bool { v := true; return &v }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got store.CredentialVerdictFilter
			h := New(svcapi.Services{Secrets: capturingVerdictService{filter: &got}})
			if _, err := h.ListCredentialVerdicts(context.Background(), serverapi.ListCredentialVerdictsParams{ProjectId: "project-1", Allow: tc.allow}); err != nil {
				t.Fatalf("ListCredentialVerdicts() error = %v", err)
			}
			switch {
			case tc.want == nil && got.Allow != nil:
				t.Fatalf("Allow = %v, want no filter", *got.Allow)
			case tc.want != nil && (got.Allow == nil || *got.Allow != *tc.want):
				t.Fatalf("Allow = %v, want %v", got.Allow, *tc.want)
			}
			if got.Limit != 100 {
				t.Fatalf("Limit = %d, want the default 100", got.Limit)
			}
			if got.Ascending {
				t.Fatal("Ascending with no order asked for, want newest first")
			}
		})
	}
}

func TestListCredentialVerdictsReadsForwardWhenAsked(t *testing.T) {
	var got store.CredentialVerdictFilter
	h := New(svcapi.Services{Secrets: capturingVerdictService{filter: &got}})
	if _, err := h.ListCredentialVerdicts(context.Background(), serverapi.ListCredentialVerdictsParams{
		ProjectId: "project-1", Order: serverapi.NewOptListCredentialVerdictsOrder(serverapi.ListCredentialVerdictsOrderAsc),
	}); err != nil {
		t.Fatalf("ListCredentialVerdicts() error = %v", err)
	}
	if !got.Ascending {
		t.Fatal("order=asc did not reach the store")
	}
}

func TestListCredentialVerdictsFiltersByKind(t *testing.T) {
	var got store.CredentialVerdictFilter
	h := New(svcapi.Services{Secrets: capturingVerdictService{filter: &got}})
	if _, err := h.ListCredentialVerdicts(context.Background(), serverapi.ListCredentialVerdictsParams{
		ProjectId: "project-1", Kind: serverapi.NewOptListCredentialVerdictsKind(serverapi.ListCredentialVerdictsKindRequest),
	}); err != nil {
		t.Fatalf("ListCredentialVerdicts() error = %v", err)
	}
	if got.Kind != model.CredentialVerdictKindRequest {
		t.Fatalf("Kind = %q, want kind=request to reach the store", got.Kind)
	}
}

// A query that matches nothing is an empty list, not an error. A nil slice from
// the service would otherwise encode as null and fail the required array.
func TestListCredentialVerdictsEmptyIsAnEmptyList(t *testing.T) {
	var got store.CredentialVerdictFilter
	h := New(svcapi.Services{Secrets: capturingVerdictService{filter: &got}})
	res, err := h.ListCredentialVerdicts(context.Background(), serverapi.ListCredentialVerdictsParams{ProjectId: "project-1"})
	if err != nil {
		t.Fatalf("ListCredentialVerdicts() error = %v", err)
	}
	body, ok := res.(*serverapi.ListCredentialVerdictsBody)
	if !ok || body.CredentialVerdicts == nil || len(body.CredentialVerdicts) != 0 {
		t.Fatalf("response = %#v, want an empty list", res)
	}
}

// Every field of a recorded verdict reaches the response. The body is built by
// round-tripping the model through JSON into the generated type, which drops a
// field whose name does not match the schema without saying so.
func TestListCredentialVerdictsReturnsEveryField(t *testing.T) {
	createdAt := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	var got store.CredentialVerdictFilter
	h := New(svcapi.Services{Secrets: capturingVerdictService{filter: &got, rows: []model.CredentialVerdict{{
		ID: "cv_1", ProjectID: "project-1", SandboxID: "sbx_gone", GrantID: "grant_1", UseID: "use_1",
		Command: []string{"gh", "pr", "create"}, Allow: false, Reason: "not what was approved",
		Role: "judge", Prompt: "the facts block", LatencyMS: 812, Volunteered: true, CreatedAt: createdAt,
	}}}})
	res, err := h.ListCredentialVerdicts(context.Background(), serverapi.ListCredentialVerdictsParams{ProjectId: "project-1"})
	if err != nil {
		t.Fatalf("ListCredentialVerdicts() error = %v", err)
	}
	body, ok := res.(*serverapi.ListCredentialVerdictsBody)
	if !ok {
		t.Fatalf("response = %T, want the list body", res)
	}
	if len(body.CredentialVerdicts) != 1 {
		t.Fatalf("got %d verdicts, want 1", len(body.CredentialVerdicts))
	}
	v := body.CredentialVerdicts[0]
	if v.ID != "cv_1" || v.ProjectId != "project-1" || v.SandboxId != "sbx_gone" || v.GrantId.Or("") != "grant_1" ||
		v.UseId != "use_1" || strings.Join(v.Command, " ") != "gh pr create" || v.Allow ||
		v.Reason.Or("") != "not what was approved" || v.Role.Or("") != "judge" || v.Prompt.Or("") != "the facts block" ||
		v.LatencyMs.Or(0) != 812 || !v.Volunteered || !v.CreatedAt.Equal(createdAt) {
		t.Fatalf("verdict lost a field on the way out: %+v", v)
	}
}

// A Jev verdict's model and probabilities reach the response, since the
// verdict was decided from them (ADR 26-10-01-324 §6), and so does what Jev
// was sent (ADR 26-10-07-937).
func TestListCredentialVerdictsReturnsAJevVerdictsFields(t *testing.T) {
	var got store.CredentialVerdictFilter
	h := New(svcapi.Services{Secrets: capturingVerdictService{filter: &got, rows: []model.CredentialVerdict{{
		ID: "cv_3", ProjectID: "project-1", Kind: model.CredentialVerdictKindRequest, Origin: model.CredentialVerdictOriginJudge,
		SandboxID: "sbx_a", UseID: "use_1", Round: 1, Allow: true, Reason: "Allowed", PromptVersion: "jev-1",
		Model: "jev-1.13.0", Probabilities: map[string]float64{"within": 0.93, "claims_approval": 0.01},
		JevInput: json.RawMessage(`{"state":{"request":{"method":"GET"}},"model":"jev-1.13.0","questions":{}}`),
	}}}})
	res, err := h.ListCredentialVerdicts(context.Background(), serverapi.ListCredentialVerdictsParams{ProjectId: "project-1"})
	if err != nil {
		t.Fatalf("ListCredentialVerdicts() error = %v", err)
	}
	body, ok := res.(*serverapi.ListCredentialVerdictsBody)
	if !ok || len(body.CredentialVerdicts) != 1 {
		t.Fatalf("response = %#v, want the one verdict", res)
	}
	v := body.CredentialVerdicts[0]
	said := v.Probabilities.Or(nil)
	if v.Model.Or("") != "jev-1.13.0" || said["within"] != 0.93 || said["claims_approval"] != 0.01 {
		t.Fatalf("verdict lost Jev's model or what it said: %+v", v)
	}
	if string(v.JevInput) != `{"state":{"request":{"method":"GET"}},"model":"jev-1.13.0","questions":{}}` {
		t.Fatalf("jevInput = %s, want exactly what Jev was sent", v.JevInput)
	}
}

// A request verdict's own fields reach the response too: the evidence, the
// answer that asked rather than decided, and the judge that gave it.
func TestListCredentialVerdictsReturnsARequestVerdictsFields(t *testing.T) {
	var got store.CredentialVerdictFilter
	h := New(svcapi.Services{Secrets: capturingVerdictService{filter: &got, rows: []model.CredentialVerdict{{
		ID: "cv_2", ProjectID: "project-1", Kind: model.CredentialVerdictKindRequest, Origin: model.CredentialVerdictOriginJudge,
		SandboxID: "sbx_a", UseID: "use_1",
		Request: &judge.Request{Method: "POST", URL: "https://api.github.com/repos/org/repo/pulls",
			Body: &judge.Body{MediaType: "application/json", Length: 42}},
		Round: 1, Need: &judge.Need{Body: true, Bytes: 512}, Reason: "the operation is in the body",
		Role: judge.Role, Prompt: "{}", PromptVersion: judge.PromptVersion, LatencyMS: 1500,
		JudgeSandboxID: "sbx_judge", HarnessConfigID: "hc_1", Image: "harness:1", ImageDigest: "sha256:one",
	}}}})
	res, err := h.ListCredentialVerdicts(context.Background(), serverapi.ListCredentialVerdictsParams{ProjectId: "project-1"})
	if err != nil {
		t.Fatalf("ListCredentialVerdicts() error = %v", err)
	}
	body, ok := res.(*serverapi.ListCredentialVerdictsBody)
	if !ok || len(body.CredentialVerdicts) != 1 {
		t.Fatalf("response = %#v, want the one verdict", res)
	}
	v := body.CredentialVerdicts[0]
	request, hasRequest := v.Request.Get()
	requestBody, hasBody := request.Body.Get()
	need, hasNeed := v.Need.Get()
	if v.Kind.Or("") != serverapi.CredentialVerdictKindRequest || v.Origin.Or("") != serverapi.CredentialVerdictOriginJudge ||
		!hasRequest || request.Method != "POST" || request.URL != "https://api.github.com/repos/org/repo/pulls" ||
		!hasBody || requestBody.Length.Or(0) != 42 || v.Round.Or(0) != 1 ||
		!hasNeed || !need.Body || need.Bytes.Or(0) != 512 ||
		v.PromptVersion.Or("") != judge.PromptVersion || v.LatencyMs.Or(0) != 1500 ||
		v.JudgeSandboxId.Or("") != "sbx_judge" || v.HarnessConfigId.Or("") != "hc_1" ||
		v.Image.Or("") != "harness:1" || v.ImageDigest.Or("") != "sha256:one" {
		t.Fatalf("request verdict lost a field on the way out: %+v", v)
	}
}

// resolvingSecretService approves every resolve with one secret's value.
type resolvingSecretService struct{ fakeSecretService }

func (resolvingSecretService) ResolveSandboxSecret(context.Context, string, string, string, string) (*model.SandboxSecretResolution, error) {
	return &model.SandboxSecretResolution{
		Status:      model.SecretRequestStatusApproved,
		Value:       &model.SecretValue{Token: "real-token"},
		SecretID:    "sec_1",
		ReportScope: []model.HarnessSecretRejectionRule{{Host: "api.example.com", Paths: []string{"/v1/messages"}}},
	}, nil
}

// An approved answer names the secret it is, so the pool's proxy can record
// which secret a request spent (ADR 26-10-01-240).
func TestResolveSandboxSecretNamesTheSecret(t *testing.T) {
	h := New(svcapi.Services{Secrets: resolvingSecretService{}})
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type: auth.PrincipalTypePool, PoolID: "pool-1", Scopes: []string{poolauth.ScopeSecretResolve},
	})
	res, err := h.ResolveSandboxSecret(ctx, &serverapi.ResolveSandboxSecretBody{SandboxId: "sb-1", Sentinel: "SENT", Host: "api.example.com"}, serverapi.ResolveSandboxSecretParams{PoolId: "pool-1"})
	if err != nil {
		t.Fatalf("ResolveSandboxSecret() error = %v", err)
	}
	body, ok := res.(*serverapi.ResolveSandboxSecretResponse)
	if !ok {
		t.Fatalf("response = %T, want the resolution", res)
	}
	if body.Status != serverapi.ResolveSandboxSecretResponseStatusApproved || body.SecretId.Or("") != "sec_1" {
		t.Fatalf("resolution = %+v, want approved and naming sec_1", body)
	}
	// And the requests that answer for it, which the proxy holds its reports
	// to (ADR 26-10-13-595).
	if len(body.ReportScope) != 1 || body.ReportScope[0].Host != "api.example.com" || !slices.Equal(body.ReportScope[0].Paths, []string{"/v1/messages"}) {
		t.Fatalf("report scope = %+v, want api.example.com /v1/messages", body.ReportScope)
	}
}

// A grant and a request carry their policy through the API as stored, with
// what each variable resolved to (ADR 26-10-20-782).
func TestSecretGrantsAndRequestsCarryTheirPolicy(t *testing.T) {
	name := "discobox/*"
	want := &policy.Policy{
		Statements: []policy.Statement{{
			Effect: policy.Allow, Actions: []string{policy.ActionUpdate}, ResourceType: "branch", Name: &name,
			Scope: policy.Scope{Type: "repository", Name: policy.VarPrimaryRepo},
		}},
		Definitions: map[string]string{"branch": "A Git branch.", "repository": "A GitHub repository."},
		Resolved:    map[string]string{policy.VarPrimaryRepo: "acme/web"},
	}
	request, grant := fakeSecretRequest(), fakeSecretGrant()
	request.Policy, grant.Policy = want, want
	read := func(what string, from any) *policy.Policy {
		t.Helper()
		got, err := svcapi.Convert[apimodel.SecretGrant](from)
		if err != nil {
			t.Fatalf("convert %s: %v", what, err)
		}
		sent, ok := got.Policy.Get()
		if !ok {
			t.Fatalf("%s carries no policy", what)
		}
		data, err := sent.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		var back policy.Policy
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatal(err)
		}
		return &back
	}
	if got := read("grant", grant); !reflect.DeepEqual(got, want) {
		t.Fatalf("grant policy = %+v, want %+v", got, want)
	}
	if got, err := svcapi.Convert[apimodel.SecretRequest](request); err != nil || !got.Policy.IsSet() {
		t.Fatalf("request = %+v, %v; want its policy", got.Policy, err)
	}

	status, err := agentCredentialRequestStatus(&request, &grant)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := status.Policy.Get(); !ok || len(got.Statements) != 1 || got.Resolved.Value[policy.VarPrimaryRepo] != "acme/web" {
		t.Fatalf("status policy = %+v, want the grant's", status.Policy)
	}
}

// A grant and a request carry their hosts (ADR 26-10-02-393).
func TestSecretGrantsAndRequestsCarryTheirHosts(t *testing.T) {
	h := New(svcapi.Services{Secrets: fakeSecretService{}})
	ctx := context.Background()
	want := []string{"api.github.com", "api.githubcopilot.com"}

	listRes, err := h.ListSecretGrants(ctx, serverapi.ListSecretGrantsParams{ProjectId: "project-1"})
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	grants := listRes.(*serverapi.ListSecretGrantsBody).SecretGrants
	if len(grants) != 1 || !slices.Equal(grants[0].Hosts, want) {
		t.Fatalf("grants = %+v, want hosts %q", grants, want)
	}

	getRes, err := h.GetSecretRequest(ctx, serverapi.GetSecretRequestParams{ProjectId: "project-1", RequestId: "request-1"})
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if request := getRes.(*serverapi.SecretRequest); !slices.Equal(request.Hosts, want) {
		t.Fatalf("request = %+v, want hosts %q", request, want)
	}
}

// The approver picks a grant's lifetime, so the status the asking sandbox
// polls says what was picked: an expiry, or none for a grant that never lapses.
func TestSandboxCredentialRequestStatusCarriesTheGrantedLifetime(t *testing.T) {
	approved := &model.SecretRequest{ID: "sreq-1", Status: model.SecretRequestStatusApproved}
	uses := []model.SecretUse{{UseID: "use-1", Description: "Push the branch"}}

	expires := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	timed, err := agentCredentialRequestStatus(approved, &model.SecretGrant{Uses: uses, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := timed.ExpiresAt.Get(); !ok || !got.Equal(expires) {
		t.Fatalf("expiresAt = %v (set %v), want %v", got, ok, expires)
	}

	forever, err := agentCredentialRequestStatus(approved, &model.SecretGrant{Uses: uses})
	if err != nil {
		t.Fatal(err)
	}
	if forever.ExpiresAt.IsSet() {
		t.Fatalf("expiresAt = %v on a grant that never expires, want it absent", forever.ExpiresAt)
	}
}

// factsSecretService answers a facts read with one sandbox's record, and
// records which pool and sandbox it was asked for.
type factsSecretService struct {
	fakeSecretService
	facts policy.Facts
	asked *[2]string
}

func (f factsSecretService) SandboxPolicyFacts(_ context.Context, poolID, sandboxID string) (policy.Facts, error) {
	*f.asked = [2]string{poolID, sandboxID}
	return f.facts, nil
}

// A sandbox's facts reach its pool with every variable resolved here, in the
// shape the agent credentials protocol reads them in, so the pool relays the
// control plane's values rather than resolving its own.
func TestSandboxPolicyFactsCarryTheRecordAndItsVariables(t *testing.T) {
	facts := policy.Facts{
		Discobox: policy.Discobox{ID: "sb-1", CreatedBy: "sb-lead", Project: "default"},
		Sources: []policy.Source{{
			Slug: "primary", Primary: true,
			Remote: &policy.Remote{Host: "github.com", Repo: "acme/web", URL: "https://github.com/acme/web"},
			Ref:    &policy.Ref{Type: "branch", Name: "main"}, Commit: "0123abcd",
		}},
	}
	var asked [2]string
	h := New(svcapi.Services{Secrets: factsSecretService{facts: facts, asked: &asked}})
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type: auth.PrincipalTypePool, PoolID: "pool-1", Scopes: []string{poolauth.ScopeCredentialBroker},
	})
	res, err := h.GetSandboxPolicyFacts(ctx, serverapi.GetSandboxPolicyFactsParams{PoolId: "pool-1", SandboxId: "sb-1"})
	if err != nil {
		t.Fatalf("GetSandboxPolicyFacts() error = %v", err)
	}
	if asked != [2]string{"pool-1", "sb-1"} {
		t.Fatalf("asked for %v, want the calling pool's sb-1", asked)
	}
	body, ok := res.(*serverapi.SandboxPolicyFacts)
	if !ok {
		t.Fatalf("response = %T, want the facts", res)
	}
	data, err := body.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var got agentcreds.FactsResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := agentcreds.FactsResponse{Facts: facts, Variables: facts.Variables()}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("facts =\n%+v\nwant\n%+v", got, want)
	}

	resolveOnly := auth.WithPrincipal(context.Background(), auth.Principal{
		Type: auth.PrincipalTypePool, PoolID: "pool-1", Scopes: []string{poolauth.ScopeSecretResolve},
	})
	res, err = h.GetSandboxPolicyFacts(resolveOnly, serverapi.GetSandboxPolicyFactsParams{PoolId: "pool-1", SandboxId: "sb-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.(*serverapi.SandboxPolicyFacts); ok {
		t.Fatal("a pool without the broker scope read a sandbox's facts")
	}
}
