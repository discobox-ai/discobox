package sandboxes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	sandboxauth "github.com/discobox-ai/discobox/server/internal/auth/sandbox"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/store"
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
func (r *SandboxReconciler) Reconcile(ctx context.Context, id string) (reconcile.Result, error) {
	projectID, sandboxID, err := splitSandboxDirtyID(id)
	if err != nil {
		return reconcile.Result{}, err
	}
	sandbox, err := r.store.GetSandbox(ctx, projectID, sandboxID)
	if errors.Is(err, store.ErrNotFound) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}

	result, err := r.ReconcileSandbox(ctx, sandbox)
	if errors.Is(err, reconcile.ErrSuperseded) {
		// Superseded: the newer intent's mark re-runs us, and it is the run that
		// gets to arm a timer against the state it actually read.
		return reconcile.Result{}, nil
	}
	if err == nil {
		return result, nil
	}
	if current, gerr := r.store.GetSandbox(ctx, projectID, sandboxID); gerr == nil &&
		current.ErrorMessage != nil && current.Converged() {
		return reconcile.Result{}, nil // failure recorded on the resource: converged until new intent
	}
	return reconcile.Result{}, err
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
	expired, err := r.store.ListArchivedSandboxRefsExpiredBefore(ctx, r.serverArchiveRetention(), time.Now().UTC())
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
	// archiveRetention is the server-wide default a project follows until it
	// sets its own. Zero means unconfigured, not "purge immediately", so the
	// package default applies; see serverArchiveRetention.
	archiveRetention time.Duration
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

// WithArchiveRetention sets the server-wide default retention for archived
// sandboxes, which every project that has not set its own follows. A project
// setting still wins: this is the default it defers to, not a ceiling.
//
// It exists so a development server can hold archived sandboxes for minutes
// rather than a day — a development tree is as large as a production one and is
// discarded far more often — without writing that shorter window into any
// project, which is a setting the developer would then carry into production.
func WithArchiveRetention(retention time.Duration) SandboxReconcilerOption {
	return func(reconciler *SandboxReconciler) {
		reconciler.archiveRetention = retention
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
func (r *SandboxReconciler) ReconcileSandbox(ctx context.Context, sandbox *model.Sandbox) (reconcile.Result, error) {
	generation := sandbox.Generation
	switch sandbox.DesiredState {
	case model.DesiredStatePresent:
		return r.ensure(ctx, sandbox, generation)
	case model.DesiredStateArchived:
		return r.archive(ctx, sandbox, generation)
	case model.DesiredStateDeleted:
		return reconcile.Result{}, r.delete(ctx, sandbox, generation)
	default:
		return reconcile.Result{}, fmt.Errorf("unsupported sandbox desired state %q", sandbox.DesiredState)
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
func (r *SandboxReconciler) ensure(ctx context.Context, sandbox *model.Sandbox, generation int64) (reconcile.Result, error) {
	// A settled failure is converged by design and needs new intent, not another
	// attempt (ADR 0017 §4). Anything else gets the idempotent ensure below,
	// because a dirty mark can come from an observation as well as from intent —
	// "your container is gone" is the case that matters, and it arrives with the
	// generations already in agreement.
	if sandbox.Converged() && sandbox.ErrorMessage != nil {
		return reconcile.Result{}, nil
	}

	firstCreate := sandboxHasNeverRun(sandbox.State)
	// A repair rides exactly this generation (ADR 0035): tear the runtime down
	// first — container and disposable pool-host state dropped, durable tree
	// kept — so the create below rebuilds from the retained tree instead of
	// adopting whatever broken container or stale material survived. The
	// teardown is the same provider Archive an archive uses, and it is
	// idempotent, so a retry within this generation is safe; a later
	// generation no longer matches and never tears down again.
	//
	// The teardown belongs to the repair that has not landed yet, which is why
	// the generation alone does not select it. Ensure also runs on observation
	// — "your container is gone" arrives with the generations already in
	// agreement (above) — and on a settled repair generation that reads as a
	// retry of a repair which already finished. Tearing down there re-archived
	// a healthy sandbox on every attach, and left it settled as archived with
	// its row still reading `ready`.
	if sandbox.RepairGeneration == generation && !sandbox.Converged() && !firstCreate {
		if err := r.archiveSandbox(ctx, sandbox); err != nil {
			sandbox.ObservedGeneration = generation
			sandbox.RecordFailure(model.SandboxStateFailed, err.Error())
			if updateErr := r.update(ctx, sandbox, generation); updateErr != nil {
				return reconcile.Result{}, updateErr
			}
			return reconcile.Result{}, err
		}
	}
	if err := r.createSandbox(ctx, sandbox, firstCreate); err != nil {
		sandbox.ObservedGeneration = generation
		sandbox.RecordFailure(model.SandboxStateFailed, err.Error())
		if updateErr := r.update(ctx, sandbox, generation); updateErr != nil {
			return reconcile.Result{}, updateErr
		}
		return reconcile.Result{}, err
	}

	// Parked waiting for the client's push. The generation is fully handled —
	// there is nothing further to do until the client acts — so record it as
	// observed and arm the give-up timer off StateChangedAt.
	//
	// The question is whether the push is still outstanding, not what state the
	// sandbox is in: `awaiting_source` is where the resume starts from, and
	// createSandbox above leaves it in place rather than clearing it, so keying
	// on the state re-parked the sandbox it had just materialized the source
	// into. It then sat at `awaiting_source` forever — running, with its source
	// in place, and reported as still provisioning.
	if awaitingSourcePush(sandbox) {
		sandbox.ObservedGeneration = generation
		if err := r.update(ctx, sandbox, generation); err != nil {
			return reconcile.Result{}, err
		}
		return armSourceAwaitTimeout(sandbox), nil
	}

	// The container exists and matches the spec, which is the whole of what this
	// reconciler converges. `ready` says exactly that and nothing about power —
	// whether the container is running is the pool agent's to report, and it
	// already has: the create it just performed reported what it observed
	// before returning (ADR 0034 §4), so an unarchive that rebuilt a stopped
	// container reads `stopped` by the time this lands, without waiting for a
	// complete sync.
	sandbox.SetState(model.SandboxStateReady)
	sandbox.ObservedGeneration = generation
	sandbox.ErrorMessage = nil
	if err := r.update(ctx, sandbox, generation); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
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
	// Repair the sandbox's secret bindings on the way up. The fan-out when a
	// binding changes reaches the sandboxes that exist and are reachable at that
	// moment; this catches what it missed — a sandbox archived across the change,
	// a fan-out that stopped on an earlier sandbox's error — before the create
	// options below read the assignments. It only covers a sandbox being
	// reconciled: one whose generations already agree is never handed here, so
	// drift on an idle sandbox waits for its next start.
	if _, err := rebindSandboxSecretRows(ctx, r.store, sb.ProjectID, sb); err != nil {
		return secretState, fmt.Errorf("rebind sandbox secrets: %w", err)
	}
	createOpts, err := r.createOptionsFromSandbox(ctx, sb)
	if err != nil {
		return secretState, err
	}
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
		setProviderState(sb, runtimeSandbox)
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
		sb.ProviderState = nil
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

func (r *SandboxReconciler) createOptionsFromSandbox(ctx context.Context, sb *model.Sandbox) (CreateOptions, error) {
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
		// A failed read must fail the reconcile, not degrade to "no secrets":
		// a sandbox launched without its assignments never gets them (the
		// fingerprint excludes assignments, so a later reconcile sees no
		// drift), while a returned error just retries the reconcile.
		assignments, err := r.store.ListInjectedSandboxSecrets(ctx, sb.ProjectID, sb.ID)
		if err != nil {
			return CreateOptions{}, fmt.Errorf("list sandbox secret assignments: %w", err)
		}
		for _, assignment := range assignments {
			opts.Sentinels = append(opts.Sentinels, assignment.Sentinel)
			if opts.SecretEnv == nil {
				opts.SecretEnv = map[string]string{}
			}
			opts.SecretEnv[assignment.EnvName] = assignment.Sentinel
		}
	}
	opts.Source = sb.Source
	opts.SourceCodeReferences = sb.SourceCodeReferences
	if sb.Origin != nil {
		opts.SourceDataKey = sourceDataKey(sb.Origin.HostID, sb.Source)
		if len(sb.SourceCodeReferences) > 0 {
			opts.SourceCodeReferenceDataKeys = make(map[string]string, len(sb.SourceCodeReferences))
			for name, source := range sb.SourceCodeReferences {
				if key := sourceDataKey(sb.Origin.HostID, &source); key != "" {
					opts.SourceCodeReferenceDataKeys[name] = key
				}
			}
		}
	}
	opts.SpecFingerprint = sourceDataFingerprint(opts.SpecFingerprint, opts.SourceDataKey, opts.SourceCodeReferenceDataKeys)
	opts.UserName = sb.UserName
	opts.UserUID = sb.UserUID
	opts.UserGID = sb.UserGID
	opts.UserGroupName = sb.UserGroupName
	opts.UserAdditionalGroups = append([]string(nil), sb.UserAdditionalGroups...)
	opts.HomeDirectory = sb.HomeDirectory
	opts.GitUserName = sb.GitUserName
	opts.GitUserEmail = sb.GitUserEmail
	if sb.HarnessConfigID != nil && r.store != nil {
		// A load failure is not swallowed: the pool agent refuses a create with
		// no resolved harness config, so quietly omitting one would turn a
		// missing config row into an agent-side error about a request this side
		// built wrong.
		cfg, err := r.store.GetHarnessConfig(ctx, sb.ProjectID, *sb.HarnessConfigID)
		if err != nil {
			return CreateOptions{}, fmt.Errorf("resolve harness config %s for sandbox %s: %w", *sb.HarnessConfigID, sb.ID, err)
		}
		{
			opts.ResolvedHarnessConfig = &ResolvedHarnessConfig{
				ID:               cfg.ID,
				Name:             cfg.Name,
				RunCommand:       cfg.RunCommand,
				RelaunchCommand:  cfg.RelaunchCommand,
				ConfigCommand:    cfg.ConfigCommand,
				Files:            cfg.Files,
				ConfiguredFiles:  cfg.ConfiguredFiles,
				Secrets:          cfg.Secrets,
				Env:              cfg.Env,
				Volumes:          cfg.Volumes,
				AdditionalGroups: cfg.AdditionalGroups,
			}
		}
	}
	return opts, nil
}

// sourceDataFingerprint makes the source-scoped mounts part of the runtime
// spec. Sandboxes created before source data existed therefore rebuild once
// instead of retaining a container that can never see the new mounts.
func sourceDataFingerprint(base, primary string, refs map[string]string) string {
	if primary == "" && len(refs) == 0 {
		return base
	}
	payload, err := json.Marshal(struct {
		Version int               `json:"version"`
		Base    string            `json:"base"`
		Primary string            `json:"primary,omitempty"`
		Refs    map[string]string `json:"refs,omitempty"`
	}{Version: 1, Base: base, Primary: primary, Refs: refs})
	if err != nil {
		return "unfingerprintable-source-data"
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func setProviderState(sb *model.Sandbox, runtimeSandbox *Sandbox) {
	data, err := json.Marshal(runtimeSandbox)
	if err != nil {
		return
	}
	sb.ProviderState = data
}

// sandboxHasNeverRun reports whether a sandbox has yet to run for the first
// time, which is the one case where creating it also starts it (see ensure).
//
// Pending is the obvious case. Awaiting-source is the same thing interrupted:
// the sandbox was created, parked for its client's push, and has been waiting
// ever since. Resuming it is still its first start.
//
// It reads the existence axis, not the runtime one, and that is the point: a
// sandbox that has reached `ready` has been created once already, so a later
// ensure — a re-pin, a spec change, a rebuild after the container was lost —
// rebuilds without starting, whatever the runtime last reported.
func sandboxHasNeverRun(state string) bool {
	return state == model.SandboxStatePending || state == model.SandboxStateAwaitingSource
}
