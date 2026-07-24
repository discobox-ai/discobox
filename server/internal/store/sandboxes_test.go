package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/obot-platform/discobox/internal/originkey"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/secrets"
	"github.com/obot-platform/discobox/server/internal/store"
)

func TestGetSandboxWithGeneration(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	sandbox := &model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       "project-1",
		PoolID:          "pool-1",
		CreatedByUserID: "user-1",
		Name:            "alpha",
	}
	if err := s.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	got, err := s.GetSandbox(ctx, sandbox.ProjectID, sandbox.ID, store.WithGeneration(sandbox.Generation))
	if err != nil {
		t.Fatalf("get matching generation: %v", err)
	}
	if got.ID != sandbox.ID {
		t.Fatalf("sandbox id = %q, want %q", got.ID, sandbox.ID)
	}

	if _, err := s.GetSandbox(ctx, sandbox.ProjectID, sandbox.ID, store.WithGeneration(sandbox.Generation+1)); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("get stale generation error = %v, want ErrGenerationConflict", err)
	}

	sandbox.Name = "renamed"
	if err := s.UpdateSandbox(ctx, sandbox, store.WithGeneration(sandbox.Generation)); err != nil {
		t.Fatalf("update matching generation: %v", err)
	}

	sandbox.Name = "stale"
	if err := s.UpdateSandbox(ctx, sandbox, store.WithGeneration(sandbox.Generation+1)); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("update stale generation error = %v, want ErrGenerationConflict", err)
	}

	if err := s.DeleteSandbox(ctx, sandbox.ProjectID, sandbox.ID, store.WithGeneration(sandbox.Generation+1)); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("delete stale generation error = %v, want ErrGenerationConflict", err)
	}
	if _, err := s.GetSandbox(ctx, sandbox.ProjectID, sandbox.ID); err != nil {
		t.Fatalf("get sandbox after stale delete: %v", err)
	}
}

func TestListSandboxesFiltersBySourceRoot(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	newSandbox := func(id, sourceRoot string) *model.Sandbox {
		sandbox := &model.Sandbox{
			ID:              id,
			ProjectID:       "project-1",
			PoolID:          "pool-1",
			CreatedByUserID: "user-1",
			Name:            id,
		}
		if sourceRoot != "" {
			sandbox.Source = &model.GitSource{Kind: "git", LocalDirectory: &sourceRoot}
			sandbox.SourceRoot = &sourceRoot
		}
		if err := s.CreateSandbox(ctx, sandbox); err != nil {
			t.Fatalf("create sandbox %s: %v", id, err)
		}
		return sandbox
	}
	newSandbox("sandbox-1", "/src/alpha")
	newSandbox("sandbox-2", "/src/beta")
	newSandbox("sandbox-3", "")

	matching, err := s.ListSandboxes(ctx, "project-1", "/src/alpha", "")
	if err != nil {
		t.Fatalf("list sandboxes by source root: %v", err)
	}
	if len(matching) != 1 || matching[0].ID != "sandbox-1" {
		t.Fatalf("sandboxes for /src/alpha = %v, want only sandbox-1", sandboxIDs(matching))
	}

	all, err := s.ListSandboxes(ctx, "project-1", "", "")
	if err != nil {
		t.Fatalf("list all sandboxes: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered sandboxes = %v, want all three", sandboxIDs(all))
	}
}

// Origin and source root are independent identities: the same project
// directory on two machines is two origins, and one machine can start
// sandboxes against repositories it does not hold.
func TestListSandboxesFiltersByOriginKey(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	newSandbox := func(id, hostID, projectPath string) {
		sandbox := &model.Sandbox{
			ID:              id,
			ProjectID:       "project-1",
			PoolID:          "pool-1",
			CreatedByUserID: "user-1",
			Name:            id,
		}
		if hostID != "" {
			sandbox.Origin = &model.Origin{HostID: hostID, ProjectPath: projectPath}
			key := sandbox.Origin.Key()
			sandbox.OriginKey = &key
		}
		if err := s.CreateSandbox(ctx, sandbox); err != nil {
			t.Fatalf("create sandbox %s: %v", id, err)
		}
	}
	newSandbox("sandbox-1", "host_aaaaaaaaaaaaaaaa", "/src/alpha")
	newSandbox("sandbox-2", "host_aaaaaaaaaaaaaaaa", "/src/beta")
	// Same project path as sandbox-1, different machine.
	newSandbox("sandbox-3", "host_bbbbbbbbbbbbbbbb", "/src/alpha")
	newSandbox("sandbox-4", "", "")

	key := originkey.Of("host_aaaaaaaaaaaaaaaa", "/src/alpha")
	matching, err := s.ListSandboxes(ctx, "project-1", "", key)
	if err != nil {
		t.Fatalf("list sandboxes by origin key: %v", err)
	}
	if len(matching) != 1 || matching[0].ID != "sandbox-1" {
		t.Fatalf("sandboxes for host_a /src/alpha = %v, want only sandbox-1", sandboxIDs(matching))
	}

	all, err := s.ListSandboxes(ctx, "project-1", "", "")
	if err != nil {
		t.Fatalf("list all sandboxes: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("unfiltered sandboxes = %v, want all four", sandboxIDs(all))
	}
}

// Origin survives the JSON round trip through the serializer column, so a
// listing can show where a sandbox came from.
func TestCreateSandboxPersistsOrigin(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	want := &model.Origin{
		HostID:      "host_aaaaaaaaaaaaaaaa",
		Hostname:    "laptop",
		ProjectPath: "/src/alpha",
		User:        "darren",
	}
	key := want.Key()
	if err := s.CreateSandbox(ctx, &model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       "project-1",
		PoolID:          "pool-1",
		CreatedByUserID: "user-1",
		Name:            "sandbox-1",
		Origin:          want,
		OriginKey:       &key,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	got, err := s.GetSandbox(ctx, "project-1", "sandbox-1")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if got.Origin == nil || *got.Origin != *want {
		t.Fatalf("origin = %+v, want %+v", got.Origin, want)
	}
}

func sandboxIDs(sandboxes []model.Sandbox) []string {
	ids := make([]string, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		ids = append(ids, sandbox.ID)
	}
	return ids
}

func TestGetResourcesByShortIDPrefix(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	project := &model.Project{ID: "proj_abc12345000000p1", OwnerUserID: "user-1", Name: "Project"}
	if err := s.UpsertProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.CreateProjectMemberIfNotExists(ctx, &model.ProjectMember{ProjectID: project.ID, UserID: "user-1", Role: "owner"}); err != nil {
		t.Fatalf("create project member: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "prov_abc12345000000p2", ProjectID: project.ID, Type: "docker", Name: "provider"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	sandbox := &model.Sandbox{ID: "sbx_abc12345000000p3", ProjectID: project.ID, PoolID: "pool-1", CreatedByUserID: "user-1", Name: "sandbox"}
	if err := s.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	gotProject, err := s.GetProject(ctx, "proj_abc12345")
	if err != nil || gotProject.ID != project.ID {
		t.Fatalf("short project = %#v err=%v", gotProject, err)
	}
	gotProvider, err := s.GetSandboxProviderInstance(ctx, project.ID, "prov_abc12345")
	if err != nil || gotProvider.ID != provider.ID {
		t.Fatalf("short provider = %#v err=%v", gotProvider, err)
	}
	gotSandbox, err := s.GetSandbox(ctx, project.ID, "sbx_abc12345")
	if err != nil || gotSandbox.ID != sandbox.ID {
		t.Fatalf("short sandbox = %#v err=%v", gotSandbox, err)
	}
	gotPool, err := s.GetPoolByID(ctx, "pool-1")
	if err != nil || gotPool.ID != "pool-1" {
		t.Fatalf("pool by id = %#v err=%v", gotPool, err)
	}
	ambiguous := &model.Sandbox{ID: "sbx_abc12345000000p5", ProjectID: project.ID, PoolID: "pool-1", CreatedByUserID: "user-1", Name: "ambiguous"}
	if err := s.CreateSandbox(ctx, ambiguous); err != nil {
		t.Fatalf("create ambiguous sandbox: %v", err)
	}
	if _, err := s.GetSandbox(ctx, project.ID, "sbx_abc12345"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ambiguous short sandbox error = %v, want not found", err)
	}
}

func TestSandboxSecretStateEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	key, err := secrets.GenerateBase64Key()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealerFromBase64Key(key)
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}
	s, db := newTestStoreWithDB(t, sealer)
	createTestPool(t, s, "project-1", "pool-1")
	plaintext := []byte("provider secret state")
	sandbox := &model.Sandbox{
		ID:              "sandbox-secret",
		ProjectID:       "project-1",
		PoolID:          "pool-1",
		CreatedByUserID: "user-1",
		Name:            "secret",
		SecretState:     plaintext,
	}
	if err := s.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	var row struct {
		SecretState []byte
	}
	if err := db.Write.WithContext(ctx).
		Model(&model.Sandbox{}).
		Select("secret_state").
		Where("id = ?", sandbox.ID).
		Scan(&row).Error; err != nil {
		t.Fatalf("read raw secret state: %v", err)
	}
	if bytes.Equal(row.SecretState, plaintext) {
		t.Fatalf("raw secret state equals plaintext")
	}

	got, err := s.GetSandbox(ctx, sandbox.ProjectID, sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if bytes.Equal(got.SecretState, plaintext) {
		t.Fatalf("loaded secret state equals plaintext")
	}
	if !secrets.IsSealed(got.SecretState) {
		t.Fatalf("loaded secret state is not sealed")
	}
	opened, err := s.OpenSandboxSecretState(ctx, got)
	if err != nil {
		t.Fatalf("open sandbox secret state: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("opened secret state = %q, want %q", string(opened), string(plaintext))
	}

	sealed := append([]byte(nil), got.SecretState...)
	got.Name = "renamed secret"
	if err := s.UpdateSandbox(ctx, got); err != nil {
		t.Fatalf("update sandbox with sealed secret state: %v", err)
	}
	var updatedRow struct {
		SecretState []byte
	}
	if err := db.Write.WithContext(ctx).
		Model(&model.Sandbox{}).
		Select("secret_state").
		Where("id = ?", sandbox.ID).
		Scan(&updatedRow).Error; err != nil {
		t.Fatalf("read updated raw secret state: %v", err)
	}
	if !bytes.Equal(updatedRow.SecretState, sealed) {
		t.Fatalf("sealed secret state changed on metadata-only update")
	}
}

func TestUpdateSandboxAgentStatusRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	sandbox := &model.Sandbox{
		ID: "sandbox-status", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "status",
	}
	if err := s.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	observedAt := time.Now().UTC().Truncate(time.Second)
	payload := json.RawMessage(`{"sessions":[{"state":"running"}]}`)
	if err := s.UpdateSandboxAgentStatus(ctx, "project-1", sandbox.ID, payload, observedAt); err != nil {
		t.Fatalf("update agent status: %v", err)
	}

	got, err := s.GetSandbox(ctx, "project-1", sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if string(got.AgentStatus) != string(payload) {
		t.Fatalf("agentStatus = %s, want %s", got.AgentStatus, payload)
	}
	if got.AgentStatusObservedAt == nil || !got.AgentStatusObservedAt.Equal(observedAt) {
		t.Fatalf("agentStatusObservedAt = %v, want %v", got.AgentStatusObservedAt, observedAt)
	}
	// The generation-guarded desired-state contract is untouched by this write.
	if got.Generation != sandbox.Generation {
		t.Fatalf("generation = %d, want unchanged %d", got.Generation, sandbox.Generation)
	}
}

func TestUpdateSandboxAgentStatusUnknownSandboxReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	if err := s.UpdateSandboxAgentStatus(ctx, "project-1", "does-not-exist", json.RawMessage(`{}`), time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update agent status for unknown sandbox = %v, want ErrNotFound", err)
	}
}

// TestUpdateSandboxAgentStatusComposesWithConcurrentFieldUpdates confirms the
// narrow status write and a plain UpdateSandbox call (each starting from its
// own fresh read, as any correct concurrent caller must) do not clobber each
// other's columns — the risk a whole-row Save from a *stale* read would
// otherwise create.
func TestUpdateSandboxAgentStatusComposesWithConcurrentFieldUpdates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	sandbox := &model.Sandbox{
		ID: "sandbox-status", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "status",
	}
	if err := s.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	observedAt := time.Now().UTC().Truncate(time.Second)
	if err := s.UpdateSandboxAgentStatus(ctx, "project-1", sandbox.ID, json.RawMessage(`{"sessions":[]}`), observedAt); err != nil {
		t.Fatalf("update agent status: %v", err)
	}

	// A fresh read (as a correct reconciler would take) picks up the agent
	// status just written, so saving it back after an unrelated field change
	// must not lose it.
	fresh, err := s.GetSandbox(ctx, "project-1", sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	fresh.Name = "renamed"
	if err := s.UpdateSandbox(ctx, fresh); err != nil {
		t.Fatalf("update sandbox name: %v", err)
	}

	got, err := s.GetSandbox(ctx, "project-1", sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if got.Name != "renamed" {
		t.Fatalf("name = %q, want renamed", got.Name)
	}
	if got.AgentStatusObservedAt == nil || !got.AgentStatusObservedAt.Equal(observedAt) {
		t.Fatalf("agentStatusObservedAt = %v, want %v (must survive a concurrent field update from a fresh read)", got.AgentStatusObservedAt, observedAt)
	}
}

func newTestStore(t *testing.T) *store.Store {
	s, _ := newTestStoreWithDB(t, nil)
	return s
}

func newTestStoreWithDB(t *testing.T, sealer secrets.Sealer) (*store.Store, *database.DB) {
	t.Helper()

	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	project := &model.Project{
		ID:          "project-1",
		OwnerUserID: "user-1",
		Name:        "Project",
	}
	if err := db.Write.WithContext(ctx).Create(project).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}

	return store.New(db.Write, db.Read, store.WithSealer(sealer)), db
}
