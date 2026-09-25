package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/judge"
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

func (fakeSecretService) ListSecretGrants(context.Context, string, string) ([]model.SecretGrant, error) {
	return []model.SecretGrant{fakeSecretGrant()}, nil
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

func (fakeSecretService) CreateSandboxCredentialRequest(context.Context, string, svcapi.CreateSandboxCredentialRequestBody) (*model.SecretRequest, error) {
	return &model.SecretRequest{ID: "sreq-1", Status: model.SecretRequestStatusPending}, nil
}

func (fakeSecretService) GetSandboxCredentialRequest(context.Context, string, string, string) (*model.SecretRequest, *model.SecretGrant, error) {
	return &model.SecretRequest{ID: "sreq-1", Status: model.SecretRequestStatusPending}, nil, nil
}

func (fakeSecretService) RecordCredentialVerdict(context.Context, string, svcapi.RecordCredentialVerdictBody) error {
	return nil
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
		Host:        "github.com",
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
		Host:      "github.com",
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

// A request verdict's own fields reach the response too: the evidence, the
// answer that asked rather than decided, and the judge that gave it.
func TestListCredentialVerdictsReturnsARequestVerdictsFields(t *testing.T) {
	var got store.CredentialVerdictFilter
	h := New(svcapi.Services{Secrets: capturingVerdictService{filter: &got, rows: []model.CredentialVerdict{{
		ID: "cv_2", ProjectID: "project-1", Kind: model.CredentialVerdictKindRequest, Origin: model.CredentialVerdictOriginJudge,
		SandboxID: "sbx_a", UseID: "use_1",
		Request: &judge.Request{Method: "POST", URL: "https://api.github.com/repos/org/repo/pulls",
			Body: &judge.Body{MediaType: "application/json", Length: 42}},
		Round: 1, Need: &judge.Need{Body: judge.FormJSON, Bytes: 512}, Reason: "the operation is in the body",
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
		!hasNeed || need.Body != serverapi.JudgeNeedBodyJSON || need.Bytes.Or(0) != 512 ||
		v.PromptVersion.Or("") != judge.PromptVersion || v.LatencyMs.Or(0) != 1500 ||
		v.JudgeSandboxId.Or("") != "sbx_judge" || v.HarnessConfigId.Or("") != "hc_1" ||
		v.Image.Or("") != "harness:1" || v.ImageDigest.Or("") != "sha256:one" {
		t.Fatalf("request verdict lost a field on the way out: %+v", v)
	}
}
