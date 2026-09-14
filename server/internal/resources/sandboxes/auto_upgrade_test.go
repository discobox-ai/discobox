package sandboxes

import (
	"context"
	"fmt"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// eligibleSandbox stores a sandbox in the healthy shape an automatic upgrade
// acts on (ADR 0082 §2): converged at `ready`, observed `stopped`, and present.
// mutate bends exactly one of those for the negative cases, or turns it into
// the failed shape ADR 0121 also admits.
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

// A failed sandbox is the one most likely to be failing because of its image,
// so it moves with the rest (ADR 0121). The re-pin is intent like any other:
// it clears the latched error, which is what lets the reconciler retry the
// create on the new image rather than treat the failure as settled.
//
// Never observed is admitted for a failed sandbox, unlike a ready one: it is
// the first create that failed before any container was seen, so there is
// nothing running to restart.
func TestAutomaticUpgradeMovesAFailedSandbox(t *testing.T) {
	cases := []struct {
		name         string
		runtimeState string
	}{
		{"stopped", model.SandboxRuntimeStateStopped},
		{"never observed", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, st := newUpgradeEngineFixture(t)
			config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:new")
			failed := "image not found"
			sb := eligibleSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:old", func(sb *model.Sandbox) {
				sb.State = model.SandboxStateFailed
				sb.ErrorMessage = &failed
				sb.RuntimeState = tc.runtimeState
			})
			before := sb.Generation

			if err := svc.UpgradeHarnessConfigSandboxes(ctx, "project-1", config.ID); err != nil {
				t.Fatalf("upgrade harness config sandboxes: %v", err)
			}

			stored, err := st.GetSandbox(ctx, "project-1", sb.ID)
			if err != nil {
				t.Fatalf("get sandbox: %v", err)
			}
			if stored.ImageDigest != "sha256:new" {
				t.Fatalf("pin = %q, want the config's current image", stored.ImageDigest)
			}
			if stored.Generation <= before || stored.Converged() {
				t.Fatalf("generation = %d observed %d, want an unsettled bump past %d", stored.Generation, stored.ObservedGeneration, before)
			}
			if stored.ErrorMessage != nil {
				t.Fatalf("error = %q, want it cleared so the reconciler retries", *stored.ErrorMessage)
			}
			if stored.RepairGeneration == stored.Generation {
				t.Fatal("recorded a repair; an automatic upgrade of a failed sandbox is the plain re-pin")
			}
		})
	}
}

// Everything the eligibility rule excludes, in one table. Each case bends
// exactly one condition, so a failure names the condition that stopped working.
func TestAutomaticUpgradeSkipsAnythingButAStoppedSettledSandbox(t *testing.T) {
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
		// Not observed is not stopped (ADR 0034 §2): a ready sandbox nobody
		// has reported on is in the window before its create's report lands.
		{"never observed", func(sb *model.Sandbox) { sb.RuntimeState = "" }},
		{"still creating", func(sb *model.Sandbox) { sb.State = model.SandboxStatePending }},
		{"awaiting its source", func(sb *model.Sandbox) { sb.State = model.SandboxStateAwaitingSource }},
		// Failed is admitted stopped or never observed, never running
		// (ADR 0121): a failed sandbox that is running would be restarted
		// unattended.
		{"failed and running", func(sb *model.Sandbox) {
			sb.State = model.SandboxStateFailed
			sb.ErrorMessage = &failed
			sb.RuntimeState = model.SandboxRuntimeStateRunning
		}},
		{"failed and unsettled", func(sb *model.Sandbox) {
			sb.State = model.SandboxStateFailed
			sb.ErrorMessage = &failed
			sb.Generation = 4
			sb.ObservedGeneration = 3
		}},
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

// retryRecordingProvider records what the reconcile after an automatic upgrade
// asks of the provider: whether it rebuilt, whether it was told to start, and
// whether it tore down first as a repair would.
type retryRecordingProvider struct {
	recordingProvider
	creates  int
	started  bool
	archives int
}

func (p *retryRecordingProvider) Create(_ context.Context, _ sandbox.SandboxRef, state []byte, opts sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	p.creates++
	p.started = p.started || opts.Start
	return &sandbox.Sandbox{ID: "runtime-1"}, state, nil
}

func (p *retryRecordingProvider) Archive(context.Context, sandbox.SandboxRef, []byte) ([]byte, error) {
	p.archives++
	return nil, nil
}

// What ADR 0121 claims happens after the re-pin, driven through the reconciler
// rather than read off the row: the failed create is retried on the new image,
// is not a repair, starts nothing, and settles `ready`.
func TestAutomaticUpgradeRetriesAFailedSandboxWithoutStartingIt(t *testing.T) {
	for _, runtimeState := range []string{model.SandboxRuntimeStateStopped, ""} {
		name := runtimeState
		if name == "" {
			name = "never observed"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			svc, st := newUpgradeEngineFixture(t)
			config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:new")
			failed := "pull image: not found"
			sb := eligibleSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:old", func(sb *model.Sandbox) {
				sb.State = model.SandboxStateFailed
				sb.ErrorMessage = &failed
				sb.RuntimeState = runtimeState
			})

			if err := svc.UpgradeHarnessConfigSandboxes(ctx, "project-1", config.ID); err != nil {
				t.Fatalf("upgrade harness config sandboxes: %v", err)
			}
			repinned, err := st.GetSandbox(ctx, "project-1", sb.ID)
			if err != nil {
				t.Fatalf("get sandbox: %v", err)
			}
			provider := &retryRecordingProvider{}
			if _, err := NewSandboxReconciler(st, WithSandboxProvider(provider)).ReconcileSandbox(ctx, repinned); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			if provider.creates != 1 {
				t.Fatalf("creates = %d, want 1: the failure must be retried", provider.creates)
			}
			if provider.started {
				t.Fatal("the retry asked the provider to start the sandbox; an automatic upgrade starts nothing")
			}
			if provider.archives != 0 {
				t.Fatalf("archives = %d, want 0: an automatic upgrade is not a repair", provider.archives)
			}
			stored, err := st.GetSandbox(ctx, "project-1", sb.ID)
			if err != nil {
				t.Fatalf("get sandbox: %v", err)
			}
			if stored.State != model.SandboxStateReady || stored.ErrorMessage != nil || !stored.Converged() {
				t.Fatalf("state = %q, error = %v, generation %d observed %d; want ready, clear, and settled",
					stored.State, stored.ErrorMessage, stored.Generation, stored.ObservedGeneration)
			}
		})
	}
}

// A failed sandbox still owed a client push is left where it is: a retry could
// only park it at `awaiting_source` again, reading as starting while nobody
// pushes, and then replace its cause with the push timeout (ADR 0121).
func TestAutomaticUpgradeSkipsAFailedSandboxAwaitingItsPush(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:new")
	failed := "pull image: not found"
	sb := eligibleSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:old", func(sb *model.Sandbox) {
		sb.State = model.SandboxStateFailed
		sb.ErrorMessage = &failed
		sb.Source = &model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryPush}
	})

	if err := svc.UpgradeHarnessConfigSandboxes(ctx, "project-1", config.ID); err != nil {
		t.Fatalf("upgrade harness config sandboxes: %v", err)
	}
	stored, err := st.GetSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if stored.ImageDigest != "sha256:old" || stored.Generation != sb.Generation || stored.ErrorMessage == nil {
		t.Fatalf("pin = %q, generation %d (was %d), error = %v; want it untouched",
			stored.ImageDigest, stored.Generation, sb.Generation, stored.ErrorMessage)
	}
}
