package projects_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	apigen "github.com/obot-platform/discobox/api/gen"
	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/server/internal/auth"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/resources/projects"
	"github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

const testUserID = "user-1"

// stubProviders, stubPools, and stubHarnesses stand in for the owning services
// project copying delegates to. They record what a copy asked for without
// dragging provider validation or a reconcile engine into these tests.
type stubProviders struct{ st *store.Store }

func (s stubProviders) CreateSandboxProviderInstance(ctx context.Context, projectID string, input services.CreateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error) {
	provider := &model.SandboxProviderInstance{
		ProjectID: projectID,
		Type:      input.Type,
		Name:      input.Name,
		Config:    json.RawMessage(input.Config),
	}
	if err := s.st.CreateSandboxProviderInstance(ctx, provider); err != nil {
		return nil, err
	}
	return provider, nil
}

type stubPools struct{ st *store.Store }

func (s stubPools) CreatePool(ctx context.Context, projectID string, input services.CreatePoolBody) (*model.Pool, error) {
	pool := &model.Pool{
		ProjectID: projectID,
		PoolManifest: model.PoolManifest{
			Name:               input.Name,
			ProviderInstanceID: input.ProviderInstanceId,
			CPUVCPUs:           input.CpuVcpus.Or(0),
			MemoryBytes:        input.MemoryBytes.Or(0),
			StorageBytes:       input.StorageBytes.Or(0),
		},
	}
	if err := s.st.CreatePool(ctx, pool); err != nil {
		return nil, err
	}
	return pool, nil
}

// stubHarnesses seeds one built-in, the way the real service seeds the
// included harnesses into every new project against current images.
type stubHarnesses struct{ st *store.Store }

func (s stubHarnesses) SeedBuiltIns(ctx context.Context, projectID string) error {
	return s.st.CreateHarnessConfig(ctx, &model.HarnessConfig{
		ProjectID: projectID, Slug: "codex", Name: "Codex", BuiltIn: true, Image: "codex:current",
	})
}

func newService(t *testing.T) (*projects.Service, *store.Store, context.Context) {
	t.Helper()
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypeUser, UserID: testUserID})
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	st := store.New(db.Write, db.Read)
	svc := projects.NewService(st, stubProviders{st}, stubPools{st}, stubHarnesses{st})
	return svc, st, ctx
}

// seedSourceProject builds a project that looks like one a user has been
// working in: a provider instance, a default pool on it, and a configured
// built-in harness holding a secret, a binding, and the grant its configure
// flow created.
func seedSourceProject(ctx context.Context, t *testing.T, st *store.Store) *model.Project {
	t.Helper()
	project := &model.Project{ID: id.NewString(id.PrefixProject), OwnerUserID: testUserID, Name: "Source", Default: true}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatalf("create source project: %v", err)
	}
	if _, err := st.CreateProjectMemberIfNotExists(ctx, &model.ProjectMember{ProjectID: project.ID, UserID: testUserID, Role: "owner"}); err != nil {
		t.Fatalf("create membership: %v", err)
	}
	provider := &model.SandboxProviderInstance{ProjectID: project.ID, Type: "docker", Name: "Docker", Config: json.RawMessage(`{"agentPort":1234}`)}
	if err := st.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ProjectID: project.ID, PoolManifest: model.PoolManifest{Name: "Default", ProviderInstanceID: provider.ID, CPUVCPUs: 4}}
	if err := st.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	config := &model.HarnessConfig{ProjectID: project.ID, Slug: "codex", Name: "Codex", BuiltIn: true, Configured: true, Image: "codex:stale"}
	config.ConfiguredFiles = []model.HarnessConfigFile{{Path: "/home/user/.codex/config", Content: "hello"}}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	secretID := id.NewString(id.PrefixSecret)
	secret := &model.Secret{ID: secretID, ProjectID: project.ID, Name: "CODEX_TOKEN", Type: "bearer", UniqueKey: secretID, EncryptedValue: []byte(`{"token":"t0ken"}`)}
	if err := st.CreateSecret(ctx, secret); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if err := st.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
		ProjectID: project.ID, HarnessConfigID: config.ID, EnvName: "CODEX_TOKEN", SecretID: secret.ID,
	}); err != nil {
		t.Fatalf("bind secret: %v", err)
	}
	if err := st.CreateSecretGrant(ctx, &model.SecretGrant{
		ProjectID: project.ID, SecretID: secret.ID, Scope: model.SecretGrantScopeHarnessConfig, ScopeKey: config.ID,
	}); err != nil {
		t.Fatalf("grant secret: %v", err)
	}
	config.ConfiguredSecretIDs = []string{secret.ID}
	if err := st.UpdateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("update harness config: %v", err)
	}
	project.DefaultPoolID = pool.ID
	project.DefaultHarnessConfigID = config.ID
	if err := st.UpsertProject(ctx, project); err != nil {
		t.Fatalf("update source project: %v", err)
	}
	return project
}

func statusOf(t *testing.T, err error, want int) {
	t.Helper()
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != want {
		t.Fatalf("err = %v, want status %d", err, want)
	}
}

func TestCreateProjectSeedsBuiltIns(t *testing.T) {
	svc, st, ctx := newService(t)
	project, err := svc.CreateProject(ctx, services.CreateProjectBody{Name: "My Next Thing!"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if project.Default {
		t.Fatal("a new project must not steal the default flag")
	}
	member, err := st.IsProjectMember(ctx, project.ID, testUserID)
	if err != nil || !member {
		t.Fatalf("IsProjectMember = %v, %v; want true", member, err)
	}
	configs, err := st.ListHarnessConfigs(ctx, project.ID)
	if err != nil {
		t.Fatalf("list harness configs: %v", err)
	}
	if len(configs) != 1 || configs[0].Slug != "codex" || configs[0].Configured {
		t.Fatalf("harness configs = %#v; want one unconfigured built-in", configs)
	}
}

// Name is the only handle a project has besides its ID, and the CLI resolves
// it, so a duplicate would make every name-based selection ambiguous.
func TestCreateProjectRejectsDuplicateName(t *testing.T) {
	svc, _, ctx := newService(t)
	if _, err := svc.CreateProject(ctx, services.CreateProjectBody{Name: "Taken"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	_, err := svc.CreateProject(ctx, services.CreateProjectBody{Name: "Taken"})
	statusOf(t, err, http.StatusConflict)
}

// A copy has to carry the whole chain a configured harness depends on:
// without the secret, the binding, and the grant, the copied harness is bound
// to a credential in another project that it cannot resolve.
func TestCreateProjectCopiesProvidersPoolsAndConfiguredHarnesses(t *testing.T) {
	svc, st, ctx := newService(t)
	source := seedSourceProject(ctx, t, st)

	project, err := svc.CreateProject(ctx, services.CreateProjectBody{
		Name:              "Copy",
		CopyFromProjectId: apigen.NewOptString(source.ID),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	providers, err := st.ListSandboxProviderInstances(ctx, project.ID)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(providers) != 1 || providers[0].Type != "docker" || string(providers[0].Config) != `{"agentPort":1234}` {
		t.Fatalf("providers = %#v; want one copied docker provider", providers)
	}
	pools, err := st.ListPools(ctx, project.ID)
	if err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if len(pools) != 1 || pools[0].ProviderInstanceID != providers[0].ID || pools[0].CPUVCPUs != 4 {
		t.Fatalf("pools = %#v; want one pool rebound to the copied provider", pools)
	}
	if project.DefaultPoolID != pools[0].ID {
		t.Fatalf("default pool = %q, want %q", project.DefaultPoolID, pools[0].ID)
	}

	configs, err := st.ListHarnessConfigs(ctx, project.ID)
	if err != nil {
		t.Fatalf("list harness configs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("harness configs = %#v; want one", configs)
	}
	config := configs[0]
	if !config.Configured || len(config.ConfiguredFiles) != 1 {
		t.Fatalf("copied harness config = %#v; want the configured state carried across", config)
	}
	// The new project was seeded against the current image, and copying the
	// source's configured state must not drag its stale image back in.
	if config.Image != "codex:current" {
		t.Fatalf("image = %q, want the freshly seeded codex:current", config.Image)
	}
	if project.DefaultHarnessConfigID != config.ID {
		t.Fatalf("default harness config = %q, want %q", project.DefaultHarnessConfigID, config.ID)
	}

	secrets, err := st.ListSecrets(ctx, project.ID)
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	if len(secrets) != 1 {
		t.Fatalf("secrets = %#v; want one copied secret", secrets)
	}
	value, err := st.OpenSecretValue(ctx, &secrets[0])
	if err != nil {
		t.Fatalf("open copied secret: %v", err)
	}
	if value == nil || value.Token != "t0ken" {
		t.Fatalf("copied secret value = %#v, want the source's token", value)
	}
	if len(config.ConfiguredSecretIDs) != 1 || config.ConfiguredSecretIDs[0] != secrets[0].ID {
		t.Fatalf("configured secret IDs = %#v, want the copy's ID", config.ConfiguredSecretIDs)
	}
	bindings, err := st.ListHarnessConfigSecretBindings(ctx, project.ID, config.ID)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 1 || bindings[0].SecretID != secrets[0].ID {
		t.Fatalf("bindings = %#v; want one pointing at the copied secret", bindings)
	}
	grants, err := st.ListSecretGrants(ctx, project.ID, secrets[0].ID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 1 || grants[0].ScopeKey != config.ID {
		t.Fatalf("grants = %#v; want one scoped to the copied harness config", grants)
	}
}

func TestCreateProjectCopySelectionExcludesPoolsAndHarnesses(t *testing.T) {
	svc, st, ctx := newService(t)
	source := seedSourceProject(ctx, t, st)

	project, err := svc.CreateProject(ctx, services.CreateProjectBody{
		Name:              "Providers Only",
		CopyFromProjectId: apigen.NewOptString(source.ID),
		Copy:              apigen.NewOptNilCreateProjectBodyCopyItemArray([]apigen.CreateProjectBodyCopyItem{apigen.CreateProjectBodyCopyItemProviders}),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	providers, err := st.ListSandboxProviderInstances(ctx, project.ID)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("providers = %#v; want the one copied provider", providers)
	}
	pools, err := st.ListPools(ctx, project.ID)
	if err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if len(pools) != 0 {
		t.Fatalf("pools = %#v; want none", pools)
	}
	configs, err := st.ListHarnessConfigs(ctx, project.ID)
	if err != nil {
		t.Fatalf("list harness configs: %v", err)
	}
	if len(configs) != 1 || configs[0].Configured {
		t.Fatalf("harness configs = %#v; want the seeded built-in, unconfigured", configs)
	}
	if secrets, err := st.ListSecrets(ctx, project.ID); err != nil || len(secrets) != 0 {
		t.Fatalf("secrets = %#v, %v; want none", secrets, err)
	}
}

// Copying pools without providers would leave every copied pool bound to a
// provider instance in someone else's project, so pools pull providers along.
func TestCreateProjectCopyPoolsImpliesProviders(t *testing.T) {
	svc, st, ctx := newService(t)
	source := seedSourceProject(ctx, t, st)

	project, err := svc.CreateProject(ctx, services.CreateProjectBody{
		Name:              "Pools Only",
		CopyFromProjectId: apigen.NewOptString(source.ID),
		Copy:              apigen.NewOptNilCreateProjectBodyCopyItemArray([]apigen.CreateProjectBodyCopyItem{apigen.CreateProjectBodyCopyItemPools}),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	providers, err := st.ListSandboxProviderInstances(ctx, project.ID)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	pools, err := st.ListPools(ctx, project.ID)
	if err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if len(providers) != 1 || len(pools) != 1 || pools[0].ProviderInstanceID != providers[0].ID {
		t.Fatalf("providers = %#v, pools = %#v; want the pool bound to a copied provider", providers, pools)
	}
}

func TestCreateProjectRejectsUnauthorizedSource(t *testing.T) {
	svc, st, ctx := newService(t)
	other := &model.Project{ID: id.NewString(id.PrefixProject), OwnerUserID: "someone-else", Name: "Other"}
	if err := st.CreateProject(ctx, other); err != nil {
		t.Fatalf("create other project: %v", err)
	}
	_, err := svc.CreateProject(ctx, services.CreateProjectBody{
		Name:              "Sneaky",
		CopyFromProjectId: apigen.NewOptString(other.ID),
	})
	statusOf(t, err, http.StatusForbidden)
	// The failed create must not leave a project behind.
	if _, err := st.GetProjectByOwnerAndName(ctx, testUserID, "Sneaky"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetProjectByOwnerAndName = %v, want not found", err)
	}
}

func TestSetDefaultProjectMovesTheFlag(t *testing.T) {
	svc, st, ctx := newService(t)
	source := seedSourceProject(ctx, t, st)
	project, err := svc.CreateProject(ctx, services.CreateProjectBody{Name: "Next"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	updated, err := svc.SetDefaultProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("set default project: %v", err)
	}
	if !updated.Default {
		t.Fatal("new default project does not carry the flag")
	}
	previous, err := st.GetProject(ctx, source.ID)
	if err != nil {
		t.Fatalf("get previous default: %v", err)
	}
	if previous.Default {
		t.Fatal("previous default project still carries the flag")
	}
	resolved, err := st.GetDefaultProjectForUser(ctx, testUserID)
	if err != nil {
		t.Fatalf("resolve default project: %v", err)
	}
	if resolved.ID != project.ID {
		t.Fatalf("default project = %q, want %q", resolved.ID, project.ID)
	}
}

func TestDeleteProjectRefusesDefaultAndNonEmptyProjects(t *testing.T) {
	svc, st, ctx := newService(t)
	source := seedSourceProject(ctx, t, st)

	// Default first: it is refused before anything else is even counted.
	statusOf(t, svc.DeleteProject(ctx, source.ID), http.StatusConflict)

	project, err := svc.CreateProject(ctx, services.CreateProjectBody{
		Name:              "Busy",
		CopyFromProjectId: apigen.NewOptString(source.ID),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	statusOf(t, svc.DeleteProject(ctx, project.ID), http.StatusConflict)

	sandbox := &model.Sandbox{ProjectID: project.ID, PoolID: project.DefaultPoolID}
	if err := st.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	statusOf(t, svc.DeleteProject(ctx, project.ID), http.StatusConflict)
}

func TestDeleteProjectRemovesItsConfiguration(t *testing.T) {
	svc, st, ctx := newService(t)
	source := seedSourceProject(ctx, t, st)
	project, err := svc.CreateProject(ctx, services.CreateProjectBody{
		Name:              "Temporary",
		CopyFromProjectId: apigen.NewOptString(source.ID),
		Copy: apigen.NewOptNilCreateProjectBodyCopyItemArray([]apigen.CreateProjectBodyCopyItem{
			apigen.CreateProjectBodyCopyItemProviders,
			apigen.CreateProjectBodyCopyItemHarnesses,
		}),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := svc.DeleteProject(ctx, project.ID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if _, err := st.GetProject(ctx, project.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetProject = %v, want not found", err)
	}
	if providers, err := st.ListSandboxProviderInstances(ctx, project.ID); err != nil || len(providers) != 0 {
		t.Fatalf("providers = %#v, %v; want none", providers, err)
	}
	if configs, err := st.ListHarnessConfigs(ctx, project.ID); err != nil || len(configs) != 0 {
		t.Fatalf("harness configs = %#v, %v; want none", configs, err)
	}
	if secrets, err := st.ListSecrets(ctx, project.ID); err != nil || len(secrets) != 0 {
		t.Fatalf("secrets = %#v, %v; want none", secrets, err)
	}
	// The source project is untouched by a delete of its copy.
	if secrets, err := st.ListSecrets(ctx, source.ID); err != nil || len(secrets) != 1 {
		t.Fatalf("source secrets = %#v, %v; want the original", secrets, err)
	}
}

func TestUpdateProjectRenames(t *testing.T) {
	svc, _, ctx := newService(t)
	project, err := svc.CreateProject(ctx, services.CreateProjectBody{Name: "Before"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	updated, err := svc.UpdateProject(ctx, project.ID, services.UpdateProjectBody{
		Name: apigen.NewOptString("After"),
	})
	if err != nil {
		t.Fatalf("update project: %v", err)
	}
	if updated.Name != "After" {
		t.Fatalf("updated name = %q, want After", updated.Name)
	}
	// Renaming onto a name the user already has is the same conflict create
	// reports, since both would break name-based selection.
	if _, err := svc.CreateProject(ctx, services.CreateProjectBody{Name: "Taken"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	_, err = svc.UpdateProject(ctx, project.ID, services.UpdateProjectBody{Name: apigen.NewOptString("Taken")})
	statusOf(t, err, http.StatusConflict)

	// A no-op rename to its own name is not a conflict with itself.
	if _, err := svc.UpdateProject(ctx, project.ID, services.UpdateProjectBody{Name: apigen.NewOptString("After")}); err != nil {
		t.Fatalf("rename to same name: %v", err)
	}
}
