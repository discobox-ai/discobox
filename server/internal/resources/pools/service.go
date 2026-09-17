// Package pools owns the Pool resource: the user-visible sharing boundary
// sandboxes are scheduled into. A pool binds to one provider instance at
// create time, immutably; capacity and sizing policy live on the pool, while
// the provider instance is backend identity only.
package pools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

type Service struct {
	store           *store.Store
	providers       *sandbox.ProviderManager
	pools           *ControlPlane
	sandboxReporter SandboxStateReporter
}

func NewService(appStore *store.Store, providerManager *sandbox.ProviderManager, controlPlane *ControlPlane) *Service {
	return &Service{store: appStore, providers: providerManager, pools: controlPlane}
}

func (s *Service) ListPools(ctx context.Context, projectID string) ([]model.Pool, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	return s.store.ListPools(ctx, projectID)
}

func (s *Service) CreatePool(ctx context.Context, projectID string, input services.CreatePoolBody) (*model.Pool, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "pool name is required")
	}
	providerInstanceID := strings.TrimSpace(input.ProviderInstanceId)
	if providerInstanceID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "pool provider instance is required")
	}
	provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, providerInstanceID)
	if err != nil {
		return nil, apperrors.NotFound(err, "provider instance not found")
	}
	if err := s.checkPoolSize(provider, sizeOf(input)); err != nil {
		return nil, err
	}
	pool := &model.Pool{
		ProjectID: projectID,
		PoolManifest: model.PoolManifest{
			Name:               name,
			ProviderInstanceID: provider.ID,
			CPUVCPUs:           input.CpuVcpus.Or(0),
			MemoryBytes:        input.MemoryBytes.Or(0),
			StorageBytes:       input.StorageBytes.Or(0),
		},
	}
	if err := s.store.CreatePool(ctx, pool); err != nil {
		return nil, err
	}
	if s.pools != nil {
		if err := s.pools.SchedulePoolReconciliation(ctx, projectID, pool.ID); err != nil {
			return nil, err
		}
	}
	return s.GetPool(ctx, projectID, pool.ID)
}

func (s *Service) GetPool(ctx context.Context, projectID, poolID string) (*model.Pool, error) {
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return nil, apperrors.NotFound(err, "pool not found")
	}
	return pool, nil
}

func (s *Service) UpdatePool(ctx context.Context, projectID, poolID string, input services.UpdatePoolBody) (*model.Pool, error) {
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return nil, apperrors.NotFound(err, "pool not found")
	}
	if sizes := updatedSizeOf(input); len(sizes) > 0 {
		provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, pool.ProviderInstanceID)
		if err != nil {
			return nil, apperrors.NotFound(err, "provider instance not found")
		}
		if err := s.checkPoolSize(provider, sizes); err != nil {
			return nil, err
		}
	}
	if name, ok := input.Name.Get(); ok {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "pool name is required")
		}
		pool.Name = name
	}
	if value, ok := input.CpuVcpus.Get(); ok {
		pool.CPUVCPUs = value
	}
	if value, ok := input.MemoryBytes.Get(); ok {
		pool.MemoryBytes = value
	}
	if value, ok := input.StorageBytes.Get(); ok {
		pool.StorageBytes = value
	}
	if err := s.store.UpdatePool(ctx, pool); err != nil {
		return nil, err
	}
	// Sizing policy may have changed; let the pool reconciler converge it.
	if s.pools != nil {
		if err := s.pools.SchedulePoolReconciliation(ctx, projectID, pool.ID); err != nil {
			return nil, err
		}
	}
	return s.GetPool(ctx, projectID, poolID)
}

// SetDefaultPool points the project's default pool at poolID, so new sandboxes
// created without an explicit pool are scheduled into it.
func (s *Service) SetDefaultPool(ctx context.Context, projectID, poolID string) (*model.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return nil, apperrors.NotFound(err, "pool not found")
	}
	project.DefaultPoolID = pool.ID
	if err := s.store.UpsertProject(ctx, project); err != nil {
		return nil, err
	}
	return s.store.GetProject(ctx, projectID)
}

// UnsetDefaultPool clears the project's default pool when it currently points
// at poolID, leaving the project with no default. New sandboxes must then name
// a pool explicitly. Clearing a pool that is not the default is rejected so the
// intent is unambiguous.
func (s *Service) UnsetDefaultPool(ctx context.Context, projectID, poolID string) (*model.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return nil, apperrors.NotFound(err, "pool not found")
	}
	if project.DefaultPoolID != pool.ID {
		return nil, apperrors.NewStatusError(http.StatusConflict, "pool is not the project default")
	}
	project.DefaultPoolID = ""
	if err := s.store.UpsertProject(ctx, project); err != nil {
		return nil, err
	}
	return s.store.GetProject(ctx, projectID)
}

// DeletePool submits delete intent for an empty pool that is not the project
// default. A pool with sandboxes cannot be deleted (pool assignment is
// immutable, so there is nothing to drain to), and the default pool must first
// be unset or replaced so new sandboxes retain a scheduling target. The
// reconciler removes the runtime host, then the row.
func (s *Service) DeletePool(ctx context.Context, projectID, poolID string) error {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return apperrors.NotFound(err, "project not found")
	}
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return apperrors.NotFound(err, "pool not found")
	}
	if project.DefaultPoolID == pool.ID {
		return apperrors.NewStatusError(http.StatusConflict, "pool is the project default; set a different default or unset it before deleting")
	}
	sandboxCount, err := s.store.CountSandboxesForPool(ctx, projectID, pool.ID)
	if err != nil {
		return err
	}
	if sandboxCount > 0 {
		return apperrors.NewStatusError(http.StatusConflict, "pool has sandboxes")
	}
	if s.pools == nil {
		return fmt.Errorf("pool control plane is required")
	}
	if _, err := s.pools.SubmitPoolDelete(ctx, projectID, pool.ID); err != nil {
		return apperrors.NotFound(err, "pool not found")
	}
	return nil
}

// ClearPoolCache has the pool's agent stop every running sandbox on the pool and
// empty the pool's caches, and returns the sandboxes it stopped.
//
// The agent owns the operation: it knows which sandboxes use the caches and
// where they are, it keeps sandboxes from starting while it works, and it
// answers only once they are empty. Nothing here tracks or persists the
// clear, and nothing is started again afterwards — a stopped sandbox starts on
// its next use.
func (s *Service) ClearPoolCache(ctx context.Context, projectID, poolID string) ([]string, error) {
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return nil, apperrors.NotFound(err, "pool not found")
	}
	provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, pool.ProviderInstanceID)
	if err != nil {
		return nil, apperrors.NotFound(err, "pool provider instance not found")
	}
	if s.providers == nil {
		return nil, apperrors.NewStatusError(http.StatusServiceUnavailable, "sandbox provider manager is not configured")
	}
	instance, err := s.providers.ResolveInstance(ctx, provider)
	if err != nil {
		return nil, err
	}
	runtime, ok := instance.(sandbox.PoolRuntime)
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotImplemented, fmt.Sprintf("provider %q hosts no pool runtime with a cache to clear", provider.Type))
	}
	stopped, err := runtime.ClearCache(ctx, pool)
	if errors.Is(err, sandbox.ErrPoolAgentUnsupported) {
		// A pool whose agent predates the operation. The route-level 404 it
		// answers with reads as "not found" otherwise, about a pool that is
		// plainly there.
		return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
			"pool %s is running a pool agent older than this server, which cannot clear its caches; the pool moves onto the current agent when it is next reconciled, after which this will work", pool.ID))
	}
	return stopped, err
}

// auditPoolReadTimeout bounds one pool's part of an audit read. The whole read
// waits for its slowest pool, so without it one unreachable host holds every
// answer for as long as the network takes to give up on it. A variable so a
// test can shorten it.
var auditPoolReadTimeout = 20 * time.Second

// ListHTTPAudit reads the project's pool proxies' HTTP audit and merges it
// newest first (ADR 0130 §§1, 4).
//
// Which pools are asked: the one PoolID names; otherwise, when the sandbox the
// filter names still exists, the pool it runs on; otherwise every pool in the
// project. The last case is not a fallback so much as the reason the read is
// project-scoped at all — a purged sandbox's exchanges stay on its pool for
// the retention window, and nothing left records which pool that was.
//
// Every pool is asked for the whole limit, because the newest N across pools
// can all come from one of them, and each is given auditPoolReadTimeout to
// answer. A pool that cannot be read is reported by name with why; a trail that
// silently omits a pool reads as a complete one. A pool being deleted, or whose
// agent never registered, is reported without being asked: there is no agent to
// answer, and asking would only spend the deadline finding that out.
func (s *Service) ListHTTPAudit(ctx context.Context, projectID string, filter services.HTTPAuditFilter) (*services.HTTPAuditResult, error) {
	pools, err := s.auditPools(ctx, projectID, filter)
	if err != nil {
		return nil, err
	}
	query := sandbox.HTTPAuditQuery{
		SandboxID: filter.SandboxID,
		Host:      filter.Host,
		UseID:     filter.UseID,
		Since:     filter.Since,
		Limit:     filter.Limit,
	}
	type poolRead struct {
		poolID    string
		exchanges []sandbox.HTTPAuditExchange
		err       error
	}
	reads := make([]poolRead, len(pools))
	var wg sync.WaitGroup
	for i := range pools {
		if reason := unaskableAuditPool(&pools[i]); reason != "" {
			reads[i] = poolRead{poolID: pools[i].ID, err: errors.New(reason)}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			poolCtx, cancel := context.WithTimeout(ctx, auditPoolReadTimeout)
			defer cancel()
			exchanges, err := s.readPoolHTTPAudit(poolCtx, &pools[i], query)
			if err != nil && errors.Is(poolCtx.Err(), context.DeadlineExceeded) {
				err = fmt.Errorf("it did not answer within %s", auditPoolReadTimeout)
			}
			reads[i] = poolRead{poolID: pools[i].ID, exchanges: exchanges, err: err}
		}()
	}
	wg.Wait()

	result := &services.HTTPAuditResult{
		Exchanges:        []services.PoolHTTPAuditExchange{},
		UnavailablePools: []services.UnavailableAuditPool{},
	}
	for _, read := range reads {
		if read.err != nil {
			result.UnavailablePools = append(result.UnavailablePools, services.UnavailableAuditPool{PoolID: read.poolID, Reason: read.err.Error()})
			continue
		}
		for _, exchange := range read.exchanges {
			result.Exchanges = append(result.Exchanges, services.PoolHTTPAuditExchange{PoolID: read.poolID, HTTPAuditExchange: exchange})
		}
	}
	slices.SortStableFunc(result.Exchanges, func(a, b services.PoolHTTPAuditExchange) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		if c := strings.Compare(a.PoolID, b.PoolID); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	if filter.Limit > 0 && len(result.Exchanges) > filter.Limit {
		result.Exchanges = result.Exchanges[:filter.Limit]
	}
	return result, nil
}

// unaskableAuditPool says why a pool has no agent to ask, or "" when it does.
func unaskableAuditPool(pool *model.Pool) string {
	switch {
	case pool.DesiredState != model.DesiredStatePresent:
		return "it is being deleted"
	case pool.RegisteredAt == nil:
		return "its pool agent has not registered"
	default:
		return ""
	}
}

// auditPools resolves which pools an audit read asks. See ListHTTPAudit.
func (s *Service) auditPools(ctx context.Context, projectID string, filter services.HTTPAuditFilter) ([]model.Pool, error) {
	if filter.PoolID != "" {
		pool, err := s.store.GetPool(ctx, projectID, filter.PoolID)
		if err != nil {
			return nil, apperrors.NotFound(err, "pool not found")
		}
		return []model.Pool{*pool}, nil
	}
	if filter.SandboxID != "" {
		sb, err := s.store.GetSandbox(ctx, projectID, filter.SandboxID)
		switch {
		case err == nil && sb.PoolID != "":
			pool, err := s.store.GetPool(ctx, projectID, sb.PoolID)
			if err == nil {
				return []model.Pool{*pool}, nil
			}
			if !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
	}
	return s.store.ListPools(ctx, projectID)
}

// readPoolHTTPAudit reads one pool's audit through its provider's runtime.
func (s *Service) readPoolHTTPAudit(ctx context.Context, pool *model.Pool, query sandbox.HTTPAuditQuery) ([]sandbox.HTTPAuditExchange, error) {
	provider, err := s.store.GetSandboxProviderInstance(ctx, pool.ProjectID, pool.ProviderInstanceID)
	if err != nil {
		return nil, fmt.Errorf("its provider instance could not be loaded: %w", err)
	}
	if s.providers == nil {
		return nil, errors.New("no sandbox provider manager is configured")
	}
	instance, err := s.providers.ResolveInstance(ctx, provider)
	if err != nil {
		return nil, err
	}
	runtime, ok := instance.(sandbox.PoolRuntime)
	if !ok {
		return nil, fmt.Errorf("provider %q runs no pool proxy", provider.Type)
	}
	exchanges, err := runtime.ListHTTPAudit(ctx, pool, query)
	if errors.Is(err, sandbox.ErrPoolAgentUnsupported) {
		return nil, errors.New("its pool agent predates the audit read; the pool moves onto the current agent when it is next reconciled")
	}
	return exchanges, err
}

// OpenPoolConsole attaches to the pool host's administrative console.
//
// It resolves the pool's provider and asks it for the console directly. It
// deliberately does not require the pool to be ready, registered, or
// schedulable, and it does not refuse a disabled provider instance: a console
// is asked for when the pool is broken, and every one of those checks would
// withhold it exactly then.
func (s *Service) OpenPoolConsole(ctx context.Context, projectID, poolID string, opts sandbox.ConsoleOptions) (sandbox.PTY, error) {
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return nil, apperrors.NotFound(err, "pool not found")
	}
	provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, pool.ProviderInstanceID)
	if err != nil {
		return nil, apperrors.NotFound(err, "pool provider instance not found")
	}
	if s.providers == nil {
		return nil, apperrors.NewStatusError(http.StatusServiceUnavailable, "sandbox provider manager is not configured")
	}
	instance, err := s.providers.ResolveInstance(ctx, provider)
	if err != nil {
		return nil, err
	}
	runtime, ok := instance.(sandbox.PoolRuntime)
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotImplemented, fmt.Sprintf("provider %q hosts no pool runtime to open a console on", provider.Type))
	}
	console, err := runtime.OpenConsole(ctx, provider, pool, opts)
	if err != nil {
		return nil, err
	}
	return console, nil
}

// BuildPoolGuestImage rebuilds the guest image the pool's backend boots.
//
// It resolves the pool the same way OpenPoolLogs does and gates on nothing more
// for a related reason: the build needs the pool's Docker daemon and nothing
// else about the pool, and requiring a ready pool would withhold the operation
// from someone whose pool is unhealthy precisely because of the guest image
// they are trying to replace.
func (s *Service) BuildPoolGuestImage(ctx context.Context, projectID, poolID string, opts sandbox.GuestImageBuildOptions) (*sandbox.GuestImageBuild, error) {
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return nil, apperrors.NotFound(err, "pool not found")
	}
	provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, pool.ProviderInstanceID)
	if err != nil {
		return nil, apperrors.NotFound(err, "pool provider instance not found")
	}
	if s.providers == nil {
		return nil, apperrors.NewStatusError(http.StatusServiceUnavailable, "sandbox provider manager is not configured")
	}
	instance, err := s.providers.ResolveInstance(ctx, provider)
	if err != nil {
		return nil, err
	}
	runtime, ok := instance.(sandbox.PoolRuntime)
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotImplemented, fmt.Sprintf("provider %q hosts no pool runtime with a guest image to build", provider.Type))
	}
	build, err := runtime.BuildGuestImage(ctx, provider, pool, opts)
	if err != nil {
		if errors.Is(err, sandbox.ErrGuestImageBuildUnsupported) {
			return nil, apperrors.NewStatusError(http.StatusNotImplemented, err.Error())
		}
		return nil, err
	}
	return build, nil
}

// OpenPoolLogs reads what the pool's backend recorded about its host.
//
// It resolves the pool's provider the same way OpenPoolConsole does, and gates
// on nothing for the same reason: a host log is read when the pool is broken.
// What it adds is a status for the backends that keep no such record — that is
// a settled answer about this provider, not a failure to reach the host, and an
// operator should be told which of the two they got.
func (s *Service) OpenPoolLogs(ctx context.Context, projectID, poolID string, opts sandbox.PoolLogOptions) (*sandbox.PoolLogStream, error) {
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return nil, apperrors.NotFound(err, "pool not found")
	}
	provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, pool.ProviderInstanceID)
	if err != nil {
		return nil, apperrors.NotFound(err, "pool provider instance not found")
	}
	if s.providers == nil {
		return nil, apperrors.NewStatusError(http.StatusServiceUnavailable, "sandbox provider manager is not configured")
	}
	instance, err := s.providers.ResolveInstance(ctx, provider)
	if err != nil {
		return nil, err
	}
	runtime, ok := instance.(sandbox.PoolRuntime)
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotImplemented, fmt.Sprintf("provider %q hosts no pool runtime to read host logs from", provider.Type))
	}
	stream, err := runtime.OpenLogs(ctx, provider, pool, opts)
	if err != nil {
		if errors.Is(err, sandbox.ErrPoolLogsUnsupported) {
			return nil, apperrors.NewStatusError(http.StatusNotImplemented, err.Error())
		}
		return nil, err
	}
	return stream, nil
}
