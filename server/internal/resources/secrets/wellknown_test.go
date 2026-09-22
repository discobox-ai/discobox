package secrets_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/wellknown"
)

// askFor asks for a well-known credential by ID, spelling out nothing else.
func askFor(ctx context.Context, t *testing.T, svc *resourcesecrets.Service, id string) *model.SecretRequest {
	t.Helper()
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID,
		ID:        serverapi.NewOptString(id),
		Uses:      []apimodel.SecretUse{{Description: "open a pull request against org/repo"}},
	})
	if err != nil {
		t.Fatalf("ask for %s: %v", id, err)
	}
	return req
}

func requireStatus(t *testing.T, err error, want int) {
	t.Helper()
	var statusErr apperrors.StatusError
	if !errors.As(err, &statusErr) || statusErr.Status != want {
		t.Fatalf("err = %v, want %d", err, want)
	}
}

// An ask by ID carries the name, variable, and host the ID names, and keeps the
// ID for the approval to bind by.
func TestAnAskByWellKnownIDCarriesWhatTheIDNames(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)

	req := askFor(ctx, t, svc, wellknown.GitHubAPI)
	if req.WellKnownID != wellknown.GitHubAPI || req.Name != "github" || req.EnvName != "GH_TOKEN" || req.Host != "github.com" {
		t.Fatalf("request = %+v, want com.github.api's name, variable, and host", req)
	}

	// A host beneath the one the ID names is the narrower ask, not a
	// contradiction: an agent that only calls the API may say so and be
	// granted that alone.
	narrower, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID,
		ID:        serverapi.NewOptString(wellknown.GitHubAPI),
		Host:      "api.github.com",
		Uses:      []apimodel.SecretUse{{Description: "read the issues in org/repo"}},
	})
	if err != nil || narrower.Host != "api.github.com" || narrower.EnvName != "GH_TOKEN" {
		t.Fatalf("request = %+v, %v; want the narrower host kept", narrower, err)
	}

	for name, body := range map[string]services.CreateSandboxCredentialRequestBody{
		"an unknown ID":             {SandboxId: testSandboxID, ID: serverapi.NewOptString("com.example.nothing")},
		"a variable it contradicts": {SandboxId: testSandboxID, ID: serverapi.NewOptString(wellknown.GitHubAPI), EnvVar: "GITHUB_TOKEN"},
		"a host it is not sent to":  {SandboxId: testSandboxID, ID: serverapi.NewOptString(wellknown.GitHubAPI), Host: "gitlab.com"},
	} {
		t.Run(name, func(t *testing.T) {
			body.Uses = []apimodel.SecretUse{{Description: "open a pull request"}}
			_, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, body)
			requireStatus(t, err, http.StatusBadRequest)
		})
	}
}

// A bound credential asks which secret answers it once: the secret named on the
// first approval is marked, and later approvals name none.
func TestTheFirstApprovalOfABoundCredentialMarksItsSecret(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)

	first := askFor(ctx, t, svc, wellknown.GitHubAPI)
	_, err := svc.ApproveSecretRequest(ctx, "project-1", first.ID, services.ApproveSecretRequestBody{})
	requireStatus(t, err, http.StatusBadRequest)
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", first.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)}); err != nil {
		t.Fatalf("approve naming the secret: %v", err)
	}
	marked, err := st.FindSecretByWellKnownID(ctx, "project-1", wellknown.GitHubAPI)
	if err != nil || marked.ID != secret.ID {
		t.Fatalf("marked = %+v, %v; want the secret the first approval named", marked, err)
	}

	second, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID,
		ID:        serverapi.NewOptString(wellknown.GitHubAPI),
		Uses:      []apimodel.SecretUse{{Description: "read the issues in org/repo"}},
	})
	if err != nil {
		t.Fatalf("ask again: %v", err)
	}
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", second.ID, services.ApproveSecretRequestBody{})
	if err != nil || approved.SecretID != secret.ID {
		t.Fatalf("approved = %+v, %v; want the marked secret bound without being named", approved, err)
	}
}

// The mark is what every later approval falls back on, so it is only written
// once an approval has actually gone through. An approval that fails leaves
// the project with no answer to the ID rather than a wrong one.
func TestAFailedApprovalMarksNothing(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)

	req := askFor(ctx, t, svc, wellknown.GitHubAPI)
	// Project scope is refused for a protocol request (ADR 0031 §5).
	_, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID),
		Scope:    serverapi.NewOptApproveSecretRequestBodyScope(serverapi.ApproveSecretRequestBodyScopeProject),
	})
	requireStatus(t, err, http.StatusBadRequest)
	if _, err := st.FindSecretByWellKnownID(ctx, "project-1", wellknown.GitHubAPI); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want no secret marked by an approval that failed", err)
	}
}

// An open ask reaches the same approval a retry does, so what the approval
// would bind has to match: a plain ask for the same variable and host is not
// the ask that carries an ID, and reusing it would lose the ID silently.
func TestAnAskByIDDoesNotReuseAPlainAsk(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)

	plain, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID,
		Name:      "github",
		EnvVar:    "GH_TOKEN",
		Host:      "github.com",
		Uses:      []apimodel.SecretUse{{Description: "read the issues in org/repo"}},
	})
	if err != nil {
		t.Fatalf("plain ask: %v", err)
	}
	byID := askFor(ctx, t, svc, wellknown.GitHubAPI)
	if byID.ID == plain.ID {
		t.Fatalf("the ask by ID reused the plain ask %s, losing the ID", plain.ID)
	}
	// A retry of the same ask by ID still reuses its own open request, rather
	// than adding another line to the approval inbox.
	if again := askFor(ctx, t, svc, wellknown.GitHubAPI); again.ID != byID.ID {
		t.Fatalf("retry = %s, want the open ask by ID %s", again.ID, byID.ID)
	}
}

// Access to the discobox API is a gate: approving a request for it chooses no
// secret, the project's one gate secret stands behind every grant of it, and
// that secret is never handed out to anything.
func TestTheDiscoboxAPIIsApprovedWithNoSecret(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	github := createBearerSecret(ctx, t, svc)

	req := askFor(ctx, t, svc, wellknown.DiscoboxSandbox)
	if req.Host != "api.discobox.internal" || req.EnvName != "DISCOBOX_TOKEN" {
		t.Fatalf("request = %+v, want the discobox API's host and variable", req)
	}
	_, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(github.ID)})
	requireStatus(t, err, http.StatusBadRequest)

	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{})
	if err != nil {
		t.Fatalf("approve without a secret: %v", err)
	}
	gate, err := st.GetSecret(ctx, "project-1", approved.SecretID)
	if err != nil || gate.WellKnownID != wellknown.DiscoboxSandbox || gate.Host != "api.discobox.internal" {
		t.Fatalf("answered with %+v, %v; want the project's gate secret", gate, err)
	}

	// The gate stands behind every later grant of it too.
	again := askFor(ctx, t, svc, wellknown.DiscoboxSandbox)
	if again.ID == req.ID {
		t.Fatal("a second ask reused the approved one")
	}
	second, err := svc.ApproveSecretRequest(ctx, "project-1", again.ID, services.ApproveSecretRequestBody{})
	if err != nil || second.SecretID != gate.ID {
		t.Fatalf("second approval = %+v, %v; want the same gate secret", second, err)
	}

	// And it resolves to nothing, whatever asks.
	bindings, err := st.ListSandboxSecrets(ctx, "project-1", testSandboxID)
	if err != nil || len(bindings) != 1 {
		t.Fatalf("bindings = %#v, %v; want the one binding for DISCOBOX_TOKEN", bindings, err)
	}
	resolution, err := svc.ResolveSandboxSecret(ctx, testPoolID, testSandboxID, bindings[0].Sentinel, "api.discobox.internal")
	if err != nil || resolution.Status != model.SecretRequestStatusDenied || resolution.Value != nil {
		t.Fatalf("resolution = %+v, %v; want a gate's secret never handed out", resolution, err)
	}
}

// The discobox API's host is reached only by asking for it by ID: a secret
// approved for that host under another name would open the API unannounced.
func TestTheDiscoboxAPIsHostIsAskedForOnlyByID(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	for _, host := range []string{"api.discobox.internal", "x.api.discobox.internal"} {
		_, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
			SandboxId: testSandboxID,
			Name:      "discobox",
			EnvVar:    "DISCOBOX_TOKEN",
			Host:      host,
			Uses:      []apimodel.SecretUse{{Description: "create a discobox"}},
		})
		requireStatus(t, err, http.StatusBadRequest)
	}
}

// The gate's secret stands for access, not a credential: nothing is behind its
// value, and its host is where the pool admits the discobox API. Changing
// either is refused, with the way to take access back instead.
func TestTheGateSecretIsNotEdited(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	req := askFor(ctx, t, svc, wellknown.DiscoboxSandbox)
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	for name, body := range map[string]services.UpdateSecretBody{
		"its value": {Value: serverapi.NewOptSecretValue(serverapi.SecretValue{Token: serverapi.NewOptString("a-real-token")})},
		"its host":  {Host: serverapi.NewOptString("evil.example")},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.UpdateSecret(ctx, "project-1", approved.SecretID, body)
			requireStatus(t, err, http.StatusBadRequest)
		})
	}
	gate, err := st.GetSecret(ctx, "project-1", approved.SecretID)
	if err != nil || gate.Host != "api.discobox.internal" {
		t.Fatalf("gate = %+v, %v; want it unchanged", gate, err)
	}
}

// The discobox API is granted only by a person. A discobox holding it may
// answer the inbox, but not a request for the API itself — or it could give
// every credential onward with no person seeing it (ADR 0140 §4).
func TestOnlyAPersonApprovesTheDiscoboxAPI(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	lead := auth.WithPrincipal(ctx, auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: "project-1", UserID: "user-1",
	})

	req := askFor(ctx, t, svc, wellknown.DiscoboxSandbox)
	_, err := svc.ApproveSecretRequest(lead, "project-1", req.ID, services.ApproveSecretRequestBody{})
	requireStatus(t, err, http.StatusForbidden)

	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{}); err != nil {
		t.Fatalf("a person approving it: %v", err)
	}
}
