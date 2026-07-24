package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	sandboxauth "github.com/obot-platform/discobox/server/internal/auth/sandbox"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/reconcile"
	"github.com/obot-platform/discobox/server/internal/store"
)

// SandboxResourceType is the reconcile-engine resource type for sandboxes. The
// dirty id carries the project scope: "projectID/sandboxID".
const SandboxResourceType = "sandbox"

// SandboxDirtyID encodes the composite identity a sandbox reconcile needs.
func SandboxDirtyID(projectID, sandboxID string) string {
	return projectID + "/" + sandboxID
}

func splitSandboxDirtyID(id string) (projectID, sandboxID string, err error) {
	projectID, sandboxID, ok := strings.Cut(id, "/")
	if !ok || projectID == "" || sandboxID == "" {
		return "", "", fmt.Errorf("invalid sandbox dirty id %q", id)
	}
	return projectID, sandboxID, nil
}

// Reconcile loads the LATEST sandbox state and converges it. Semantics:
//   - missing sandbox: converged (settle).
//   - superseded mid-run (generation conflict): settle; the newer intent's
//     transactional mark re-runs us against current state.
//   - failure RECORDED on the resource (LastOperationStatus == failed):
//     converged-to-failed — one logical attempt, like the old MaxAttempts(1)
//     jobs. New intent re-drives it.
//   - failure NOT recorded (crash/timeout before the status write): return the
//     error so the row stays dirty and retries with backoff. This replaces the
//     old MarkSandboxJobFailed terminal latch with self-healing.
func (r *SandboxReconciler) Reconcile(ctx context.Context, id string) error {
	projectID, sandboxID, err := splitSandboxDirtyID(id)
	if err != nil {
		return err
	}
	sandbox, err := r.store.GetSandbox(ctx, projectID, sandboxID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	err = r.ReconcileSandbox(ctx, sandbox)
	if errors.Is(err, reconcile.ErrSuperseded) {
		return nil // superseded: newer intent's mark re-runs us
	}
	if err == nil {
		return nil
	}
	if current, gerr := r.store.GetSandbox(ctx, projectID, sandboxID); gerr == nil &&
		current.LastOperationStatus == model.SandboxOperationStatusFailed {
		return nil // failure recorded on the resource: converged until new intent
	}
	return err
}

// stuckSandboxCutoff mirrors the worker backstop: an operation recorded as in
// flight for this long means the dirty mark was lost.
const stuckSandboxCutoff = 10 * time.Minute

// ScanDirty is the level-triggered backstop: sandboxes whose recorded
// operation has been in flight implausibly long are re-marked. Terminal
// operations are excluded — a recorded failure is converged by design until
// new intent arrives.
func (r *SandboxReconciler) ScanDirty(ctx context.Context) ([]string, error) {
	pairs, err := r.store.ListSandboxIDsWithStaleOperations(ctx, time.Now().Add(-stuckSandboxCutoff))
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(pairs))
	for _, p := range pairs {
		ids = append(ids, SandboxDirtyID(p.ProjectID, p.SandboxID))
	}
	return ids, nil
}

// SandboxReconciler converges sandboxes toward their desired state. It
// implements reconcile.Reconciler and reconcile.Scanner (see reconciler.go for
// the engine entry points); this file holds the convergence state machine.
type SandboxReconciler struct {
	store    *store.Store
	provider Provider
	manager  *ProviderManager
	auth     SandboxAuthenticator
	engine   *reconcile.Engine
}

// SandboxReconcilerOption configures a sandbox reconciler.
type SandboxReconcilerOption func(*SandboxReconciler)

func NewSandboxReconciler(store *store.Store, options ...SandboxReconcilerOption) *SandboxReconciler {
	reconciler := &SandboxReconciler{
		store: store,
	}
	for _, option := range options {
		if option != nil {
			option(reconciler)
		}
	}
	return reconciler
}

// WithSandboxProvider uses a single provider for all sandbox reconciliation.
func WithSandboxProvider(provider Provider) SandboxReconcilerOption {
	return func(reconciler *SandboxReconciler) {
		reconciler.provider = provider
	}
}

// WithSandboxProviderManager resolves providers through a manager.
func WithSandboxProviderManager(manager *ProviderManager) SandboxReconcilerOption {
	return func(reconciler *SandboxReconciler) {
		reconciler.manager = manager
	}
}

// WithSandboxReconcileEngine lets the reconciler schedule its own future wake-up.
// A sandbox waiting for a client push has no other trigger: nothing changes on
// the server when a client goes away mid-push, so without a timer it would wait
// forever, holding its provider resources.
func WithSandboxReconcileEngine(engine *reconcile.Engine) SandboxReconcilerOption {
	return func(reconciler *SandboxReconciler) {
		reconciler.engine = engine
	}
}

// WithSandboxAuthenticator injects trust-key authentication into sandbox
// creation. When configured, sandbox starts ensure the creating user has a
// public trust key and pass it to the runtime as DISCOBOX_TRUST_KEY.
func WithSandboxAuthenticator(auth SandboxAuthenticator) SandboxReconcilerOption {
	return func(reconciler *SandboxReconciler) {
		reconciler.auth = auth
	}
}

// ReconcileSandbox converges the given (freshly loaded) sandbox toward its
// desired state. The sandbox's current generation guards every write, so newer
// intent arriving mid-run surfaces as a Superseded error, which the reconciler
// maps to a clean settle (the newer intent's own mark re-runs the reconcile).
func (r *SandboxReconciler) ReconcileSandbox(ctx context.Context, sandbox *model.Sandbox) error {
	generation := sandbox.Generation
	switch sandbox.DesiredState {
	case model.SandboxDesiredStateRunning:
		if sandbox.RestartGeneration > sandbox.RestartedGeneration {
			return r.restart(ctx, sandbox, generation)
		}
		return r.start(ctx, sandbox, generation)
	case model.SandboxDesiredStateStopped:
		return r.stop(ctx, sandbox, generation)
	case model.SandboxDesiredStateDeleted:
		return r.delete(ctx, sandbox, generation)
	default:
		return fmt.Errorf("unsupported sandbox desired state %q", sandbox.DesiredState)
	}
}

func (r *SandboxReconciler) start(ctx context.Context, sandbox *model.Sandbox, generation int64) error {
	if sandbox.Phase == model.SandboxPhaseRunning && sandbox.ObservedGeneration == generation && sandbox.LastOperationStatus == model.SandboxOperationStatusSuccess {
		return nil
	}

	if sandbox.Phase == model.SandboxPhaseStopped {
		r.repinToCurrentImage(ctx, sandbox)
	}

	status := "starting sandbox"
	sandbox.MarkOperationRunning(&status)
	if err := r.update(ctx, sandbox, generation); err != nil {
		return err
	}
	if err := r.startSandbox(ctx, sandbox); err != nil {
		sandbox.FailOperation(err.Error())
		if updateErr := r.update(ctx, sandbox, generation); updateErr != nil {
			return updateErr
		}
		return err
	}
	if sandbox.Phase == model.SandboxPhaseAwaitingSource {
		// Parked waiting for the client's push. The generation is fully handled
		// — there is nothing further to do until the client acts — so record it
		// as observed and leave the create operation running rather than
		// completing it as running.
		sandbox.ObservedGeneration = generation
		if err := r.update(ctx, sandbox, generation); err != nil {
			return err
		}
		return r.scheduleSourceAwaitTimeout(ctx, sandbox)
	}
	sandbox.ObservedGeneration = generation
	sandbox.CompleteOperation(model.SandboxPhaseRunning, nil)
	return r.update(ctx, sandbox, generation)
}

func (r *SandboxReconciler) restart(ctx context.Context, sandbox *model.Sandbox, generation int64) error {
	status := "restarting sandbox"
	sandbox.MarkOperationRunning(&status)
	if err := r.update(ctx, sandbox, generation); err != nil {
		return err
	}
	if err := r.stopSandbox(ctx, sandbox); err != nil {
		sandbox.FailOperation(err.Error())
		if updateErr := r.update(ctx, sandbox, generation); updateErr != nil {
			return updateErr
		}
		return err
	}
	if err := r.startSandbox(ctx, sandbox); err != nil {
		sandbox.FailOperation(err.Error())
		if updateErr := r.update(ctx, sandbox, generation); updateErr != nil {
			return updateErr
		}
		return err
	}
	sandbox.RestartedGeneration = sandbox.RestartGeneration
	sandbox.ObservedGeneration = generation
	sandbox.CompleteOperation(model.SandboxPhaseRunning, nil)
	return r.update(ctx, sandbox, generation)
}

// repinToCurrentImage moves a stopped sandbox onto its harness config's current
// image as it comes back up (ADR 0016 §5).
//
// A stopped sandbox has no session to interrupt and nothing running that a user
// is relying on, and starting it is the moment its container is built. Building
// it deliberately obsolete serves nobody, so the pin advances here — and only
// here. A running sandbox never moves without the explicit upgrade action, which
// is why this is reached from the stopped phase rather than from startSandbox,
// where every reconcile of a running sandbox would pass through it.
//
// Best-effort by design: a harness config that cannot be read is not a reason to
// refuse to start a sandbox that was going to start anyway on the image it
// already has.
func (r *SandboxReconciler) repinToCurrentImage(ctx context.Context, sb *model.Sandbox) {
	if r.store == nil || sb.HarnessConfigID == nil || strings.TrimSpace(sb.ImageDigest) == "" || sb.HarnessMode == "config" {
		return
	}
	config, err := r.store.GetHarnessConfig(ctx, sb.ProjectID, strings.TrimSpace(*sb.HarnessConfigID))
	if err != nil {
		return
	}
	image, digest := strings.TrimSpace(config.Image), strings.TrimSpace(config.ImageDigest)
	if image == "" || digest == "" || digest == strings.TrimSpace(sb.ImageDigest) {
		return
	}
	slog.InfoContext(ctx, "re-pinning stopped sandbox to its harness config's current image",
		"sandboxId", sb.ID, "image", image,
		"previousImageDigest", sb.ImageDigest, "imageDigest", digest)
	sb.Image, sb.ImageDigest = image, digest
}

func (r *SandboxReconciler) stop(ctx context.Context, sandbox *model.Sandbox, generation int64) error {
	if sandbox.Phase == model.SandboxPhaseStopped && sandbox.ObservedGeneration == generation && sandbox.LastOperationStatus == model.SandboxOperationStatusSuccess {
		return nil
	}

	status := "stopping sandbox"
	sandbox.MarkOperationRunning(&status)
	if err := r.update(ctx, sandbox, generation); err != nil {
		return err
	}
	if err := r.stopSandbox(ctx, sandbox); err != nil {
		sandbox.FailOperation(err.Error())
		if updateErr := r.update(ctx, sandbox, generation); updateErr != nil {
			return updateErr
		}
		return err
	}
	sandbox.ObservedGeneration = generation
	sandbox.CompleteOperation(model.SandboxPhaseStopped, nil)
	return r.update(ctx, sandbox, generation)
}

func (r *SandboxReconciler) delete(ctx context.Context, sandbox *model.Sandbox, generation int64) error {
	if sandbox.Phase == model.SandboxPhaseDeleted && sandbox.ObservedGeneration == generation && sandbox.LastOperationStatus == model.SandboxOperationStatusSuccess {
		return r.softDelete(ctx, sandbox, generation)
	}

	status := "deleting sandbox"
	sandbox.MarkOperationRunning(&status)
	if err := r.update(ctx, sandbox, generation); err != nil {
		return err
	}
	if err := r.deleteSandbox(ctx, sandbox); err != nil {
		sandbox.FailOperation(err.Error())
		if updateErr := r.update(ctx, sandbox, generation); updateErr != nil {
			return updateErr
		}
		return err
	}
	sandbox.ObservedGeneration = generation
	sandbox.CompleteOperation(model.SandboxPhaseDeleted, nil)
	if err := r.update(ctx, sandbox, generation); err != nil {
		return err
	}
	return r.softDelete(ctx, sandbox, generation)
}

func (r *SandboxReconciler) softDelete(ctx context.Context, sandbox *model.Sandbox, generation int64) error {
	if err := r.store.DeleteSandbox(ctx, sandbox.ProjectID, sandbox.ID, store.WithGeneration(generation)); err != nil {
		if errors.Is(err, store.ErrGenerationConflict) {
			return reconcile.Superseded("sandbox generation changed")
		}
		return err
	}
	return nil
}

func (r *SandboxReconciler) update(ctx context.Context, sandbox *model.Sandbox, generation int64) error {
	if err := r.store.UpdateSandbox(ctx, sandbox, store.WithGeneration(generation)); err != nil {
		if errors.Is(err, store.ErrGenerationConflict) {
			return reconcile.Superseded("sandbox generation changed")
		}
		return err
	}
	return nil
}

const defaultSandboxStopTimeout = 30 * time.Second

var (
	ErrNoSandboxCapacity = errors.New("no sandbox capacity available")
	ErrNoWorkerCapacity  = ErrNoSandboxCapacity
)

type SandboxAuthenticator interface {
	EnsureTrustKey(ctx context.Context, projectID, userID string) (string, error)
	CreateToken(ctx context.Context, claims sandboxauth.TokenClaims) (string, error)
}

func (r *SandboxReconciler) startSandbox(ctx context.Context, sb *model.Sandbox) error {
	provider, err := r.resolveProvider(ctx, sb)
	if err != nil {
		return err
	}
	if provider == nil {
		now := time.Now().UTC()
		sb.LastActiveAt = &now
		return nil
	}
	secretState, err := r.store.OpenSandboxSecretState(ctx, sb)
	if err != nil {
		return err
	}
	secretState, err = r.ensureSandboxCreated(ctx, sb, provider, secretState)
	if err != nil {
		return err
	}
	if awaitingSourcePush(sb) {
		if len(secretState) > 0 {
			sb.SecretState = secretState
		}
		return parkForSourcePush(sb)
	}
	runtimeSandbox, state, err := provider.Start(ctx, sandboxRefFromSandbox(sb), secretState)
	if err != nil && !errors.Is(err, ErrAlreadyRunning) {
		return err
	}
	if len(state) > 0 || secretState != nil {
		sb.SecretState = state
	}
	if runtimeSandbox != nil {
		setRuntimeState(sb, runtimeSandbox)
	}
	now := time.Now().UTC()
	sb.LastActiveAt = &now
	return nil
}

func (r *SandboxReconciler) ensureSandboxCreated(ctx context.Context, sb *model.Sandbox, provider Provider, secretState []byte) ([]byte, error) {
	ref := sandboxRefFromSandbox(sb)
	createOpts := r.createOptionsFromSandbox(ctx, sb)
	if err := r.applyTrustKey(ctx, sb, &createOpts); err != nil {
		return secretState, err
	}
	runtimeSandbox, state, err := provider.Create(ctx, ref, secretState, createOpts)
	if err != nil && !errors.Is(err, ErrAlreadyExists) {
		return secretState, err
	}
	if len(state) > 0 || secretState != nil {
		sb.SecretState = state
		secretState = state
	}
	if runtimeSandbox != nil {
		setRuntimeState(sb, runtimeSandbox)
	}
	return secretState, nil
}

func (r *SandboxReconciler) stopSandbox(ctx context.Context, sb *model.Sandbox) error {
	provider, err := r.resolveProvider(ctx, sb)
	if err != nil {
		return err
	}
	if provider == nil {
		return nil
	}
	secretState, err := r.store.OpenSandboxSecretState(ctx, sb)
	if err != nil {
		return err
	}
	runtimeSandbox, state, err := provider.Stop(ctx, sandboxRefFromSandbox(sb), secretState, defaultSandboxStopTimeout)
	if errors.Is(err, ErrNotFound) {
		secretState, err = r.ensureSandboxCreated(ctx, sb, provider, secretState)
		if err != nil {
			return err
		}
		runtimeSandbox, state, err = provider.Stop(ctx, sandboxRefFromSandbox(sb), secretState, defaultSandboxStopTimeout)
	}
	if err != nil && !errors.Is(err, ErrNotRunning) {
		return err
	}
	if len(state) > 0 || secretState != nil {
		sb.SecretState = state
	}
	if runtimeSandbox != nil {
		setRuntimeState(sb, runtimeSandbox)
	}
	return nil
}

func (r *SandboxReconciler) deleteSandbox(ctx context.Context, sb *model.Sandbox) error {
	provider, err := r.resolveProvider(ctx, sb)
	if err != nil {
		return err
	}
	if provider == nil {
		return nil
	}
	secretState, err := r.store.OpenSandboxSecretState(ctx, sb)
	if err != nil {
		return err
	}
	state, err := provider.Remove(ctx, sandboxRefFromSandbox(sb), secretState, RemoveVolumes())
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	sb.SecretState = state
	if len(state) == 0 {
		sb.RuntimeState = nil
	}
	return nil
}

func (r *SandboxReconciler) resolveProvider(ctx context.Context, sb *model.Sandbox) (Provider, error) {
	if r == nil {
		return nil, nil
	}
	if r.manager != nil {
		return r.manager.ResolveForSandbox(ctx, sb)
	}
	return r.provider, nil
}

func (r *SandboxReconciler) applyTrustKey(ctx context.Context, sb *model.Sandbox, opts *CreateOptions) error {
	if r == nil || r.auth == nil {
		return nil
	}
	trustKey, err := r.auth.EnsureTrustKey(ctx, sb.ProjectID, sb.CreatedByUserID)
	if err != nil {
		return err
	}
	if trustKey == "" {
		return nil
	}
	if opts.Env == nil {
		opts.Env = map[string]string{}
	}
	opts.Env["DISCOBOX_TRUST_KEY"] = trustKey
	return nil
}

func sandboxRefFromSandbox(sb *model.Sandbox) SandboxRef {
	return SandboxRef{
		SandboxID: sb.ID,
		ProjectID: sb.ProjectID,
	}
}

func (r *SandboxReconciler) createOptionsFromSandbox(ctx context.Context, sb *model.Sandbox) CreateOptions {
	opts := CreateOptions{
		Labels: map[string]string{
			"discobox.project_id": sb.ProjectID,
			"discobox.sandbox_id": sb.ID,
		},
	}
	opts.Image = ImageRef{Name: sb.Image, Digest: sb.ImageDigest}
	opts.PoolID = sb.PoolID
	opts.Name = sb.Name
	opts.Description = sb.Description
	opts.HarnessConfigID = sb.HarnessConfigID
	opts.HarnessMode = sb.HarnessMode
	opts.Model = sb.Model
	opts.ModelServiceTier = sb.ModelServiceTier
	opts.ModelReasoningLevel = sb.ModelReasoningLevel
	opts.Prompt = sb.Prompt
	if sb.Env != nil {
		opts.Env = make(map[string]string, len(sb.Env))
		for key, value := range sb.Env {
			opts.Env[key] = value
		}
	}
	if r.store != nil {
		if assignments, err := r.store.ListSandboxSecrets(ctx, sb.ProjectID, sb.ID); err == nil {
			for _, assignment := range assignments {
				opts.Sentinels = append(opts.Sentinels, assignment.Sentinel)
				if opts.SecretEnv == nil {
					opts.SecretEnv = map[string]string{}
				}
				opts.SecretEnv[assignment.EnvName] = assignment.Sentinel
			}
		}
	}
	opts.CPUVCPUs = sb.CPUVCPUs
	opts.MemoryBytes = sb.MemoryBytes
	opts.StorageBytes = sb.StorageBytes
	opts.Source = sb.Source
	opts.SourceCodeReferences = sb.SourceCodeReferences
	opts.UserName = sb.UserName
	opts.UserUID = sb.UserUID
	opts.UserGID = sb.UserGID
	opts.HomeDirectory = sb.HomeDirectory
	if sb.HarnessConfigID != nil && r.store != nil {
		if cfg, err := r.store.GetHarnessConfig(ctx, sb.ProjectID, *sb.HarnessConfigID); err == nil {
			opts.ResolvedHarnessConfig = &ResolvedHarnessConfig{
				ID:               cfg.ID,
				Name:             cfg.Name,
				RunCommand:       cfg.RunCommand,
				RelaunchCommand:  cfg.RelaunchCommand,
				ConfigCommand:    cfg.ConfigCommand,
				Files:            cfg.Files,
				Env:              cfg.Env,
				Volumes:          cfg.Volumes,
				AdditionalGroups: cfg.AdditionalGroups,
			}
		}
	}
	return opts
}

func setRuntimeState(sb *model.Sandbox, runtimeSandbox *Sandbox) {
	data, err := json.Marshal(runtimeSandbox)
	if err != nil {
		return
	}
	sb.RuntimeState = data
}
