package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		current.ErrorMessage != nil && current.Converged() {
		return nil // failure recorded on the resource: converged until new intent
	}
	return err
}

// ScanDirty is the level-triggered backstop: every sandbox whose generations
// disagree is re-marked, whatever it is doing and however long ago the mark was
// lost (ADR 0017 §1). A settled failure has already advanced
// ObservedGeneration, so it is converged by design and this does not re-drive
// it until new intent arrives.
// It has one addition the generation comparison cannot express: archived
// sandboxes past their retention. Those have converged — their generations
// agree, and by design nothing re-drives them — so the deadline is the only
// thing that makes them work again, and the future-dated mark carrying it is a
// mark like any other and can be lost. A lost mark here means data kept
// forever, so retention gets the same backstop everything else has
// (ADR 0022 §4).
func (r *SandboxReconciler) ScanDirty(ctx context.Context) ([]string, error) {
	pairs, err := r.store.ListSandboxRefsNeedingReconcile(ctx)
	if err != nil {
		return nil, err
	}
	expired, err := r.store.ListArchivedSandboxRefsExpiredBefore(ctx, DefaultArchiveRetention, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(pairs)+len(expired))
	seen := make(map[string]struct{}, len(pairs)+len(expired))
	for _, p := range append(pairs, expired...) {
		id := SandboxDirtyID(p.ProjectID, p.SandboxID)
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
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
//
// Desired state answers existence only (ADR 0017 §9). Whether the sandbox is
// running right now is not converged here and is not stored as intent: start,
// stop, and restart are operations the service forwards to the pool agent, and
// the resulting state comes back on the agent's reporting channel.
func (r *SandboxReconciler) ReconcileSandbox(ctx context.Context, sandbox *model.Sandbox) error {
	generation := sandbox.Generation
	switch sandbox.DesiredState {
	case model.DesiredStatePresent:
		return r.ensure(ctx, sandbox, generation)
	case model.DesiredStateArchived:
		return r.archive(ctx, sandbox, generation)
	case model.DesiredStateDeleted:
		return r.delete(ctx, sandbox, generation)
	default:
		return fmt.Errorf("unsupported sandbox desired state %q", sandbox.DesiredState)
	}
}

// ensure brings the sandbox's container into existence, built from the current
// spec. It does not start it.
//
// The one exception is a sandbox that has never run: creating a sandbox means
// asking for one, so the first create issues a start. A later ensure — a
// re-pin, a spec change, or a rebuild after the container was lost — creates
// the container and leaves its power state alone, which is what makes a
// recovered sandbox stay stopped until something uses it (ADR 0017 §13). Where
// that ensure replaces a *running* container, the pool agent restarts it into
// the new spec from its own observation; the start does not come from here
// (ADR 0021 §§3–4).
//
// The spec is taken as it stands. Nothing here advances the image pin: a
// sandbox runs what it is pinned to until an upgrade re-pins it (ADR 0021 §2).
func (r *SandboxReconciler) ensure(ctx context.Context, sandbox *model.Sandbox, generation int64) error {
	// A settled failure is converged by design and needs new intent, not another
	// attempt (ADR 0017 §4). Anything else gets the idempotent ensure below,
	// because a dirty mark can come from an observation as well as from intent —
	// "your container is gone" is the case that matters, and it arrives with the
	// generations already in agreement.
	if sandbox.Converged() && sandbox.ErrorMessage != nil {
		return nil
	}

	firstCreate := sandboxHasNeverRun(sandbox.State)
	if err := r.createSandbox(ctx, sandbox, firstCreate); err != nil {
		sandbox.ObservedGeneration = generation
		sandbox.RecordFailure(model.SandboxStateFailed, err.Error())
		if updateErr := r.update(ctx, sandbox, generation); updateErr != nil {
			return updateErr
		}
		return err
	}

	if sandbox.State == model.SandboxStateAwaitingSource {
		// Parked waiting for the client's push. The generation is fully handled
		// — there is nothing further to do until the client acts — so record it
		// as observed and arm the give-up timer off StateChangedAt.
		sandbox.ObservedGeneration = generation
		if err := r.update(ctx, sandbox, generation); err != nil {
			return err
		}
		return r.scheduleSourceAwaitTimeout(ctx, sandbox)
	}

	if sandbox.State == model.SandboxStateArchived {
		// Unarchiving: the container has just been rebuilt against the retained
		// tree, so the sandbox exists as a runtime again and `archived` is no
		// longer true of it. Recording that here rather than waiting for the
		// agent's next complete sync is not the reconciler holding an opinion
		// about power — it owns existence, and this is the existence change it
		// just made. Leaving it stale would report an archived sandbox that has
		// a container for up to a full sync interval.
		//
		// `stopped` is the honest value: the container exists and is not
		// running, and nothing started it (ADR 0022 §2).
		sandbox.SetState(model.SandboxStateStopped)
	}
	sandbox.ObservedGeneration = generation
	sandbox.ErrorMessage = nil
	if err := r.update(ctx, sandbox, generation); err != nil {
		return err
	}
	return nil
}

// The image pin moves in exactly one place: UpgradeSandbox. There is
// deliberately no reconciler-side re-pin. An implicit one would be a control
// plane rescue for a condition only the pool agent can detect — whether the
// pinned image can actually be run there — and it would fire on reconciles a
// user never asked for (ADR 0021 §§2, 5).

func (r *SandboxReconciler) delete(ctx context.Context, sandbox *model.Sandbox, generation int64) error {
	if sandbox.State == model.SandboxStateDeleted && sandbox.Converged() {
		return r.softDelete(ctx, sandbox, generation)
	}

	if err := r.deleteSandbox(ctx, sandbox); err != nil {
		sandbox.ObservedGeneration = generation
		sandbox.RecordFailure(model.SandboxStateFailed, err.Error())
		if updateErr := r.update(ctx, sandbox, generation); updateErr != nil {
			return updateErr
		}
		return err
	}
	sandbox.ObservedGeneration = generation
	sandbox.SetState(model.SandboxStateDeleted)
	sandbox.ErrorMessage = nil
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

// createSandbox brings the sandbox's container into existence and parks it if
// its source has not arrived yet. It never starts anything: power is not this
// reconciler's business (ADR 0017 §9).
func (r *SandboxReconciler) createSandbox(ctx context.Context, sb *model.Sandbox, start bool) error {
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
	secretState, err = r.ensureSandboxCreated(ctx, sb, provider, secretState, start)
	if err != nil {
		return err
	}
	if awaitingSourcePush(sb) {
		if len(secretState) > 0 {
			sb.SecretState = secretState
		}
		return parkForSourcePush(sb)
	}
	now := time.Now().UTC()
	sb.LastActiveAt = &now
	return nil
}

func (r *SandboxReconciler) ensureSandboxCreated(ctx context.Context, sb *model.Sandbox, provider Provider, secretState []byte, start bool) ([]byte, error) {
	ref := sandboxRefFromSandbox(sb)
	createOpts := r.createOptionsFromSandbox(ctx, sb)
	createOpts.Start = start
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
	// Remove returns only once the provider has confirmed the container and the
	// sandbox's data are both gone (ADR 0022 §3). Everything after this point —
	// the deleted state, and then the row itself — is written on the strength of
	// that confirmation, so an error here must not be swallowed. ErrNotFound is
	// the one exception: a provider that never heard of the sandbox is not
	// holding data for it.
	state, err := provider.Remove(ctx, sandboxRefFromSandbox(sb), secretState)
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
	opts.SpecFingerprint = sb.Fingerprint()
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
	opts.UserGroupName = sb.UserGroupName
	opts.UserAdditionalGroups = append([]string(nil), sb.UserAdditionalGroups...)
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

// sandboxHasNeverRun reports whether a sandbox has yet to run for the first
// time, which is the one case where creating it also starts it (see ensure).
//
// Pending is the obvious case. Awaiting-source is the same thing interrupted:
// the sandbox was created, parked for its client's push, and has been waiting
// ever since. Resuming it is still its first start.
func sandboxHasNeverRun(state string) bool {
	return state == model.SandboxStatePending || state == model.SandboxStateAwaitingSource
}
