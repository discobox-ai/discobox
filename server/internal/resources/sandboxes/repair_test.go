package sandboxes

import (
	"context"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// presentSandbox stores a pinned sandbox with the present intent repair acts
// on, so a repair is not refused before it starts.
func presentSandbox(t *testing.T, st *store.Store, configID, image, digest string) *model.Sandbox {
	t.Helper()
	sb := pinnedSandbox(t, st, configID, image, digest)
	sb.DesiredState = model.DesiredStatePresent
	sb.State = model.SandboxStateFailed
	message := "create failed: no such file or directory"
	sb.ErrorMessage = &message
	if err := st.UpdateSandbox(context.Background(), sb); err != nil {
		t.Fatalf("store present sandbox: %v", err)
	}
	return sb
}

// The rebuild lands on the current image (ADR 0062 §1). A stale image is itself
// a way for a sandbox to be wedged, and the teardown has already paid what a
// re-pin costs, so repairing onto the old pin would rebuild the container that
// could not work and report it fixed.
func TestRepairRebuildsOnTheCurrentImage(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:new")
	sb := presentSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:old")

	repaired, err := svc.RepairSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatalf("repair sandbox: %v", err)
	}
	if repaired.ImageDigest != "sha256:new" {
		t.Fatalf("digest = %q, want the config's current one: repair is an upgrade too", repaired.ImageDigest)
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
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:same")
	sb := presentSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:same")

	repaired, err := svc.RepairSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatalf("repair sandbox: %v", err)
	}
	if repaired.Image != "discobox-harness-codex:local" || repaired.ImageDigest != "sha256:same" {
		t.Fatalf("pin = %s@%s, want the one it already had", repaired.Image, repaired.ImageDigest)
	}
	if repaired.RepairGeneration != repaired.Generation {
		t.Fatalf("repair generation = %d, generation = %d", repaired.RepairGeneration, repaired.Generation)
	}
}

// A sandbox with no image to move to — no harness config, nothing seeded — is
// repaired all the same. Upgrade 409s there; the recovery path cannot.
func TestRepairProceedsWithNoImageToPinTo(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	sb := presentSandbox(t, st, "", "discobox-sandbox-agent:local", "sha256:old")

	repaired, err := svc.RepairSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatalf("repair sandbox: %v", err)
	}
	if repaired.ImageDigest != "sha256:old" {
		t.Fatalf("digest = %q, want the pin it had", repaired.ImageDigest)
	}
}

// The re-pin's legacy rider: a sandbox created before every sandbox carried a
// harness config resolves its target through the fallback, and pinning that
// config's image without adopting the config would leave the row describing an
// image no config of its own names (ADR 0062 §3).
func TestRepairAdoptsTheFallbackConfigWithTheRepin(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	shell := shellConfig(t, st, "discobox-sandbox-agent:dev-new", "sha256:default-new")
	sb := presentSandbox(t, st, "", "discobox-sandbox-agent:dev-old", "sha256:default-old")

	repaired, err := svc.RepairSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatalf("repair sandbox: %v", err)
	}
	if repaired.HarnessConfigID == nil || *repaired.HarnessConfigID != shell.ID {
		t.Fatalf("harness config = %v, want the adopted shell config %s", repaired.HarnessConfigID, shell.ID)
	}
	if repaired.Image != shell.Image || repaired.ImageDigest != shell.ImageDigest {
		t.Fatalf("pin = %s@%s, want the shell config's", repaired.Image, repaired.ImageDigest)
	}
}

// Archive is undone by unarchive, and repair says so rather than rebuilding a
// sandbox whose container is gone by intent (ADR 0035).
func TestRepairRefusedOnAnArchivedSandbox(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:new")
	sb := presentSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:old")
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
	if stored.ImageDigest != "sha256:old" {
		t.Fatalf("digest = %q: a refused repair must not re-pin", stored.ImageDigest)
	}
}
