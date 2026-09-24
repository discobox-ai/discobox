package service_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/resources/sandboxes"
	"github.com/discobox-ai/discobox/server/internal/service"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/wellknown"
)

// githubGrant is a grant of the secret for a new discobox, pushing a branch.
func githubGrant(secretID string) serverapi.SandboxGrant {
	return serverapi.SandboxGrant{
		SecretId: serverapi.NewOptString(secretID),
		EnvVar:   serverapi.NewOptString("GH_TOKEN"),
		Host:     serverapi.NewOptString("github.com"),
		Uses:     []serverapi.SecretUse{{Description: "push a branch to org/repo"}},
	}
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
	if got.Assignment.EnvName != "GH_TOKEN" || got.Grant.Host != "github.com" || len(got.Grant.Uses) != 1 ||
		got.Grant.Uses[0].Description != "push a branch to org/repo" || got.Grant.Purpose != model.SecretGrantPurposeUse {
		t.Fatalf("credential = %+v, want push to github.com in GH_TOKEN", got)
	}
}

// What a sandbox creates it creates as the user who created it, and the grants
// it gives are recorded as its own doing. It gives uses, never values.
func TestADiscoboxCreatedByASandboxIsItsUsersAndItsGrantsAreTheSandboxs(t *testing.T) {
	ctx := context.Background()
	svc, _, _, projectID := newSandboxTestService(t, nil)
	secret := createGitHubSecret(ctx, t, svc, projectID)
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
	grants, err := svc.ListSecretGrants(ctx, projectID, secret.ID)
	if err != nil || len(grants) != 1 || grants[0].GrantedBy != "sbx-lead" || grants[0].ScopeKey != created.ID {
		t.Fatalf("grants = %+v, %v; want one for the worker, granted by the lead", grants, err)
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
	noHost.Host = serverapi.OptString{}
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
		}
		if host != "" {
			grant.Host = serverapi.NewOptString(host)
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
	if got := credentials[0]; got.Assignment.EnvName != "GH_TOKEN" || got.Assignment.SecretID != secret.ID || got.Grant.Host != "api.github.com" {
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
	})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	approved, err := svc.ApproveSecretRequest(ctx, projectID, req.ID, services.ApproveSecretRequestBody{})
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
		}},
	})
	requireStatus(t, err, http.StatusForbidden)
}

// hostPathProvider reaches the server's whole filesystem, so a source whose
// origin is the server's own machine is cloned from its path there. root is
// that filesystem's root as the host spells it: "/" on POSIX, a volume such as
// "C:\\" on Windows, where "/" is not absolute and covers nothing.
type hostPathProvider struct {
	noopSandboxProvider
	root string
}

func (p hostPathProvider) Definition() sandboxes.ProviderDefinition {
	return sandboxes.ProviderDefinition{Name: "host-path", LocalSourceRoots: []string{p.root}}
}

// A sandbox can read the user's origin off any discobox's record. Claiming it
// must not get a source cloned from the server's filesystem: a create from a
// sandbox is always push-delivered (ADR 26-09-24-630 §3).
func TestASandboxsOriginDoesNotDecideDelivery(t *testing.T) {
	ctx := context.Background()
	svc, _, _, projectID := newSandboxTestService(t, nil)
	// A real host path, so it is absolute on the machine running the test.
	directory := t.TempDir()
	svc.RegisterSandboxProvider("test", hostPathProvider{root: filepath.VolumeName(directory) + string(filepath.Separator)})
	svc.SetHostID("host-user")
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
	if mine.Source.Delivery != model.GitSourceDeliveryClone {
		t.Fatalf("user's own source delivered by %q, want clone from the path", mine.Source.Delivery)
	}
	forged, err := svc.CreateSandbox(lead, projectID, body("from-a-sandbox"))
	if err != nil {
		t.Fatalf("create sandbox as a sandbox: %v", err)
	}
	if forged.Source.Delivery != model.GitSourceDeliveryPush {
		t.Fatalf("sandbox's source delivered by %q, want push", forged.Source.Delivery)
	}

	// Nor may it name a host path by URL, which the pool agent would clone
	// as itself — as its source, or as a source beside it.
	hostURL := func(raw string) serverapi.GitSource {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return serverapi.GitSource{Kind: serverapi.GitSourceKindGit, URL: serverapi.NewOptURI(*u)}
	}
	for name, raw := range map[string]string{"file": "file:///home/user/.password-store", "bare path": "/home/user/.password-store"} {
		source := body("url-" + strings.ReplaceAll(name, " ", "-"))
		source.Config.Source = serverapi.NewOptGitSource(hostURL(raw))
		_, err := svc.CreateSandbox(lead, projectID, source)
		requireStatus(t, err, http.StatusForbidden)

		beside := body("ref-" + strings.ReplaceAll(name, " ", "-"))
		beside.Config.Source = serverapi.OptGitSource{}
		beside.Config.SourceCodeReferences = serverapi.NewOptSandboxCreateConfigSourceCodeReferences(
			serverapi.SandboxCreateConfigSourceCodeReferences{"/workspace/secrets": hostURL(raw)})
		_, err = svc.CreateSandbox(lead, projectID, beside)
		requireStatus(t, err, http.StatusForbidden)
	}
	remote := body("remote")
	remote.Config.Source = serverapi.NewOptGitSource(hostURL("https://github.com/org/repo.git"))
	if _, err := svc.CreateSandbox(lead, projectID, remote); err != nil {
		t.Fatalf("create from a network URL: %v", err)
	}
}
