package sandboxes

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func newBindingFixture(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{
		ID: "project-1", OwnerUserID: "user-1", Name: "Project",
	}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	st := store.New(db.Write, db.Read)
	return &Service{store: st}, st
}

// codexConfig stores a harness config declaring OPENAI_API_KEY as required.
func codexConfig(t *testing.T, st *store.Store) *model.HarnessConfig {
	t.Helper()
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", RunCommand: []string{"codex"},
		Secrets: []model.HarnessConfigSecret{{Name: "OPENAI_API_KEY", Required: true}},
	}
	if err := st.CreateHarnessConfig(context.Background(), config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	return config
}

func bearerSecret(t *testing.T, st *store.Store, name, host string) *model.Secret {
	t.Helper()
	sec := &model.Secret{ProjectID: "project-1", Name: name, Type: model.SecretTypeBearer, Host: host, EncryptedValue: []byte(`{"token":"sk-abc"}`)}
	if err := st.CreateSecret(context.Background(), sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	return sec
}

func TestApplyHarnessConfigSecretsMaterializesBinding(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := codexConfig(t, st)
	sec := bearerSecret(t, st, "openai", "")
	if err := st.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
		ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "OPENAI_API_KEY", SecretID: sec.ID,
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}

	sandbox := &model.Sandbox{ID: "sb-1", ProjectID: "project-1"}
	assignments, err := svc.applyHarnessConfigSecrets(ctx, "project-1", sandbox, config.ID, nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(assignments) != 1 || assignments[0].EnvName != "OPENAI_API_KEY" || assignments[0].SecretID != sec.ID {
		t.Fatalf("assignments = %#v", assignments)
	}
	sentinel := assignments[0].Sentinel
	if sentinel == "" || sentinel == "sk-abc" {
		t.Fatalf("sentinel = %q, want a non-empty sentinel that is not the raw value", sentinel)
	}
	if sandbox.Env["OPENAI_API_KEY"] != "" {
		t.Fatalf("sandbox.Env = %#v, secret sentinels must never ride in sandbox.Env (ADR 0012)", sandbox.Env)
	}
}

func TestApplyHarnessConfigSecretsBlocksMissingRequired(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := codexConfig(t, st)

	_, err := svc.applyHarnessConfigSecrets(ctx, "project-1", &model.Sandbox{ID: "sb-1", ProjectID: "project-1"}, config.ID, nil)
	if err == nil {
		t.Fatal("expected block-create error for unbound required secret")
	}
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest {
		t.Fatalf("err = %v, want 400", err)
	}
}

func TestApplyHarnessConfigSecretsInlineSatisfiesRequired(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := codexConfig(t, st)

	// An inline per-sandbox secret for the same env satisfies the requirement and
	// suppresses any binding materialization.
	inline := map[string]struct{}{"OPENAI_API_KEY": {}}
	assignments, err := svc.applyHarnessConfigSecrets(ctx, "project-1", &model.Sandbox{ID: "sb-1", ProjectID: "project-1"}, config.ID, inline)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("assignments = %#v, want none (inline wins)", assignments)
	}
}

func TestApplyHarnessConfigSecretsOptionalUnboundIsAllowed(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "claude-code", Name: "claude-code", RunCommand: []string{"claude"},
		Secrets: []model.HarnessConfigSecret{{Name: "ANTHROPIC_API_KEY", Required: false}},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}

	assignments, err := svc.applyHarnessConfigSecrets(ctx, "project-1", &model.Sandbox{ID: "sb-1", ProjectID: "project-1"}, config.ID, nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("assignments = %#v, want none", assignments)
	}
}

// A configure sandbox is offered the previous configuration's secrets under
// PREV_-prefixed names. The prefix is the point: seeding OPENAI_API_KEY itself
// would let the harness CLI silently authenticate with the old credential, so
// the configure flow could neither present a real choice nor verify what it
// collected.
func TestApplyPreviousConfigureSecretsPrefixesAndSentinelizes(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := codexConfig(t, st)
	configured := bearerSecret(t, st, "openai", "")
	handBound := bearerSecret(t, st, "hand-bound", "other.example.com")
	config.ConfiguredSecretIDs = []string{configured.ID}
	if err := st.UpdateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("update config: %v", err)
	}
	for env, sec := range map[string]*model.Secret{"OPENAI_API_KEY": configured, "OTHER_KEY": handBound} {
		if err := st.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
			ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: env, SecretID: sec.ID,
		}); err != nil {
			t.Fatalf("bind %s: %v", env, err)
		}
	}

	sandbox := &model.Sandbox{ID: "sb-1", ProjectID: "project-1"}
	assignments, err := svc.applyPreviousConfigureSecrets(ctx, "project-1", sandbox, config.ID)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Only the configure flow's own secret is replayed; the hand-bound one is the
	// user's, not the flow's.
	if len(assignments) != 1 || assignments[0].SecretID != configured.ID {
		t.Fatalf("assignments = %#v, want only the configure-created secret", assignments)
	}
	if assignments[0].EnvName != "PREV_OPENAI_API_KEY" {
		t.Fatalf("env name = %q, want the PREV_ prefixed name", assignments[0].EnvName)
	}
	sentinel := assignments[0].Sentinel
	if sentinel == "" || sentinel == "sk-abc" {
		t.Fatalf("sentinel = %q, want a sentinel rather than the credential", sentinel)
	}
	if len(sandbox.Env) != 0 {
		t.Fatalf("sandbox.Env = %#v, secret sentinels must never ride in sandbox.Env (ADR 0012)", sandbox.Env)
	}
}

// An unconfigured harness has nothing to replay: a first-time configure sandbox
// gets no PREV_ variables at all.
func TestApplyPreviousConfigureSecretsSkipsUnconfiguredHarness(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := codexConfig(t, st)

	sandbox := &model.Sandbox{ID: "sb-1", ProjectID: "project-1"}
	assignments, err := svc.applyPreviousConfigureSecrets(ctx, "project-1", sandbox, config.ID)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(assignments) != 0 || len(sandbox.Env) != 0 {
		t.Fatalf("assignments = %#v, env = %#v, want nothing replayed", assignments, sandbox.Env)
	}
}
