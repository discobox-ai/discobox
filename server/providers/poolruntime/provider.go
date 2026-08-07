// Package poolruntime implements the generic pool-backed sandbox provider.
//
// A pool is its own runtime host (ADR-0006): one container, VM, or pod runs
// the pool agent and hosts the pool's sandboxes. This package owns sandbox
// placement gating, capacity waits, bootstrap credential minting, and
// user-sandbox operations through the pool agent API. Runtime mechanics for
// the host itself live behind the RuntimeProvider seam (the dockerworker
// engine).
package poolruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	poolagent "github.com/obot-platform/discobox/pool-agent"
	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
)

const (
	defaultPoolCapacityWaitTimeout  = 30 * time.Second
	defaultPoolCapacityPollInterval = time.Second
)

var (
	poolCapacityWaitTimeout  = defaultPoolCapacityWaitTimeout
	poolCapacityPollInterval = defaultPoolCapacityPollInterval
)

// PoolManager is the control-plane surface a pool provider needs.
type PoolManager = sandbox.PoolManager

// RuntimeProvider owns pool host runtime lifecycle and agent connectivity.
// It is implemented by the dockerworker engine; this package never sees
// Docker or VM details.
type RuntimeProvider interface {
	Close() error
	// EnsurePool creates or drift-corrects the pool's host runtime. It is
	// idempotent and updates the pool row's runtime state and scheduling flags
	// in place; the caller persists the row.
	//
	// mint is called ONLY when a runtime is actually created: minting persists a
	// single-use bootstrap token, and a drift check that finds a healthy
	// container needs no credentials.
	EnsurePool(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool, mint poolagent.MintBootstrap) error
	// RepairPool replaces an unhealthy pool runtime in place, preserving pool
	// identity and pool-local state.
	RepairPool(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool, mint poolagent.MintBootstrap, reason string) error
	RemovePool(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool) error
	// AcquirePoolAgentClient returns an HTTP client lease that reaches the pool
	// agent API for the pool.
	AcquirePoolAgentClient(ctx context.Context, pool *model.Pool) (*transport.HTTPClientLease, error)
}

// Provider is a sandbox provider backed by pool hosts.
type Provider struct {
	runtimeProvider RuntimeProvider
	definition      sandbox.ProviderDefinition
	manager         PoolManager
}

// New creates a pool-backed sandbox provider over a runtime provider.
func New(runtimeProvider RuntimeProvider, definition sandbox.ProviderDefinition, manager PoolManager) *Provider {
	return &Provider{runtimeProvider: runtimeProvider, definition: definition, manager: manager}
}

func (p *Provider) Initialize(ctx context.Context, provider *model.SandboxProviderInstance) error {
	pools, err := p.manager.ListPoolsForProviderInstance(ctx, provider.ProjectID, provider.ID)
	if err != nil {
		return err
	}
	for i := range pools {
		if err := p.manager.SchedulePoolReconciliation(ctx, provider.ProjectID, pools[i].ID); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) List(context.Context) ([]*sandbox.Sandbox, error) {
	return nil, nil
}

func (p *Provider) Close() error {
	return p.runtimeProvider.Close()
}

func (p *Provider) Definition() sandbox.ProviderDefinition {
	return p.definition
}

func (p *Provider) Status() sandbox.ProviderStatus {
	return sandbox.ProviderStatus{Available: true, State: "ready"}
}

func (p *Provider) Reconcile(context.Context) error {
	return nil
}

func (p *Provider) RemoveProject(context.Context, string) error {
	return nil
}

func (p *Provider) ReconcilePool(ctx context.Context, manager sandbox.PoolManager, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool) error {
	if manager == nil {
		return fmt.Errorf("pool manager is required")
	}
	if err := p.runtimeProvider.EnsurePool(ctx, project, provider, pool, mintPoolBootstrap(manager, project, pool)); err != nil {
		return err
	}
	// Best-effort: hand the now-ready pool-agent the authoritative pool set so it
	// reaps any orphaned pools sharing its host daemon. A no-op on isolated
	// per-pool daemons, and never fatal to the reconcile.
	if err := p.syncKnownPools(ctx, manager, provider, pool); err != nil {
		slog.Warn("pool-sync failed", "pool", pool.ID, "error", err)
	}
	return armRegistrationTimeout(ctx, manager, pool)
}

// syncKnownPools sends the pool-agent the full set of pools in this project, so
// it can reclaim any others it observes on a shared host.
//
// The set is project-wide rather than provider-instance-wide because it is the
// authority for a reaper that scans project-scoped host trees. Narrowing it to
// this provider instance would make a sibling instance's live pools look like
// orphans, and the reaper would delete their data and proxy material.
func (p *Provider) syncKnownPools(ctx context.Context, manager sandbox.PoolManager, provider *model.SandboxProviderInstance, pool *model.Pool) error {
	pools, err := manager.ListPools(ctx, provider.ProjectID)
	if err != nil {
		return err
	}
	known := make([]string, 0, len(pools))
	for i := range pools {
		known = append(known, pools[i].ID)
	}
	client, err := p.agentClientForPool(ctx, pool)
	if err != nil {
		return err
	}
	return client.SyncKnownPools(ctx, provider.ProjectID, known)
}

func (p *Provider) RepairPool(ctx context.Context, manager sandbox.PoolManager, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool, reason string) error {
	if manager == nil {
		return fmt.Errorf("pool manager is required")
	}
	if err := p.runtimeProvider.RepairPool(ctx, project, provider, pool, mintPoolBootstrap(manager, project, pool), reason); err != nil {
		return err
	}
	return armRegistrationTimeout(ctx, manager, pool)
}

func (p *Provider) RemovePool(ctx context.Context, _ sandbox.PoolManager, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool) error {
	return p.runtimeProvider.RemovePool(ctx, project, provider, pool)
}

// armRegistrationTimeout schedules the pool re-check that catches a runtime
// that came up but whose agent never registered.
//
// It arms ONLY for a pool that has never registered, because only such a pool
// can time out. Arming it for an already-registered pool is a busy loop: the
// reconcile drift-checks every healthy pool through here, so the timer would
// re-mark the very pool row being reconciled.
func armRegistrationTimeout(ctx context.Context, manager sandbox.PoolManager, pool *model.Pool) error {
	if poolRegistrationTimeout <= 0 || pool.EverCreated() {
		return nil
	}
	return manager.SchedulePoolReconciliationAt(ctx, pool.ProjectID, pool.ID, time.Now().UTC().Add(poolRegistrationTimeout))
}

func (p *Provider) Create(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	if p.manager == nil {
		return nil, nil, fmt.Errorf("pool manager is required")
	}
	if poolID, err := poolIDFromRuntimeState(state); err == nil {
		// An existing runtime already lives on a pool; a state naming a
		// different pool than the sandbox's is unplaceable.
		if opts.PoolID != "" && poolID != opts.PoolID {
			return nil, state, sandbox.ErrNoSandboxCapacity
		}
		pool, err := p.manager.GetPool(ctx, ref.ProjectID, poolID)
		if err != nil {
			return nil, state, err
		}
		return p.createOnPool(ctx, ref, state, opts, pool)
	} else if len(state) > 0 && !errors.Is(err, sandbox.ErrNotFound) {
		return nil, state, err
	}
	sb := &model.Sandbox{
		ID:        ref.SandboxID,
		ProjectID: ref.ProjectID,
		PoolID:    opts.PoolID,
		SandboxManifest: model.SandboxManifest{
			CPUVCPUs:     opts.CPUVCPUs,
			MemoryBytes:  opts.MemoryBytes,
			StorageBytes: opts.StorageBytes,
		},
	}
	pool, err := p.schedulablePool(ctx, sb)
	if err != nil {
		return nil, nil, err
	}
	return p.createOnPool(ctx, ref, nil, opts, pool)
}

func (p *Provider) createOnPool(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts sandbox.CreateOptions, pool *model.Pool) (*sandbox.Sandbox, []byte, error) {
	client, err := p.agentClientForPool(ctx, pool)
	if err != nil {
		return nil, state, err
	}
	return client.Create(ctx, ref, state, opts)
}

// schedulablePool waits for the sandbox's pool to accept placement. It gives
// up early when the pool has settled into failure: a scheduling wait is only
// worth its deadline while the runtime is still on its way up, and a settled
// failure carries a cause the caller should see.
func (p *Provider) schedulablePool(ctx context.Context, sb *model.Sandbox) (*model.Pool, error) {
	pool, err := p.manager.SchedulablePoolForSandbox(ctx, sb)
	if err == nil {
		return pool, nil
	}
	if !errors.Is(err, apperrors.ErrNotFound) {
		return nil, err
	}
	if sb == nil || sb.PoolID == "" {
		return nil, sandbox.ErrNoSandboxCapacity
	}
	// Kick the pool reconcile so a missing or drifted runtime is brought up,
	// then wait for the agent to report schedulable.
	if err := p.manager.SchedulePoolReconciliation(ctx, sb.ProjectID, sb.PoolID); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(poolCapacityWaitTimeout)
	for {
		pool, err := p.manager.SchedulablePoolForSandbox(ctx, sb)
		if err == nil {
			return pool, nil
		}
		if !errors.Is(err, apperrors.ErrNotFound) {
			return nil, err
		}
		if err := p.settledFailure(ctx, sb); err != nil {
			return nil, err
		}
		if poolCapacityWaitTimeout <= 0 || !time.Now().Before(deadline) {
			return nil, sandbox.ErrNoSandboxCapacity
		}
		timer := time.NewTimer(poolCapacityPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// settledFailure returns the pool's failure once nothing can still bring the
// runtime up, and nil while a reconcile is pending or in flight. The error
// carries the pool's recorded message so the cause — a missing image, an
// unreachable daemon — reaches the sandbox.
func (p *Provider) settledFailure(ctx context.Context, sb *model.Sandbox) error {
	if sb == nil || sb.PoolID == "" {
		return nil
	}
	pool, err := p.manager.GetPool(ctx, sb.ProjectID, sb.PoolID)
	if err != nil {
		return err
	}
	if pool.RevokedAt != nil || pool.DesiredState != model.DesiredStatePresent {
		return &sandbox.PoolFailure{PoolID: pool.ID, Message: "pool is not active"}
	}
	// A failure is settled only when the latest intent was attempted and lost;
	// a bumped generation means a retry is pending.
	if pool.ErrorMessage == nil || !pool.Converged() {
		return nil
	}
	message := ""
	if pool.ErrorMessage != nil {
		message = *pool.ErrorMessage
	}
	return &sandbox.PoolFailure{PoolID: pool.ID, Message: message}
}

func (p *Provider) Update(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts sandbox.UpdateOptions) (*sandbox.Sandbox, []byte, error) {
	client, err := p.agentClientFromState(ctx, ref, state)
	if err != nil {
		return nil, state, err
	}
	return client.Update(ctx, ref, state, opts)
}

func (p *Provider) Start(ctx context.Context, ref sandbox.SandboxRef, state []byte) ([]byte, error) {
	client, err := p.agentClientFromState(ctx, ref, state)
	if err != nil {
		return state, err
	}
	return client.Start(ctx, ref, state)
}

func (p *Provider) Stop(ctx context.Context, ref sandbox.SandboxRef, state []byte, timeout time.Duration) ([]byte, error) {
	client, err := p.agentClientFromState(ctx, ref, state)
	if err != nil {
		return state, err
	}
	return client.Stop(ctx, ref, state, timeout)
}

func (p *Provider) Restart(ctx context.Context, ref sandbox.SandboxRef, state []byte, timeout time.Duration) ([]byte, error) {
	client, err := p.agentClientFromState(ctx, ref, state)
	if err != nil {
		return state, err
	}
	return client.Restart(ctx, ref, state, timeout)
}

func (p *Provider) Get(ctx context.Context, ref sandbox.SandboxRef, state []byte) (*sandbox.Sandbox, error) {
	client, err := p.agentClientFromState(ctx, ref, state)
	if err != nil {
		return nil, err
	}
	return client.Get(ctx, ref, state)
}

func (p *Provider) AcquireHTTPClient(ctx context.Context, ref sandbox.SandboxRef, state []byte, scopes []string) (*transport.HTTPClientLease, error) {
	client, err := p.agentClientFromState(ctx, ref, state)
	if err != nil {
		return nil, err
	}
	return client.AcquireHTTPClient(ctx, ref, state, scopes)
}

func (p *Provider) Archive(ctx context.Context, ref sandbox.SandboxRef, state []byte) ([]byte, error) {
	client, err := p.agentClientFromState(ctx, ref, state)
	if err != nil {
		return state, err
	}
	return client.Archive(ctx, ref, state)
}

func (p *Provider) Remove(ctx context.Context, ref sandbox.SandboxRef, state []byte) ([]byte, error) {
	client, err := p.agentClientFromState(ctx, ref, state)
	if err != nil {
		return state, err
	}
	return client.Remove(ctx, ref, state)
}

func (p *Provider) agentClientFromState(ctx context.Context, ref sandbox.SandboxRef, state []byte) (*poolAgentClient, error) {
	poolID, err := poolIDFromRuntimeState(state)
	if err != nil {
		return nil, err
	}
	pool, err := p.manager.GetPool(ctx, ref.ProjectID, poolID)
	if err != nil {
		return nil, err
	}
	return p.agentClientForPool(ctx, pool)
}

func (p *Provider) agentClientForPool(ctx context.Context, pool *model.Pool) (*poolAgentClient, error) {
	lease, err := p.acquirePoolAgentClient(ctx, pool)
	if err != nil {
		return nil, err
	}
	return &poolAgentClient{poolID: pool.ID, tokenIssuer: p.manager, lease: lease}, nil
}

func (p *Provider) acquirePoolAgentClient(ctx context.Context, pool *model.Pool) (*transport.HTTPClientLease, error) {
	lease, err := p.runtimeProvider.AcquirePoolAgentClient(ctx, pool)
	if err == nil {
		return lease, nil
	}
	if p.manager == nil || pool == nil || strings.TrimSpace(pool.ID) == "" {
		return nil, err
	}
	pool, retryErr := p.reconcilePoolAfterClientError(ctx, pool)
	if retryErr != nil {
		return nil, retryErr
	}
	return p.runtimeProvider.AcquirePoolAgentClient(ctx, pool)
}

func (p *Provider) reconcilePoolAfterClientError(ctx context.Context, pool *model.Pool) (*model.Pool, error) {
	if err := p.manager.SchedulePoolReconciliation(ctx, pool.ProjectID, pool.ID); err != nil {
		return nil, err
	}
	return p.waitForPoolReconcile(ctx, pool.ProjectID, pool.ID)
}

// waitForPoolReconcile polls the pool row until its recorded operation reaches
// a terminal status. Reconciliation is level-triggered: the reconciler writes
// progress onto the resource itself, so the resource is the thing to watch.
func (p *Provider) waitForPoolReconcile(ctx context.Context, projectID, poolID string) (*model.Pool, error) {
	deadline := time.Now().Add(poolCapacityWaitTimeout)
	for {
		pool, err := p.manager.GetPool(ctx, projectID, poolID)
		if err != nil {
			return nil, err
		}
		if pool != nil && poolReconcileSettled(pool) {
			return pool, nil
		}
		if poolCapacityWaitTimeout <= 0 || !time.Now().Before(deadline) {
			return nil, sandbox.ErrNoSandboxCapacity
		}
		timer := time.NewTimer(poolCapacityPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// poolReconcileSettled reports whether the pool's reconciler has finished
// acting on its latest intent, however that turned out. Under ADR 0017 §3 that
// is exactly what matching generations mean.
func poolReconcileSettled(pool *model.Pool) bool {
	return pool.Converged()
}

func poolIDFromRuntimeState(state []byte) (string, error) {
	var runtimeSandbox sandbox.Sandbox
	if len(state) == 0 {
		return "", sandbox.ErrNotFound
	}
	if err := json.Unmarshal(state, &runtimeSandbox); err != nil {
		return "", err
	}
	if runtimeSandbox.Metadata != nil && runtimeSandbox.Metadata["pool_id"] != "" {
		return runtimeSandbox.Metadata["pool_id"], nil
	}
	return "", sandbox.ErrNotFound
}
