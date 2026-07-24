package pools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/obot-platform/discobox/pool-agent/poolauth"
	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/auth"
	poolagentauth "github.com/obot-platform/discobox/server/internal/auth/poolagent"
	"github.com/obot-platform/discobox/server/internal/model"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

// SandboxStateReporter records agent-observed sandbox states through the
// sandbox control plane, which owns the sandbox rows.
type SandboxStateReporter interface {
	ReportSandboxStates(ctx context.Context, batch store.SandboxStateReportBatch) error
}

// SetSandboxStateReporter wires the sandbox service dependency for
// agent-reported sandbox states.
func (s *Service) SetSandboxStateReporter(reporter SandboxStateReporter) {
	s.sandboxReporter = reporter
}

// RegisterPool redeems a bootstrap token from a starting pool agent and
// records its public key.
//
// Registration is an observation, so it reaches the reconciler as a dirty mark
// rather than by writing the reconciler's fields itself (ADR 0017 §10). The
// mark is what promotes the pool out of `registering` promptly; without it the
// row would carry a stale state until the 60s drift scan.
func (s *Service) RegisterPool(ctx context.Context, input services.RegisterPoolBody) (*services.RegisterPoolResponseBody, error) {
	projectID := strings.TrimSpace(input.ProjectId)
	poolID := strings.TrimSpace(input.PoolId)
	if projectID == "" || poolID == "" || strings.TrimSpace(input.BootstrapToken) == "" || strings.TrimSpace(input.PublicKey) == "" {
		return nil, fmt.Errorf("projectId, poolId, bootstrapToken, and publicKey are required")
	}
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return nil, mapAPIError(err, "pool not found")
	}
	h := sha256.Sum256([]byte(input.BootstrapToken))
	if _, err := s.store.RegisterPool(ctx, pool.ID, h[:], input.PublicKey, defaultString(input.KeyType.Or(""), poolauth.KeyType)); err != nil {
		return nil, mapAPIError(err, "pool bootstrap token not found")
	}
	if s.pools != nil {
		if err := s.pools.SchedulePoolReconciliation(ctx, pool.ProjectID, pool.ID); err != nil {
			return nil, err
		}
	}
	return &services.RegisterPoolResponseBody{}, nil
}

// UpdatePoolStatus records an agent heartbeat. Only the authenticated pool
// principal for the same pool may report.
func (s *Service) UpdatePoolStatus(ctx context.Context, poolID string, input services.UpdatePoolStatusBody) (*model.Pool, error) {
	poolID = strings.TrimSpace(poolID)
	if poolID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "poolId is required")
	}
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypePool || principal.PoolID != poolID {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "pool agent is not authorized to update this status")
	}
	pool, err := s.store.UpdatePoolStatus(ctx, poolID, input.Ready, input.Schedulable, input.Degraded, input.AvailableCpuVcpus, input.AvailableMemoryBytes, input.AvailableStorageBytes, services.RawMessage(input.Conditions))
	if err != nil {
		return nil, mapAPIError(err, "pool not found")
	}
	return pool, nil
}

// MintSandboxAgentStatusTokens mints short-lived, status:read-only
// sandbox-agent tokens for the sandboxes a pool agent requests, so its
// standing status-poll loop can call each sandbox-agent's status endpoint
// without discobox-server ever originating a call into a sandbox itself.
//
// The minted scope is always exactly ScopeStatusRead, a Go literal never
// derived from the request: there is no code path here, buggy or malicious,
// by which this endpoint can mint anything broader.
func (s *Service) MintSandboxAgentStatusTokens(ctx context.Context, poolID string, input services.MintSandboxAgentStatusTokensBody) (*services.MintSandboxAgentStatusTokensResponseBody, error) {
	poolID = strings.TrimSpace(poolID)
	if poolID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "poolId is required")
	}
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypePool || principal.PoolID != poolID {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "pool agent is not authorized to mint sandbox-agent status tokens")
	}
	pool, err := s.store.GetPoolByID(ctx, poolID)
	if err != nil {
		return nil, mapAPIError(err, "pool not found")
	}
	tokens := make([]services.SandboxAgentStatusToken, 0, len(input.SandboxIds))
	for _, sandboxID := range input.SandboxIds {
		sandboxID = strings.TrimSpace(sandboxID)
		if sandboxID == "" {
			continue
		}
		sandboxModel, err := s.store.GetSandbox(ctx, pool.ProjectID, sandboxID)
		if err != nil || strings.TrimSpace(sandboxModel.PoolID) != poolID {
			// Skip sandboxes this pool doesn't host rather than failing the whole
			// batch: a pool cannot obtain a token for a sandbox it does not own.
			continue
		}
		token, err := s.pools.CreateSandboxAgentToken(ctx, poolagentauth.TokenClaims{
			ProjectID: pool.ProjectID,
			PoolID:    poolID,
			SandboxID: sandboxID,
			Scopes:    []string{poolagentauth.ScopeStatusRead},
		})
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, services.SandboxAgentStatusToken{
			SandboxId: sandboxID,
			Token:     token,
			ExpiresAt: time.Now().UTC().Add(poolagentauth.TokenTTL),
		})
	}
	return &services.MintSandboxAgentStatusTokensResponseBody{Tokens: tokens}, nil
}

// ReportSandboxAgentStatus records pool-agent-polled sandbox status (git
// status, harness session state, active connections — see ADR 0030, a
// distinct channel from ReportPoolSandboxStates below, which is pool-agent's
// own observed container power state, ADR 0017 §10). Entries
// naming a sandbox this pool does not host are skipped rather than failing
// the whole batch: a pool cannot write status onto a sandbox it doesn't own,
// but one bad entry must not block every other sandbox's update in the tick.
func (s *Service) ReportSandboxAgentStatus(ctx context.Context, poolID string, input services.ReportSandboxAgentStatusBody) error {
	poolID = strings.TrimSpace(poolID)
	if poolID == "" {
		return apperrors.NewStatusError(http.StatusBadRequest, "poolId is required")
	}
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypePool || principal.PoolID != poolID {
		return apperrors.NewStatusError(http.StatusForbidden, "pool agent is not authorized to report sandbox agent status")
	}
	pool, err := s.store.GetPoolByID(ctx, poolID)
	if err != nil {
		return mapAPIError(err, "pool not found")
	}
	for _, entry := range input.Sandboxes {
		sandboxID := strings.TrimSpace(entry.SandboxId)
		if sandboxID == "" {
			continue
		}
		sandboxModel, err := s.store.GetSandbox(ctx, pool.ProjectID, sandboxID)
		if err != nil || strings.TrimSpace(sandboxModel.PoolID) != poolID {
			continue
		}
		status, err := json.Marshal(entry.Status)
		if err != nil {
			continue
		}
		if err := s.store.UpdateSandboxAgentStatus(ctx, pool.ProjectID, sandboxID, status, entry.ObservedAt); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

// ReportPoolSandboxStates records a batch of sandbox state observations from
// the agent hosting them (ADR 0017 §10).
//
// It writes observations only. Nothing here bumps a generation or touches
// desired state: what the agent saw is not a request for anything, and the
// control plane holds no opinion about whether a sandbox should be running.
func (s *Service) ReportPoolSandboxStates(ctx context.Context, poolID string, input services.ReportPoolSandboxStatesBody) error {
	poolID = strings.TrimSpace(poolID)
	if poolID == "" {
		return apperrors.NewStatusError(http.StatusBadRequest, "poolId is required")
	}
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypePool || principal.PoolID != poolID {
		return apperrors.NewStatusError(http.StatusForbidden, "pool agent is not authorized to report sandbox states")
	}
	if s.sandboxReporter == nil {
		return errors.New("sandbox state reporter is required")
	}
	reports := make([]store.SandboxStateReport, 0, len(input.States))
	for _, state := range input.States {
		sandboxID := strings.TrimSpace(state.SandboxId)
		if sandboxID == "" {
			continue
		}
		reports = append(reports, store.SandboxStateReport{
			SandboxID: sandboxID,
			State:     string(state.State),
			Error:     strings.TrimSpace(state.Error.Or("")),
		})
	}
	batch := store.SandboxStateReportBatch{
		PoolID:     poolID,
		BootID:     strings.TrimSpace(input.BootId),
		Sequence:   input.Sequence,
		ReportedAt: input.ReportedAt,
		Complete:   input.Complete,
		Reports:    reports,
	}
	if err := s.sandboxReporter.ReportSandboxStates(ctx, batch); err != nil {
		return mapAPIError(err, "pool not found")
	}
	return nil
}

// ReconcilePool marks the pool dirty for a manual reconcile request.
func (s *Service) ReconcilePool(ctx context.Context, projectID, poolID string) (*model.Pool, error) {
	if s.pools == nil {
		return nil, fmt.Errorf("pool control plane is required")
	}
	pool, err := s.store.GetPool(ctx, strings.TrimSpace(projectID), strings.TrimSpace(poolID))
	if err != nil {
		return nil, mapAPIError(err, "pool not found")
	}
	if err := s.pools.SchedulePoolReconciliation(ctx, pool.ProjectID, pool.ID); err != nil {
		return nil, err
	}
	return s.store.GetPool(ctx, pool.ProjectID, pool.ID)
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
