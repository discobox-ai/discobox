package sandboxes

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/store"
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
		ID: "project-1", OwnerUserID: "user-1", Name: "Project", Slug: "project",
	}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	st := store.New(db.Write, db.Read)
	return &Service{store: st}, st
}

// codexConfig stores an agent config declaring OPENAI_API_KEY as required.
func codexConfig(t *testing.T, st *store.Store) *model.AgentConfig {
	t.Helper()
	config := &model.AgentConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", RunCommand: []string{"codex"},
		Secrets: []model.AgentConfigSecret{{Name: "OPENAI_API_KEY", Required: true}},
	}
	if err := st.CreateAgentConfig(context.Background(), config); err != nil {
		t.Fatalf("create agent config: %v", err)
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

func TestApplyAgentConfigSecretsMaterializesBinding(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := codexConfig(t, st)
	sec := bearerSecret(t, st, "openai", "")
	if err := st.UpsertAgentConfigSecretBinding(ctx, &model.AgentConfigSecretBinding{
		ProjectID: "project-1", AgentConfigID: config.ID, EnvName: "OPENAI_API_KEY", SecretID: sec.ID,
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}

	sandbox := &model.Sandbox{ID: "sb-1", ProjectID: "project-1"}
	assignments, err := svc.applyAgentConfigSecrets(ctx, "project-1", sandbox, config.ID, nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(assignments) != 1 || assignments[0].EnvName != "OPENAI_API_KEY" || assignments[0].SecretID != sec.ID {
		t.Fatalf("assignments = %#v", assignments)
	}
	sentinel := sandbox.Env["OPENAI_API_KEY"]
	if sentinel == "" || sentinel == "sk-abc" {
		t.Fatalf("env = %q, want a non-empty sentinel that is not the raw value", sentinel)
	}
	if assignments[0].Sentinel != sentinel {
		t.Fatalf("assignment sentinel %q != env sentinel %q", assignments[0].Sentinel, sentinel)
	}
}

func TestApplyAgentConfigSecretsBlocksMissingRequired(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := codexConfig(t, st)

	_, err := svc.applyAgentConfigSecrets(ctx, "project-1", &model.Sandbox{ID: "sb-1", ProjectID: "project-1"}, config.ID, nil)
	if err == nil {
		t.Fatal("expected block-create error for unbound required secret")
	}
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest {
		t.Fatalf("err = %v, want 400", err)
	}
}

func TestApplyAgentConfigSecretsInlineSatisfiesRequired(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := codexConfig(t, st)

	// An inline per-sandbox secret for the same env satisfies the requirement and
	// suppresses any binding materialization.
	inline := map[string]struct{}{"OPENAI_API_KEY": {}}
	assignments, err := svc.applyAgentConfigSecrets(ctx, "project-1", &model.Sandbox{ID: "sb-1", ProjectID: "project-1"}, config.ID, inline)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("assignments = %#v, want none (inline wins)", assignments)
	}
}

func TestApplyAgentConfigSecretsOptionalUnboundIsAllowed(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := &model.AgentConfig{
		ProjectID: "project-1", Slug: "opencode", Name: "opencode", RunCommand: []string{"opencode"},
		Secrets: []model.AgentConfigSecret{{Name: "ANTHROPIC_API_KEY", Required: false}},
	}
	if err := st.CreateAgentConfig(ctx, config); err != nil {
		t.Fatalf("create agent config: %v", err)
	}

	assignments, err := svc.applyAgentConfigSecrets(ctx, "project-1", &model.Sandbox{ID: "sb-1", ProjectID: "project-1"}, config.ID, nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("assignments = %#v, want none", assignments)
	}
}
