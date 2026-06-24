package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/obot-platform/discobox/orchestration"
	sandboxauth "github.com/obot-platform/discobox/server/internal/auth/sandbox"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/store"
)

type SandboxReconcileExecutor struct {
	store    *store.Store
	provider Provider
	manager  *ProviderManager
	auth     SandboxAuthenticator
}

// SandboxReconcileExecutorOption configures a sandbox executor.
type SandboxReconcileExecutorOption func(*SandboxReconcileExecutor)

func NewSandboxReconcileExecutor(store *store.Store, options ...SandboxReconcileExecutorOption) *SandboxReconcileExecutor {
	executor := &SandboxReconcileExecutor{
		store: store,
	}
	for _, option := range options {
		if option != nil {
			option(executor)
		}
	}
	return executor
}

// WithSandboxProvider uses a single provider for all sandbox reconciliation.
func WithSandboxProvider(provider Provider) SandboxReconcileExecutorOption {
	return func(executor *SandboxReconcileExecutor) {
		executor.provider = provider
	}
}

// WithSandboxProviderManager resolves providers through a manager.
func WithSandboxProviderManager(manager *ProviderManager) SandboxReconcileExecutorOption {
	return func(executor *SandboxReconcileExecutor) {
		executor.manager = manager
	}
}

// WithSandboxAuthenticator injects trust-key authentication into sandbox
// creation. When configured, sandbox starts ensure the creating user has a
// public trust key and pass it to the runtime as DISCOBOX_TRUST_KEY.
func WithSandboxAuthenticator(auth SandboxAuthenticator) SandboxReconcileExecutorOption {
	return func(executor *SandboxReconcileExecutor) {
		executor.auth = auth
	}
}

func (r *SandboxReconcileExecutor) AssertSandboxGeneration(ctx context.Context, projectID, sandboxID string, generation int64) error {
	if _, err := r.store.GetSandbox(ctx, projectID, sandboxID, store.WithGeneration(generation)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if errors.Is(err, store.ErrGenerationConflict) {
			return orchestration.Superseded("sandbox generation changed")
		}
		return err
	}
	return nil
}

func (r *SandboxReconcileExecutor) ReconcileSandboxJob(ctx context.Context, projectID, sandboxID, jobID string, generation int64) error {
	sandbox, err := r.store.GetSandbox(ctx, projectID, sandboxID, store.WithGeneration(generation))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if errors.Is(err, store.ErrGenerationConflict) {
		return orchestration.Superseded("sandbox generation changed")
	}
	if err != nil {
		return err
	}

	sandbox.LastJobID = &jobID

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

func (r *SandboxReconcileExecutor) start(ctx context.Context, sandbox *model.Sandbox, generation int64) error {
	if sandbox.Phase == model.SandboxPhaseRunning && sandbox.ObservedGeneration == generation && sandbox.LastOperationStatus == model.SandboxOperationStatusSuccess {
		return nil
	}

	status := "starting sandbox"
	sandbox.MarkOperationRunning(&status)
	if err := r.update(ctx, sandbox, generation); err != nil {
		return err
	}
	if err := r.startSandbox(ctx, sandbox); err != nil {
		if errors.Is(err, ErrNoSandboxCapacity) {
			sandbox.FailOperation(err.Error())
			if updateErr := r.update(ctx, sandbox, generation); updateErr != nil {
				return updateErr
			}
			return err
		}
		return err
	}
	sandbox.ObservedGeneration = generation
	sandbox.CompleteOperation(model.SandboxPhaseRunning, nil)
	return r.update(ctx, sandbox, generation)
}

func (r *SandboxReconcileExecutor) restart(ctx context.Context, sandbox *model.Sandbox, generation int64) error {
	status := "restarting sandbox"
	sandbox.MarkOperationRunning(&status)
	if err := r.update(ctx, sandbox, generation); err != nil {
		return err
	}
	if err := r.stopSandbox(ctx, sandbox); err != nil {
		return err
	}
	if err := r.startSandbox(ctx, sandbox); err != nil {
		if errors.Is(err, ErrNoSandboxCapacity) {
			sandbox.FailOperation(err.Error())
			if updateErr := r.update(ctx, sandbox, generation); updateErr != nil {
				return updateErr
			}
			return err
		}
		return err
	}
	sandbox.RestartedGeneration = sandbox.RestartGeneration
	sandbox.ObservedGeneration = generation
	sandbox.CompleteOperation(model.SandboxPhaseRunning, nil)
	return r.update(ctx, sandbox, generation)
}

func (r *SandboxReconcileExecutor) stop(ctx context.Context, sandbox *model.Sandbox, generation int64) error {
	if sandbox.Phase == model.SandboxPhaseStopped && sandbox.ObservedGeneration == generation && sandbox.LastOperationStatus == model.SandboxOperationStatusSuccess {
		return nil
	}

	status := "stopping sandbox"
	sandbox.MarkOperationRunning(&status)
	if err := r.update(ctx, sandbox, generation); err != nil {
		return err
	}
	if err := r.stopSandbox(ctx, sandbox); err != nil {
		return err
	}
	sandbox.ObservedGeneration = generation
	sandbox.CompleteOperation(model.SandboxPhaseStopped, nil)
	return r.update(ctx, sandbox, generation)
}

func (r *SandboxReconcileExecutor) delete(ctx context.Context, sandbox *model.Sandbox, generation int64) error {
	if sandbox.Phase == model.SandboxPhaseDeleted && sandbox.ObservedGeneration == generation && sandbox.LastOperationStatus == model.SandboxOperationStatusSuccess {
		return nil
	}

	status := "deleting sandbox"
	sandbox.MarkOperationRunning(&status)
	if err := r.update(ctx, sandbox, generation); err != nil {
		return err
	}
	if err := r.deleteSandbox(ctx, sandbox); err != nil {
		return err
	}
	sandbox.ObservedGeneration = generation
	sandbox.CompleteOperation(model.SandboxPhaseDeleted, nil)
	return r.update(ctx, sandbox, generation)
}

func (r *SandboxReconcileExecutor) update(ctx context.Context, sandbox *model.Sandbox, generation int64) error {
	if err := r.store.UpdateSandbox(ctx, sandbox, store.WithGeneration(generation)); err != nil {
		if errors.Is(err, store.ErrGenerationConflict) {
			return orchestration.Superseded("sandbox generation changed")
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

func (r *SandboxReconcileExecutor) startSandbox(ctx context.Context, sb *model.Sandbox) error {
	provider, err := r.resolveProvider(ctx, sb)
	if err != nil {
		return err
	}
	if provider == nil {
		now := time.Now().UTC()
		sb.LastActiveAt = &now
		return nil
	}
	ref := sandboxRefFromSandbox(sb)
	createOpts := r.createOptionsFromSandbox(sb)
	if err := r.applyTrustKey(ctx, sb, &createOpts); err != nil {
		return err
	}
	if err := ensureSandboxImage(ctx, provider, &createOpts); err != nil {
		return err
	}

	secretState, err := r.store.OpenSandboxSecretState(ctx, sb)
	if err != nil {
		return err
	}
	runtimeSandbox, state, err := provider.Create(ctx, ref, secretState, createOpts)
	if err != nil && !errors.Is(err, ErrAlreadyExists) {
		return err
	}
	if len(state) > 0 || secretState != nil {
		sb.SecretState = state
		secretState = state
	}
	if runtimeSandbox != nil {
		setRuntimeState(sb, runtimeSandbox)
		setWorkerID(sb, runtimeSandbox)
	}

	runtimeSandbox, state, err = provider.Start(ctx, ref, secretState)
	if err != nil && !errors.Is(err, ErrAlreadyRunning) {
		return err
	}
	if len(state) > 0 || secretState != nil {
		sb.SecretState = state
	}
	if runtimeSandbox != nil {
		setRuntimeState(sb, runtimeSandbox)
		setWorkerID(sb, runtimeSandbox)
	}
	now := time.Now().UTC()
	sb.LastActiveAt = &now
	return nil
}

func (r *SandboxReconcileExecutor) stopSandbox(ctx context.Context, sb *model.Sandbox) error {
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
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrNotRunning) {
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

func (r *SandboxReconcileExecutor) deleteSandbox(ctx context.Context, sb *model.Sandbox) error {
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
		sb.WorkerID = nil
	}
	return nil
}

func (r *SandboxReconcileExecutor) resolveProvider(ctx context.Context, sb *model.Sandbox) (Provider, error) {
	if r == nil {
		return nil, nil
	}
	if r.manager != nil {
		return r.manager.ResolveForSandbox(ctx, sb)
	}
	return r.provider, nil
}

func (r *SandboxReconcileExecutor) applyTrustKey(ctx context.Context, sb *model.Sandbox, opts *CreateOptions) error {
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

func ensureSandboxImage(ctx context.Context, provider Provider, opts *CreateOptions) error {
	imageProvider, ok := provider.(ImageProvider)
	if !ok {
		return nil
	}
	if opts.Image.Name == "" {
		image, err := imageProvider.DefaultImage(ctx)
		if err != nil {
			return err
		}
		opts.Image = image
	}
	exists, err := imageProvider.ImageExists(ctx, opts.Image)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	events, err := imageProvider.PullImage(ctx, opts.Image)
	if err != nil {
		return err
	}
	var lastErr string
	for event := range events {
		if event.Error != "" {
			lastErr = event.Error
		}
		if event.Status == ImageStatusFailed {
			if lastErr == "" {
				lastErr = "image pull failed"
			}
			return errors.New(lastErr)
		}
	}
	exists, err = imageProvider.ImageExists(ctx, opts.Image)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("image pull completed but image is unavailable")
	}
	return nil
}

func (r *SandboxReconcileExecutor) createOptionsFromSandbox(sb *model.Sandbox) CreateOptions {
	opts := CreateOptions{
		Labels: map[string]string{
			"discobox.project_id": sb.ProjectID,
			"discobox.sandbox_id": sb.ID,
		},
	}
	opts.Image = ImageRef{Name: sb.Image}
	if sb.ProviderInstanceID != nil {
		opts.ProviderInstanceID = *sb.ProviderInstanceID
	}
	if sb.WorkerID != nil {
		opts.WorkerID = *sb.WorkerID
	}
	opts.Name = sb.Name
	opts.Description = sb.Description
	opts.AgentConfigID = sb.AgentConfigID
	opts.AgentModel = sb.AgentModel
	opts.AgentModelServiceTier = sb.AgentModelServiceTier
	opts.AgentModelReasoningLevel = sb.AgentModelReasoningLevel
	opts.Prompt = sb.Prompt
	opts.CPUVCPUs = sb.CPUVCPUs
	opts.MemoryBytes = sb.MemoryBytes
	opts.StorageBytes = sb.StorageBytes
	opts.Source = sb.Source
	opts.SourceCodeReferences = sb.SourceCodeReferences
	opts.UserName = sb.UserName
	opts.UserUID = sb.UserUID
	opts.UserGID = sb.UserGID
	opts.HomeDirectory = sb.HomeDirectory
	return opts
}

func setWorkerID(sb *model.Sandbox, runtimeSandbox *Sandbox) {
	if runtimeSandbox == nil || runtimeSandbox.Metadata == nil {
		return
	}
	if workerID := runtimeSandbox.Metadata["worker_id"]; workerID != "" {
		sb.WorkerID = &workerID
	}
}

func setRuntimeState(sb *model.Sandbox, runtimeSandbox *Sandbox) {
	data, err := json.Marshal(runtimeSandbox)
	if err != nil {
		return
	}
	sb.RuntimeState = data
}
