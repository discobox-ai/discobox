package sandboxes

import (
	"context"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// presentSandbox stores a pinned sandbox with the present intent repair acts
// on, so a repair is not refused before it starts.
func presentSandbox(t *testing.T, st *store.Store, configID, bundle, digest string) *model.Sandbox {
	t.Helper()
	sb := pinnedSandbox(t, st, configID, bundle, digest)
	sb.DesiredState = model.DesiredStatePresent
	sb.State = model.SandboxStateFailed
	message := "create failed: no such file or directory"
	sb.ErrorMessage = &message
	if err := st.UpdateSandbox(context.Background(), sb); err != nil {
		t.Fatalf("store present sandbox: %v", err)
	}
	return sb
}

// The rebuild lands on the current bundle (ADR 0062 §1). A stale bundle is itself
// a way for a sandbox to be wedged, and the teardown has already paid what a
// re-pin costs, so repairing onto the old pin would rebuild the container that
// could not work and report it fixed.
func TestRepairRebuildsOnTheCurrentBundle(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	config := bundledConfig(t, st, codexBundle, codexBundleV2)
	sb := presentSandbox(t, st, config.ID, codexBundle, codexBundleV1)

	repaired, err := svc.RepairSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatalf("repair sandbox: %v", err)
	}
	if repaired.BundleDigest != codexBundleV2 {
		t.Fatalf("digest = %q, want the config's current one: repair is an upgrade too", repaired.BundleDigest)
	}
	// Still one intent: the re-pin rides the generation that names the repair.
	if repaired.RepairGeneration != repaired.Generation {
		t.Fatalf("repair generation = %d, generation = %d: the re-pin must ride the repair's own intent",
			repaired.RepairGeneration, repaired.Generation)
	}
	if repaired.ErrorMessage != nil {
		t.Fatalf("error message = %q, want it cleared by the recorded intent", *repaired.ErrorMessage)
	}
}

// Upgrade refuses when there is nothing newer, because the re-pin is what it
// was asked for. For repair the re-pin is a rider, so nothing newer just means
// the rebuild uses the pin it has (ADR 0062 §2).
func TestRepairProceedsWithNothingNewerToPinTo(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	config := bundledConfig(t, st, codexBundle, codexBundleV1)
	sb := presentSandbox(t, st, config.ID, codexBundle, codexBundleV1)

	repaired, err := svc.RepairSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatalf("repair sandbox: %v", err)
	}
	if repaired.Bundle != codexBundle || repaired.BundleDigest != codexBundleV1 {
		t.Fatalf("pin = %s@%s, want the one it already had", repaired.Bundle, repaired.BundleDigest)
	}
	if repaired.RepairGeneration != repaired.Generation {
		t.Fatalf("repair generation = %d, generation = %d", repaired.RepairGeneration, repaired.Generation)
	}
}

// A sandbox with no bundle to move to — no harness config, nothing seeded — is
// repaired all the same. Upgrade 409s there; the recovery path cannot.
func TestRepairProceedsWithNoBundleToPinTo(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	sb := presentSandbox(t, st, "", shellBundle, shellBundleV1)

	repaired, err := svc.RepairSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatalf("repair sandbox: %v", err)
	}
	if repaired.BundleDigest != shellBundleV1 {
		t.Fatalf("digest = %q, want the pin it had", repaired.BundleDigest)
	}
}

// The re-pin's legacy rider: a sandbox created before every sandbox carried a
// harness config resolves its target through the fallback, and pinning that
// config's bundle without adopting the config would leave the row describing a
// bundle no config of its own names (ADR 0062 §3).
func TestRepairAdoptsTheFallbackConfigWithTheRepin(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	shell := shellConfig(t, st, shellBundle, shellBundleV2)
	sb := presentSandbox(t, st, "", shellBundle, shellBundleV1)

	repaired, err := svc.RepairSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatalf("repair sandbox: %v", err)
	}
	if repaired.HarnessConfigID == nil || *repaired.HarnessConfigID != shell.ID {
		t.Fatalf("harness config = %v, want the adopted shell config %s", repaired.HarnessConfigID, shell.ID)
	}
	if repaired.Bundle != shell.Bundle || repaired.BundleDigest != shell.BundleDigest {
		t.Fatalf("pin = %s@%s, want the shell config's", repaired.Bundle, repaired.BundleDigest)
	}
}

// Archive is undone by unarchive, and repair says so rather than rebuilding a
// sandbox whose container is gone by intent (ADR 0035).
func TestRepairRefusedOnAnArchivedSandbox(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	config := bundledConfig(t, st, codexBundle, codexBundleV2)
	sb := presentSandbox(t, st, config.ID, codexBundle, codexBundleV1)
	sb.DesiredState = model.DesiredStateArchived
	if err := st.UpdateSandbox(ctx, sb); err != nil {
		t.Fatalf("archive sandbox: %v", err)
	}

	if _, err := svc.RepairSandbox(ctx, "project-1", sb.ID); err == nil {
		t.Fatal("repair should be refused on an archived sandbox")
	}
	stored, err := st.GetSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if stored.BundleDigest != codexBundleV1 {
		t.Fatalf("digest = %q: a refused repair must not re-pin", stored.BundleDigest)
	}
}
