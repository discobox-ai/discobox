package sandboxes

import (
	"context"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// newRebindFixture builds a running sandbox whose harness config binds
// OPENAI_API_KEY, with the assignment already materialized.
func newRebindFixture(t *testing.T) (*Service, *store.Store, *recordingProvider, *model.HarnessConfig, *model.Secret) {
	t.Helper()
	ctx := context.Background()
	svc, rec := newAssignFixture(t)
	st := svc.store
	config := codexConfig(t, st)
	sec := bearerSecret(t, st, "openai", "")
	if err := st.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
		ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "OPENAI_API_KEY", SecretID: sec.ID,
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	sb, err := st.GetSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	sb.HarnessConfigID = &config.ID
	if err := st.UpdateSandbox(ctx, sb); err != nil {
		t.Fatalf("update sandbox: %v", err)
	}
	// runtime_state is an observed column, so it only moves through a state
	// report — the same path the pool agent uses.
	if _, err := st.ApplySandboxStateReports(ctx, store.SandboxStateReportBatch{
		PoolID:     "pool-1",
		BootID:     "boot-1",
		Sequence:   1,
		ReportedAt: time.Now().UTC(),
		Reports:    []store.SandboxStateReport{{SandboxID: "sb-1", State: model.SandboxRuntimeStateRunning}},
	}); err != nil {
		t.Fatalf("report running: %v", err)
	}
	assignments, err := svc.applyHarnessConfigSecrets(ctx, "project-1", sb, config.ID, nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, a := range assignments {
		if err := st.CreateSandboxSecret(ctx, a); err != nil {
			t.Fatalf("create assignment: %v", err)
		}
	}
	return svc, st, rec, config, sec
}

func onlyAssignment(t *testing.T, st *store.Store) model.SandboxSecret {
	t.Helper()
	got, err := st.ListSandboxSecrets(context.Background(), "project-1", "sb-1")
	if err != nil {
		t.Fatalf("list assignments: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 assignment, got %d", len(got))
	}
	return got[0]
}

// A replacement secret of the same shape must be picked up without disturbing
// the sentinel: the harness cannot re-read its credential file while running.
func TestRebindKeepsSentinelWhenFormatMatches(t *testing.T) {
	ctx := context.Background()
	svc, st, rec, config, old := newRebindFixture(t)
	before := onlyAssignment(t, st)

	replacement := &model.Secret{
		ProjectID: "project-1", Name: "openai-2", Type: model.SecretTypeBearer,
		// Distinct UniqueKey: a replacement coexists with the secret it
		// supersedes, which the (project,type,host) slot alone would forbid.
		UniqueKey: "replacement-1", EncryptedValue: []byte(`{"token":"sk-xyz"}`),
	}
	if err := st.CreateSecret(ctx, replacement); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if err := st.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
		ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "OPENAI_API_KEY", SecretID: replacement.ID,
	}); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if err := svc.RebindHarnessConfigSecrets(ctx, "project-1", config.ID); err != nil {
		t.Fatalf("rebind secrets: %v", err)
	}

	after := onlyAssignment(t, st)
	if after.SecretID != replacement.ID {
		t.Fatalf("assignment still names %s, want %s", after.SecretID, replacement.ID)
	}
	if after.SecretID == old.ID {
		t.Fatal("assignment was not repointed")
	}
	if after.Sentinel != before.Sentinel {
		t.Fatalf("sentinel changed for a same-format rebind: %q -> %q", before.Sentinel, after.Sentinel)
	}
	// The push is what drops the proxy's cached value for the old secret.
	if len(rec.updated) == 0 {
		t.Fatal("no sentinel push after rebind")
	}
}

// A replacement of a different shape has to re-mint, because the sentinel
// byte-mimics the credential it stands in for.
func TestRebindReMintsWhenFormatDiffers(t *testing.T) {
	ctx := context.Background()
	svc, st, _, config, _ := newRebindFixture(t)
	before := onlyAssignment(t, st)

	replacement := &model.Secret{
		ProjectID: "project-1", Name: "openai-3", Type: model.SecretTypeBearer,
		UniqueKey:      "replacement-2",
		EncryptedValue: []byte(`{"token":"github_pat_11ABCDEFG0123456789abcdefghij"}`),
	}
	if err := st.CreateSecret(ctx, replacement); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if err := st.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
		ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "OPENAI_API_KEY", SecretID: replacement.ID,
	}); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if err := svc.RebindHarnessConfigSecrets(ctx, "project-1", config.ID); err != nil {
		t.Fatalf("rebind secrets: %v", err)
	}

	after := onlyAssignment(t, st)
	if after.SecretID != replacement.ID {
		t.Fatalf("assignment still names %s, want %s", after.SecretID, replacement.ID)
	}
	if after.Sentinel == before.Sentinel {
		t.Fatal("sentinel kept across a format change")
	}
	if after.Format == before.Format {
		t.Fatalf("format not updated, still %q", after.Format)
	}
}

// An assignment already naming the bound secret is left completely alone, so a
// rebind on an unrelated env does not churn sentinels.
func TestRebindIsNoOpWhenAlreadyCurrent(t *testing.T) {
	ctx := context.Background()
	svc, st, rec, config, _ := newRebindFixture(t)
	before := onlyAssignment(t, st)

	if err := svc.RebindHarnessConfigSecrets(ctx, "project-1", config.ID); err != nil {
		t.Fatalf("rebind secrets: %v", err)
	}
	after := onlyAssignment(t, st)
	if after.Sentinel != before.Sentinel || after.SecretID != before.SecretID {
		t.Fatal("assignment changed on a no-op rebind")
	}
	if len(rec.updated) != 0 {
		t.Fatalf("pushed %d times on a no-op rebind", len(rec.updated))
	}
}

// Deleting a secret must take its sandbox assignments with it: a sentinel the
// proxy still swaps on but can never resolve reaches the harness as a 401.
func TestDeleteSecretDropsSandboxAssignments(t *testing.T) {
	ctx := context.Background()
	_, st, _, _, sec := newRebindFixture(t)
	if got := onlyAssignment(t, st); got.SecretID != sec.ID {
		t.Fatalf("fixture assignment names %s, want %s", got.SecretID, sec.ID)
	}
	if err := st.DeleteSecret(ctx, "project-1", sec.ID); err != nil {
		t.Fatalf("delete secret: %v", err)
	}
	got, err := st.ListSandboxSecrets(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatalf("list assignments: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("assignment survived its secret: %+v", got)
	}
}

// The reconciler repairs drift the binding-change fan-out never reached, which
// is what makes a sandbox that was archived or unreachable across the change
// come back up with a working credential.
func TestRebindRowsRepairsDriftWithoutService(t *testing.T) {
	ctx := context.Background()
	_, st, _, config, old := newRebindFixture(t)
	before := onlyAssignment(t, st)

	replacement := &model.Secret{
		ProjectID: "project-1", Name: "openai-4", Type: model.SecretTypeBearer,
		UniqueKey: "replacement-3", EncryptedValue: []byte(`{"token":"sk-abc"}`),
	}
	if err := st.CreateSecret(ctx, replacement); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if err := st.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
		ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "OPENAI_API_KEY", SecretID: replacement.ID,
	}); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	// No fan-out: the sandbox is repaired only by the reconciler's own pass.
	sb, err := st.GetSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	changed, err := rebindSandboxSecretRows(ctx, st, "project-1", sb)
	if err != nil {
		t.Fatalf("rebind rows: %v", err)
	}
	if !changed {
		t.Fatal("drift reported as no change")
	}
	after := onlyAssignment(t, st)
	if after.SecretID != replacement.ID || after.SecretID == old.ID {
		t.Fatalf("assignment names %s, want %s", after.SecretID, replacement.ID)
	}
	if after.Sentinel != before.Sentinel {
		t.Fatal("sentinel churned on a same-format repair")
	}
}
