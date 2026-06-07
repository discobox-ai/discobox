package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/obot-platform/disco2/internal/model"
	sandboxruntime "github.com/obot-platform/disco2/internal/sandbox"
)

const defaultSandboxStopTimeout = 30 * time.Second

type SandboxAuthenticator interface {
	EnsureTrustKey(ctx context.Context, projectID, userID string) (string, error)
	CreateToken(ctx context.Context, projectID, userID string) (string, error)
}

// SandboxOperations contains the provider-facing mechanics for sandboxes.
type SandboxOperations struct {
	provider sandboxruntime.Provider
	manager  *sandboxruntime.ProviderManager
	auth     SandboxAuthenticator
}

// SandboxOperationsOption configures SandboxOperations.
type SandboxOperationsOption func(*SandboxOperations)

func NewSandboxOperations(options ...SandboxOperationsOption) *SandboxOperations {
	operations := &SandboxOperations{}
	for _, option := range options {
		if option != nil {
			option(operations)
		}
	}
	return operations
}

// WithSandboxProvider uses a single provider for all sandbox operations.
func WithSandboxProvider(provider sandboxruntime.Provider) SandboxOperationsOption {
	return func(operations *SandboxOperations) {
		operations.provider = provider
	}
}

// WithSandboxProviderManager resolves providers through a manager.
func WithSandboxProviderManager(manager *sandboxruntime.ProviderManager) SandboxOperationsOption {
	return func(operations *SandboxOperations) {
		operations.manager = manager
	}
}

// WithSandboxAuthenticator injects trust-key authentication into sandbox
// creation. When configured, sandbox starts ensure the creating user has a
// public trust key and pass it to the runtime as DISCO2_TRUST_KEY.
func WithSandboxAuthenticator(auth SandboxAuthenticator) SandboxOperationsOption {
	return func(operations *SandboxOperations) {
		operations.auth = auth
	}
}

func (o *SandboxOperations) Start(ctx context.Context, sb *model.Sandbox) error {
	provider, err := o.resolveProvider(ctx, sb)
	if err != nil {
		return err
	}
	if provider == nil {
		now := time.Now().UTC()
		sb.LastActiveAt = &now
		return nil
	}

	ref := sandboxRefFromSandbox(sb)
	createOpts := createOptionsFromSandbox(sb)
	if err := o.applyTrustKey(ctx, sb, &createOpts); err != nil {
		return err
	}
	if err := ensureSandboxImage(ctx, provider, &createOpts); err != nil {
		return err
	}

	runtimeSandbox, state, err := provider.Create(ctx, ref, sb.SecretState, createOpts)
	if err != nil && !errors.Is(err, sandboxruntime.ErrAlreadyExists) {
		return err
	}
	if len(state) > 0 || sb.SecretState != nil {
		sb.SecretState = state
	}
	if runtimeSandbox != nil {
		setRuntimeState(sb, runtimeSandbox)
	}

	runtimeSandbox, state, err = provider.Start(ctx, ref, sb.SecretState)
	if err != nil && !errors.Is(err, sandboxruntime.ErrAlreadyRunning) {
		return err
	}
	if len(state) > 0 || sb.SecretState != nil {
		sb.SecretState = state
	}
	if runtimeSandbox != nil {
		setRuntimeState(sb, runtimeSandbox)
	}
	now := time.Now().UTC()
	sb.LastActiveAt = &now
	return nil
}

func (o *SandboxOperations) Stop(ctx context.Context, sb *model.Sandbox) error {
	provider, err := o.resolveProvider(ctx, sb)
	if err != nil {
		return err
	}
	if provider == nil {
		return nil
	}
	runtimeSandbox, state, err := provider.Stop(ctx, sandboxRefFromSandbox(sb), sb.SecretState, defaultSandboxStopTimeout)
	if err != nil && !errors.Is(err, sandboxruntime.ErrNotFound) && !errors.Is(err, sandboxruntime.ErrNotRunning) {
		return err
	}
	if len(state) > 0 || sb.SecretState != nil {
		sb.SecretState = state
	}
	if runtimeSandbox != nil {
		setRuntimeState(sb, runtimeSandbox)
	}
	return nil
}

func (o *SandboxOperations) Delete(ctx context.Context, sb *model.Sandbox) error {
	provider, err := o.resolveProvider(ctx, sb)
	if err != nil {
		return err
	}
	if provider == nil {
		return nil
	}
	state, err := provider.Remove(ctx, sandboxRefFromSandbox(sb), sb.SecretState, sandboxruntime.RemoveVolumes())
	if err != nil && !errors.Is(err, sandboxruntime.ErrNotFound) {
		return err
	}
	sb.SecretState = state
	if len(state) == 0 {
		sb.RuntimeState = nil
	}
	return nil
}

func (o *SandboxOperations) resolveProvider(ctx context.Context, sb *model.Sandbox) (sandboxruntime.Provider, error) {
	if o == nil {
		return nil, nil
	}
	if o.manager != nil {
		if len(o.manager.ListProviders()) == 0 {
			return nil, nil
		}
		return o.manager.ResolveForSandbox(ctx, sb)
	}
	return o.provider, nil
}

func (o *SandboxOperations) applyTrustKey(ctx context.Context, sb *model.Sandbox, opts *sandboxruntime.CreateOptions) error {
	if o == nil || o.auth == nil {
		return nil
	}
	trustKey, err := o.auth.EnsureTrustKey(ctx, sb.ProjectID, sb.CreatedByUserID)
	if err != nil {
		return err
	}
	if trustKey == "" {
		return nil
	}
	if opts.Env == nil {
		opts.Env = map[string]string{}
	}
	opts.Env["DISCO2_TRUST_KEY"] = trustKey
	return nil
}

func sandboxRefFromSandbox(sb *model.Sandbox) sandboxruntime.SandboxRef {
	return sandboxruntime.SandboxRef{
		SandboxID: sb.ID,
		ProjectID: sb.ProjectID,
	}
}

func ensureSandboxImage(ctx context.Context, provider sandboxruntime.Provider, opts *sandboxruntime.CreateOptions) error {
	imageProvider, ok := provider.(sandboxruntime.ImageProvider)
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
		if event.Status == sandboxruntime.ImageStatusFailed {
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

func createOptionsFromSandbox(sb *model.Sandbox) sandboxruntime.CreateOptions {
	opts := sandboxruntime.CreateOptions{
		Labels: map[string]string{
			"disco2.project_id": sb.ProjectID,
			"disco2.sandbox_id": sb.ID,
		},
	}
	if sb.ProviderInstanceID != nil {
		opts.ProviderInstanceID = *sb.ProviderInstanceID
	}
	if sb.SourceURL != nil {
		opts.WorkspaceSource = *sb.SourceURL
	}
	if sb.SourceRef != nil {
		opts.WorkspaceRef = *sb.SourceRef
	}
	if sb.WorkingDirectory != nil {
		opts.WorkingDirectory = *sb.WorkingDirectory
	}
	return opts
}

func setRuntimeState(sb *model.Sandbox, runtimeSandbox *sandboxruntime.Sandbox) {
	data, err := json.Marshal(runtimeSandbox)
	if err != nil {
		return
	}
	sb.RuntimeState = data
}
