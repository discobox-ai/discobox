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
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/service"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// githubBinding binds the secret on a new discobox for use in GH_TOKEN.
func githubBinding(secret string) serverapi.SandboxBindingInput {
	return serverapi.SandboxBindingInput{
		Secret: secret,
		EnvVar: serverapi.NewOptString("GH_TOKEN"),
		Routes: []serverapi.SecretRouteInput{{Host: "github.com"}},
	}
}

func createGitHubSecret(ctx context.Context, t *testing.T, svc interface {
	CreateSecret(context.Context, string, services.CreateSecretBody) (*model.Secret, error)
}, projectID string) *model.Secret {
	t.Helper()
	secret, err := svc.CreateSecret(ctx, projectID, services.CreateSecretBody{
		Name:   "github",
		Type:   serverapi.CreateSecretBodyTypeToken,
		Value:  serverapi.SecretValue{Token: serverapi.NewOptString("ghp_realrealrealrealrealrealrealreal12")},
		Routes: serverapi.NewOptNilSecretRouteInputArray([]serverapi.SecretRouteInput{{Host: "*"}}),
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	return secret
}

// A discobox can be created already holding project secrets: bindings on it,
// stored with it.
func TestADiscoboxIsCreatedWithTheSecretsItIsGiven(t *testing.T) {
	ctx := context.Background()
	svc, _, _, projectID := newSandboxTestService(t, nil)
	secret := createGitHubSecret(ctx, t, svc, projectID)

	created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.SandboxCreateConfig{Name: "worker"},
		Bindings:    []serverapi.SandboxBindingInput{githubBinding(secret.ID)},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	credentials, err := svc.ListSandboxCredentials(ctx, created.PoolID, created.ID)
	if err != nil {
		t.Fatalf("list credentials: %v", err)
	}
	if len(credentials) != 1 {
		t.Fatalf("credentials = %#v, want the one it was given", credentials)
	}
	got := credentials[0]
	if got.EnvName != "GH_TOKEN" || !slices.Equal(got.Hosts, []string{"github.com"}) || got.Purpose != model.SecretPurposeUse {
		t.Fatalf("credential = %+v, want github.com in GH_TOKEN", got)
	}
}

// ensureLead creates the lead discobox, sbx-lead, the first time it is asked
// for: a binding names a discobox that exists, and a create from the sandbox role
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
			ID: "sbx-lead", ProjectID: projectID, PoolID: project.DefaultPoolID, CreatedByUserID: "user-lead", Name: "lead", DiscoboxAPI: true,
		}); err != nil {
			t.Fatalf("create lead sandbox: %v", err)
		}
	} else if err != nil {
		t.Fatalf("get lead sandbox: %v", err)
	}
}

// delegateToLead gives the lead a delegate binding of secret, lapsing after
// lifetime (none when zero). The lead is created the first time: a binding
// names a discobox that exists.
func delegateToLead(t *testing.T, st *store.Store, projectID string, secret *model.Secret, lifetime time.Duration) *model.SecretBinding {
	t.Helper()
	ctx := context.Background()
	ensureLead(t, st, projectID)
	binding := &model.SecretBinding{
		ProjectID: projectID, SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: "sbx-lead",
		Delegate: true, Routes: []model.SecretRoute{{Host: "github.com"}}, CreatedBy: "user-1",
	}
	if lifetime > 0 {
		expires := time.Now().UTC().Add(lifetime)
		binding.ExpiresAt = &expires
	}
	if err := st.CreateSecretBinding(ctx, binding, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatalf("create delegate binding: %v", err)
	}
	return binding
}

// What a sandbox creates it creates as the user who created it, and the
// bindings it gives are recorded as its own doing, under the delegate binding
// it holds. It gives bindings, never values (ADR 26-10-27-880 §§3, 5).
func TestADiscoboxCreatedByASandboxIsItsUsersAndItsBindingsAreTheSandboxs(t *testing.T) {
	ctx := context.Background()
	svc, _, st, projectID := newSandboxTestService(t, nil)
	secret := createGitHubSecret(ctx, t, svc, projectID)
	delegation := delegateToLead(t, st, projectID, secret, time.Hour)
	lead := auth.WithPrincipal(ctx, auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: projectID, UserID: "user-lead",
	})

	created, err := svc.CreateSandbox(lead, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.SandboxCreateConfig{Name: "worker"},
		Bindings:    []serverapi.SandboxBindingInput{githubBinding(secret.ID)},
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
	given, err := svc.ListSecretBindings(ctx, projectID, services.SecretBindingQuery{TargetKind: model.SecretBindingTargetSandbox, Target: created.ID})
	if err != nil || len(given) != 1 || given[0].CreatedBy != "sbx-lead" || !given[0].Use {
		t.Fatalf("bindings = %+v, %v; want one for the worker, made by the lead", given, err)
	}
	if given[0].ExpiresAt == nil || given[0].ExpiresAt.After(*delegation.ExpiresAt) {
		t.Fatalf("binding expires %v, want no later than the delegation it was made under", given[0].ExpiresAt)
	}
	if given[0].ParentID != delegation.ID {
		t.Fatalf("binding made under %q, want the lead's delegation %s", given[0].ParentID, delegation.ID)
	}

	_, err = svc.CreateSandbox(lead, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config: serverapi.SandboxCreateConfig{Name: "leaky", Secrets: []serverapi.SandboxSecretInput{{
			Env: "GH_TOKEN", Value: "ghp_inline",
		}}},
	})
	requireStatus(t, err, http.StatusForbidden)
}

// A create whose bindings cannot all be made creates nothing: a discobox that
// started without a use it was meant to have would be doing different work.
func TestACreateThatCannotMakeItsBindingsCreatesNothing(t *testing.T) {
	ctx := context.Background()
	svc, _, _, projectID := newSandboxTestService(t, nil)
	secret := createGitHubSecret(ctx, t, svc, projectID)

	noVariable := githubBinding(secret.ID)
	noVariable.EnvVar = serverapi.OptString{}
	badRoute := githubBinding(secret.ID)
	badRoute.Routes = []serverapi.SecretRouteInput{{Host: " "}}
	other := createSecretNamed(ctx, t, svc, projectID, "gitlab")
	twoForOneVariable := githubBinding(other.ID)

	for name, tc := range map[string]struct {
		bindings []serverapi.SandboxBindingInput
		want     int
	}{
		"a binding without a variable":    {[]serverapi.SandboxBindingInput{noVariable}, http.StatusBadRequest},
		"a route with no host":            {[]serverapi.SandboxBindingInput{badRoute}, http.StatusBadRequest},
		"a secret that does not exist":    {[]serverapi.SandboxBindingInput{githubBinding("sec_nobody")}, http.StatusNotFound},
		"two credentials in one variable": {[]serverapi.SandboxBindingInput{githubBinding(secret.ID), twoForOneVariable}, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
				HarnessName: serverapi.NewOptString("shell"),
				Config:      serverapi.SandboxCreateConfig{Name: "worker"},
				Bindings:    tc.bindings,
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

// A discobox gives on a create only what it was delegated; a binding it may
// not make refuses the whole create, which then creates nothing (ADR 26-10-27-880
// §5). A person's create is not held to any of it.
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
			Bindings:    []serverapi.SandboxBindingInput{githubBinding(secret.ID)},
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

	requireStatus(t, create(lead, "undelegated"), http.StatusForbidden)
	if created("undelegated") {
		t.Fatal("a create refused for a binding it could not make created a discobox")
	}
	delegateToLead(t, st, projectID, secret, time.Hour)
	if err := create(lead, "delegated"); err != nil {
		t.Fatalf("a create under a delegation: %v", err)
	}
	if err := create(ctx, "a-persons"); err != nil {
		t.Fatalf("a person's create with a binding: %v", err)
	}
}

// Whether a discobox may call the discobox API is set at create, and a
// discobox creating one passes it on only when it has it itself; a person
// changes it later, and a discobox cannot (ADR 26-10-27-880 §9).
func TestTheDiscoboxAPIIsGivenAtCreateAndChangedByAPerson(t *testing.T) {
	ctx := context.Background()
	svc, _, st, projectID := newSandboxTestService(t, nil)
	ensureLead(t, st, projectID)
	create := func(as context.Context, name string, api bool) (*model.Sandbox, error) {
		return svc.CreateSandbox(as, projectID, services.CreateSandboxBody{
			HarnessName: serverapi.NewOptString("shell"),
			Config:      serverapi.SandboxCreateConfig{Name: name},
			DiscoboxApi: serverapi.NewOptBool(api),
		})
	}
	lead := auth.WithPrincipal(ctx, auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: projectID, UserID: "user-lead",
	})

	worker, err := create(lead, "worker", true)
	if err != nil || !worker.DiscoboxAPI {
		t.Fatalf("worker = %+v, %v; want it given the API by a lead that has it", worker, err)
	}
	plain, err := create(ctx, "plain", false)
	if err != nil || plain.DiscoboxAPI {
		t.Fatalf("plain = %+v, %v; want no API", plain, err)
	}
	plainBox := auth.WithPrincipal(ctx, auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: plain.ID, ProjectID: projectID, UserID: service.DefaultUserID,
	})
	_, err = create(plainBox, "passed-on", true)
	requireStatus(t, err, http.StatusForbidden)

	updated, err := svc.UpdateSandbox(ctx, projectID, plain.ID, services.UpdateSandboxBody{DiscoboxApi: serverapi.NewOptBool(true)})
	if err != nil || !updated.DiscoboxAPI {
		t.Fatalf("updated = %+v, %v; want a person's change to stand", updated, err)
	}
	stored, err := st.GetSandbox(ctx, projectID, plain.ID)
	if err != nil || !stored.DiscoboxAPI {
		t.Fatalf("stored = %+v, %v; want the flag written", stored, err)
	}
	// A whole-row save of a copy read before the change does not put it back.
	stored.DiscoboxAPI = false
	if err := st.UpdateSandbox(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.GetSandbox(ctx, projectID, plain.ID); again == nil || !again.DiscoboxAPI {
		t.Fatal("a whole-row save wrote the flag back")
	}
	_, err = svc.UpdateSandbox(lead, projectID, worker.ID, services.UpdateSandboxBody{DiscoboxApi: serverapi.NewOptBool(false)})
	requireStatus(t, err, http.StatusForbidden)
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
