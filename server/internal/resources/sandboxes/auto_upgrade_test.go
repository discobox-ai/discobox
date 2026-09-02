package sandboxes

import (
	"context"
	"fmt"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// eligibleSandbox stores a sandbox in the one shape an automatic upgrade acts
// on (ADR 0082 §2): converged at `ready`, observed `stopped`, no error, and
// present. mutate bends exactly one of those for the negative cases.
//
// RuntimeState is set at create because that is the only place a test can put
// it: UpdateSandbox omits the column so no path but a state report can write it
// (ADR 0034 §2).
func eligibleSandbox(t *testing.T, st *store.Store, configID, image, digest string, mutate ...func(*model.Sandbox)) *model.Sandbox {
	t.Helper()
	ensurePool(t, st)
	sb := &model.Sandbox{
		ProjectID:       "project-1",
		CreatedByUserID: "user-1",
		PoolID:          "pool-1",
		Name:            fmt.Sprintf("sandbox-%d", pinnedSandboxSeq.Add(1)),
		SandboxManifest: model.SandboxManifest{HarnessMode: "run", Image: image, ImageDigest: digest},
		RuntimeState:    model.SandboxRuntimeStateStopped,
	}
	sb.DesiredState = model.DesiredStatePresent
	sb.State = model.SandboxStateReady
	if configID != "" {
		sb.HarnessConfigID = &configID
	}
	for _, fn := range mutate {
		fn(sb)
	}
	if err := st.CreateSandbox(context.Background(), sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	return sb
}

func storedPin(t *testing.T, st *store.Store, sandboxID string) (image, digest string, generation int64) {
	t.Helper()
	stored, err := st.GetSandbox(context.Background(), "project-1", sandboxID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	return stored.Image, stored.ImageDigest, stored.Generation
}

// The whole point: a harness image that moved carries the sandboxes stopped on
// it forward, with nobody typing anything (ADR 0082 §1).
func TestAutomaticUpgradeMovesAStoppedSandbox(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:new")
	sb := eligibleSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:old")
	before := sb.Generation

	if err := svc.UpgradeHarnessConfigSandboxes(ctx, "project-1", config.ID); err != nil {
		t.Fatalf("upgrade harness config sandboxes: %v", err)
	}

	image, digest, generation := storedPin(t, st, sb.ID)
	if image != "discobox-harness-codex:local" || digest != "sha256:new" {
		t.Fatalf("pin = %q/%q, want the config's current image", image, digest)
	}
	if generation <= before {
		t.Fatalf("generation = %d, want a bump past %d: the re-pin is intent like any other", generation, before)
	}
}

// Everything the eligibility rule excludes, in one table. Each case bends
// exactly one condition, so a failure names the condition that stopped working.
func TestAutomaticUpgradeSkipsAnythingButAStoppedConvergedSandbox(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:new")

	failed := "boom"
	cases := []struct {
		name   string
		mutate func(*model.Sandbox)
	}{
		{"running", func(sb *model.Sandbox) { sb.RuntimeState = model.SandboxRuntimeStateRunning }},
		{"starting", func(sb *model.Sandbox) { sb.RuntimeState = model.SandboxRuntimeStateStarting }},
		{"stopping", func(sb *model.Sandbox) { sb.RuntimeState = model.SandboxRuntimeStateStopping }},
		// Not observed is not stopped (ADR 0034 §2): acting on it would be
		// acting on no observation at all.
		{"never observed", func(sb *model.Sandbox) { sb.RuntimeState = "" }},
		{"still creating", func(sb *model.Sandbox) { sb.State = model.SandboxStatePending }},
		{"awaiting its source", func(sb *model.Sandbox) { sb.State = model.SandboxStateAwaitingSource }},
		// A settled failure needs intent aimed at the failure, which is repair
		// (ADR 0064), not another image.
		{"failed", func(sb *model.Sandbox) { sb.State = model.SandboxStateFailed }},
		{"carrying an error", func(sb *model.Sandbox) { sb.ErrorMessage = &failed }},
		{"unsettled", func(sb *model.Sandbox) { sb.Generation = 4; sb.ObservedGeneration = 3 }},
		{"being archived", func(sb *model.Sandbox) { sb.DesiredState = model.DesiredStateArchived }},
		{"being deleted", func(sb *model.Sandbox) { sb.DesiredState = model.DesiredStateDeleted }},
		// A config-mode sandbox runs the configure command against a
		// deliberately fixed image, and the shared rule is what says so.
		{"in config mode", func(sb *model.Sandbox) { sb.HarnessMode = "config" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := eligibleSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:old", tc.mutate)
			if err := svc.UpgradeHarnessConfigSandboxes(ctx, "project-1", config.ID); err != nil {
				t.Fatalf("upgrade harness config sandboxes: %v", err)
			}
			if image, digest, _ := storedPin(t, st, sb.ID); digest != "sha256:old" {
				t.Fatalf("pin moved to %q/%q; a sandbox that is %s must be left alone", image, digest, tc.name)
			}
		})
	}
}

// The opt-out is the project's, and it is asked before any sandbox is read
// (ADR 0082 §3).
func TestAutomaticUpgradeRespectsAManualProject(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:new")
	sb := eligibleSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:old")

	project, err := st.GetProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	project.SandboxUpgradePolicy = model.SandboxUpgradePolicyManual
	if err := st.UpsertProject(ctx, project); err != nil {
		t.Fatalf("upsert project: %v", err)
	}

	if err := svc.UpgradeHarnessConfigSandboxes(ctx, "project-1", config.ID); err != nil {
		t.Fatalf("upgrade harness config sandboxes: %v", err)
	}
	if _, digest, _ := storedPin(t, st, sb.ID); digest != "sha256:old" {
		t.Fatalf("pin moved to %q under a manual project", digest)
	}
	// Still reported, so a manual project can see what it is holding back from.
	stored, err := st.GetSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if target, err := svc.currentImageRepin(ctx, stored); err != nil || !target.Available {
		t.Fatalf("target = %+v, %v; want the upgrade still on offer", target, err)
	}
}

// There is no automatic-upgrade code path, only an automatic author of the
// upgrade every sandbox already had — so the row it writes must be
// indistinguishable from the one a typed upgrade writes (ADR 0082 §1).
func TestAutomaticUpgradeWritesWhatAnExplicitUpgradeWrites(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:new")
	typed := eligibleSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:old")
	automatic := eligibleSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:old")

	if _, err := svc.UpgradeSandbox(ctx, "project-1", typed.ID, services.UpgradeSandboxBody{}); err != nil {
		t.Fatalf("upgrade sandbox: %v", err)
	}
	if err := svc.UpgradeHarnessConfigSandboxes(ctx, "project-1", config.ID); err != nil {
		t.Fatalf("upgrade harness config sandboxes: %v", err)
	}

	typedImage, typedDigest, typedGeneration := storedPin(t, st, typed.ID)
	autoImage, autoDigest, autoGeneration := storedPin(t, st, automatic.ID)
	if typedImage != autoImage || typedDigest != autoDigest || typedGeneration != autoGeneration {
		t.Fatalf("automatic wrote %q/%q gen %d, typed wrote %q/%q gen %d; they must be the same operation",
			autoImage, autoDigest, autoGeneration, typedImage, typedDigest, typedGeneration)
	}
}

// The typed upgrade had already moved this one, so the fan-out has nothing to
// do: it must not bump a generation for a re-pin that changes nothing, which
// would cost a container rebuild for no image change.
func TestAutomaticUpgradeSkipsASandboxAlreadyOnTheImage(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:same")
	sb := eligibleSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:same")
	before := sb.Generation

	if err := svc.UpgradeHarnessConfigSandboxes(ctx, "project-1", config.ID); err != nil {
		t.Fatalf("upgrade harness config sandboxes: %v", err)
	}
	if _, _, generation := storedPin(t, st, sb.ID); generation != before {
		t.Fatalf("generation = %d, want it untouched at %d", generation, before)
	}
}
