package sandboxes

import (
	"context"
	"testing"

	"gorm.io/gorm"

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

// bearerSecret stores a token sent to host, or anywhere when host is empty.
func bearerSecret(t *testing.T, st *store.Store, name, host string) *model.Secret {
	t.Helper()
	if host == "" {
		host = "*"
	}
	sec := &model.Secret{ProjectID: "project-1", Name: name, Type: model.SecretTypeToken,
		Routes: []model.SecretRoute{{Hosts: []string{host}}}, EncryptedValue: []byte(`{"token":"sk-abc"}`)}
	if err := st.CreateSecret(context.Background(), sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	return sec
}

// bindUse binds a secret for use in env on a target.
func bindUse(t *testing.T, st *store.Store, secretID, kind, targetID, env string) *model.SecretBinding {
	t.Helper()
	binding := &model.SecretBinding{ProjectID: "project-1", SecretID: secretID, TargetKind: kind, TargetID: targetID, Use: true, EnvName: env}
	if err := st.CreateSecretBinding(context.Background(), binding, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	return binding
}

// deliver binds a secret on a sandbox and gives it the sentinel, as
// materializing the binding does.
func deliver(t *testing.T, st *store.Store, sandboxID, secretID, env, sentinel string) *model.SecretBinding {
	t.Helper()
	binding := bindUse(t, st, secretID, model.SecretBindingTargetSandbox, sandboxID, env)
	if err := st.CreateSandboxSecret(context.Background(), &model.SandboxSecret{
		ProjectID: "project-1", SandboxID: sandboxID, SecretID: secretID, EnvName: env, Sentinel: sentinel, BindingID: binding.ID,
	}); err != nil {
		t.Fatal(err)
	}
	return binding
}

func materialized(t *testing.T, st *store.Store, sandboxID string) map[string]model.SandboxSecret {
	t.Helper()
	ctx := context.Background()
	sb, err := st.GetSandbox(ctx, "project-1", sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := materializeSandboxSecrets(ctx, st, sb); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	rows, err := st.ListSandboxSecrets(ctx, "project-1", sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]model.SandboxSecret{}
	for _, row := range rows {
		out[row.EnvName] = row
	}
	return out
}

// A sandbox's sentinels are its effective use bindings: the harness config's
// until the sandbox binds the variable itself, which then wins with the same
// sentinel, and none once nothing binds it (ADR 26-10-27-880 §2).
func TestMaterializeMakesTheSentinelsTheEffectiveBindings(t *testing.T) {
	svc, _ := newAssignFixture(t)
	st := svc.store
	ctx := context.Background()
	config := codexConfig(t, st)
	sb, err := st.GetSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	sb.HarnessConfigID = &config.ID
	if err := st.UpdateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	shared := bearerSecret(t, st, "shared", "")
	own := bearerSecret(t, st, "own", "")
	harnessBinding := bindUse(t, st, shared.ID, model.SecretBindingTargetHarnessConfig, config.ID, "OPENAI_API_KEY")

	rows := materialized(t, st, "sb-1")
	first, ok := rows["OPENAI_API_KEY"]
	if len(rows) != 1 || !ok || first.BindingID != harnessBinding.ID || first.SecretID != shared.ID {
		t.Fatalf("rows = %+v, want the harness config's binding", rows)
	}
	ownBinding := bindUse(t, st, own.ID, model.SecretBindingTargetSandbox, "sb-1", "OPENAI_API_KEY")
	rows = materialized(t, st, "sb-1")
	if got := rows["OPENAI_API_KEY"]; got.BindingID != ownBinding.ID || got.SecretID != own.ID || got.Sentinel != first.Sentinel {
		t.Fatalf("row = %+v, want the sandbox's own binding under the same sentinel", got)
	}
	for _, binding := range []*model.SecretBinding{ownBinding, harnessBinding} {
		if _, err := st.DeleteSecretBinding(ctx, "project-1", binding.ID, store.BindingChange{Actor: "user-1"}); err != nil {
			t.Fatal(err)
		}
	}
	if rows = materialized(t, st, "sb-1"); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none once nothing binds the variable", rows)
	}
}

// A variable rebound to another secret (ADR 26-11-19-927 §§4-5) keeps its
// sentinel, so a process holding it switches to the new secret without a
// restart, and now delivers another value generation even when both values
// were written at the same instant, so the pool drops the old one. Only a
// secret minting sentinels of another shape gives the variable a new one.
func TestARebindKeepsTheSentinelAndMovesTheGeneration(t *testing.T) {
	svc, _ := newAssignFixture(t)
	st := svc.store
	ctx := context.Background()
	personal := bearerSecret(t, st, "personal", "")
	team := bearerSecret(t, st, "team", "")
	other := bearerSecret(t, st, "other-shape", "")
	written := personal.ValueWrittenAt()
	for _, sec := range []*model.Secret{team, other} {
		sec.ValueUpdatedAt = &written
	}
	other.Format, other.FormatSet = "xoxb-{alnum:40}", true
	for _, sec := range []*model.Secret{team, other} {
		if err := st.Transaction(ctx, func(_ *store.Store, tx *gorm.DB) error {
			return tx.Model(&model.Secret{}).Where("id = ?", sec.ID).Updates(map[string]any{"value_updated_at": written, "format": sec.Format, "format_set": sec.FormatSet}).Error
		}); err != nil {
			t.Fatal(err)
		}
	}
	rebindTo := func(from *model.SecretBinding, secretID string) *model.SecretBinding {
		t.Helper()
		before := *from
		if _, err := st.DeleteReboundBinding(ctx, "project-1", from.ID, store.BindingChange{Actor: "user-1"}); err != nil {
			t.Fatal(err)
		}
		to := &model.SecretBinding{ProjectID: "project-1", SecretID: secretID, TargetKind: model.SecretBindingTargetSandbox, TargetID: "sb-1", Use: true, EnvName: "GH_TOKEN"}
		if err := st.CreateSecretBinding(ctx, to, store.BindingChange{Actor: "user-1", Replaced: &before}); err != nil {
			t.Fatal(err)
		}
		return to
	}
	generation := func(row model.SandboxSecret) string {
		t.Helper()
		generations, err := st.SentinelGenerations(ctx, "project-1", []model.SandboxSecret{row})
		if err != nil {
			t.Fatal(err)
		}
		return generations[row.Sentinel]
	}

	first := bindUse(t, st, personal.ID, model.SecretBindingTargetSandbox, "sb-1", "GH_TOKEN")
	held := materialized(t, st, "sb-1")["GH_TOKEN"]
	second := rebindTo(first, team.ID)
	moved := materialized(t, st, "sb-1")["GH_TOKEN"]
	if moved.ID != held.ID || moved.Sentinel != held.Sentinel || moved.SecretID != team.ID || moved.BindingID != second.ID {
		t.Fatalf("row = %+v, want %+v repointed to %s under the same sentinel", moved, held, second.ID)
	}
	if generation(moved) == generation(held) {
		t.Fatalf("generation %q unchanged by a rebind between values written at the same instant", generation(moved))
	}
	rebindTo(second, other.ID)
	if reminted := materialized(t, st, "sb-1")["GH_TOKEN"]; reminted.Sentinel == held.Sentinel || reminted.SecretID != other.ID {
		t.Fatalf("row = %+v, want a new sentinel for a secret of another shape", reminted)
	}
}
