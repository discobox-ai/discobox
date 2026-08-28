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

	"github.com/go-faster/jx"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pool-agent/poolauth"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// SandboxStateReporter records agent-observed sandbox states through the
// sandbox control plane, which owns the sandbox rows.
type SandboxStateReporter interface {
	ReportSandboxStates(ctx context.Context, batch store.SandboxStateReportBatch) error
	// ReportSandboxProgress records provisioning progress on sandboxes this
	// pool hosts (ADR 0039). It rides the same channel as state but is applied
	// separately: progress carries no observed state and takes no part in the
	// complete-sync rule.
	ReportSandboxProgress(ctx context.Context, poolID string, reportedAt time.Time, reports []store.SandboxProgressReport) error
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
	// A heartbeat from a pool recorded `offline` is the observation that the
	// host is back. Like registration (ADR 0017 §10), it reaches the
	// reconciler as a dirty mark rather than by writing State here; the mark
	// is what promotes the pool back to `active` promptly instead of waiting
	// for the drift scan.
	if pool.State == model.PoolStateOffline && s.pools != nil {
		if err := s.pools.SchedulePoolReconciliation(ctx, pool.ProjectID, pool.ID); err != nil {
			return nil, err
		}
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
		lastActive := reportedLastAccess(entry.Status, entry.ObservedAt)
		if err := s.store.UpdateSandboxAgentStatus(ctx, pool.ProjectID, sandboxID, status, entry.ObservedAt, lastActive); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

// ReportPoolResources records what a pool and its sandboxes are consuming
// (ADR 0071).
//
// The pool-wide half — the pool's own totals and its disk — lands on the pool
// row. Each sandbox's half lands on that sandbox's row, so a client looking at
// one sandbox does not have to fetch its pool to find out what it is using.
//
// Entries naming a sandbox this pool does not host are skipped rather than
// failing the whole report, for the same reason the status batch skips them: a
// pool cannot write onto a sandbox it does not own, but one stale entry must
// not discard every other sandbox's accounting for this tick.
func (s *Service) ReportPoolResources(ctx context.Context, poolID string, input services.ReportPoolResourcesBody) error {
	poolID = strings.TrimSpace(poolID)
	if poolID == "" {
		return apperrors.NewStatusError(http.StatusBadRequest, "poolId is required")
	}
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypePool || principal.PoolID != poolID {
		return apperrors.NewStatusError(http.StatusForbidden, "pool agent is not authorized to report pool resources")
	}
	pool, err := s.store.GetPoolByID(ctx, poolID)
	if err != nil {
		return mapAPIError(err, "pool not found")
	}
	reportedAt := input.Report.ReportedAt
	if reportedAt.IsZero() {
		reportedAt = time.Now().UTC()
	}

	// The pool's own record carries the report without the per-sandbox array,
	// which is on the sandboxes themselves. Storing it in both places would
	// duplicate the largest part of the payload and leave two copies to
	// disagree the moment one write succeeded and the other did not.
	poolResources := encodePoolResourceReport(reportedAt, input)
	if err := s.store.RecordPoolResources(ctx, poolID, poolResources, reportedAt); err != nil {
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
		// Encoded through the generated encoder rather than encoding/json:
		// an ogen optional field writes nothing at all when it is unset, which
		// encoding/json rejects as truncated output. The entry is stored as the
		// contract defines it, so a reader decodes it with the same schema the
		// agent sent.
		var encoder jx.Encoder
		entry.Encode(&encoder)
		resources := json.RawMessage(encoder.Bytes())
		// A sandbox that reported no counters still reported its disk, and its
		// entry has no observation time of its own; the report's own time is
		// what makes it possible to tell fresh accounting from stale.
		observedAt := entry.ObservedAt.Or(reportedAt)
		if observedAt.IsZero() {
			observedAt = reportedAt
		}
		if err := s.store.UpdateSandboxResources(ctx, pool.ProjectID, sandboxID, resources, observedAt); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

// encodePoolResourceReport is the pool's half of a resource report as it is
// stored: the report object exactly as the agent sent it. Per-sandbox figures
// are not in it — each rides its own sandbox row, so the figure lives in one
// place and two copies cannot disagree.
//
// Nothing is copied field by field. The agent sends this half as one object and
// it is stored as one object, so the stored shape cannot drift from the
// reported one as either evolves.
//
// Encoded through the generated encoder rather than encoding/json: an ogen
// optional field writes nothing at all when unset, which encoding/json rejects
// as truncated output.
func encodePoolResourceReport(reportedAt time.Time, input services.ReportPoolResourcesBody) json.RawMessage {
	report := input.Report
	report.ReportedAt = reportedAt
	var encoder jx.Encoder
	report.Encode(&encoder)
	return json.RawMessage(encoder.Bytes())
}

// reportedLastAccess is the newest client access the status payload carries:
// the max of the sessions' lastAccessedAt, taken as observedAt when any
// session has a client attached at observation. This is what makes the
// sandbox's LastActiveAt mean "a person touched it" — the shim under each
// terminal records attaches and keystrokes, the report carries them here,
// and the store only ever moves the column forward. The payload is opaque to
// this service (it is relayed as the sandbox-agent sent it), so the sessions
// are decoded here into the schema type they were sent as; a payload without
// them yields nil, which updates nothing.
func reportedLastAccess(status map[string]jx.Raw, observedAt time.Time) *time.Time {
	raw, ok := status["sessions"]
	if !ok {
		return nil
	}
	// A local struct, not the generated schema type: the payload is relayed
	// verbatim from whatever sandbox-agent version the sandbox runs, and the
	// generated type rejects any field it does not declare
	// (additionalProperties: false). Reading only the two fields this needs
	// keeps an agent on either side of a schema change from zeroing a
	// sandbox's last-active time.
	var sessions []struct {
		AttacherCount  int        `json:"attacherCount"`
		LastAccessedAt *time.Time `json:"lastAccessedAt"`
	}
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return nil
	}
	var latest time.Time
	for _, session := range sessions {
		if session.AttacherCount > 0 && observedAt.After(latest) {
			latest = observedAt
		}
		if at := session.LastAccessedAt; at != nil && at.After(latest) {
			latest = *at
		}
	}
	if latest.IsZero() {
		return nil
	}
	return &latest
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
	progress := make([]store.SandboxProgressReport, 0, len(input.Progress))
	for _, entry := range input.Progress {
		sandboxID := strings.TrimSpace(entry.SandboxId)
		if sandboxID == "" {
			continue
		}
		// The stored blob is the client-facing shape, not the agent-facing one:
		// the two schemas are separate contracts, and SandboxProvisionProgress
		// forbids additional properties, so passing the agent's entry through
		// verbatim would embed a sandboxId that clients then fail to decode.
		// The phase crosses the two schemas as a bare string conversion because
		// ogen enums are string-typed, so unlike the pull struct below this one
		// would not stop compiling if the vocabularies diverged. That is what
		// TestProvisionPhaseVocabulariesMatch pins instead.
		observed := serverapi.SandboxProvisionProgress{
			Phase: serverapi.SandboxProvisionPhase(entry.Phase),
		}
		if pull, ok := entry.Pull.Get(); ok {
			// A conversion rather than a field-by-field copy: the two shapes are
			// identical today, and if the agent-facing and client-facing schemas
			// ever diverge this stops compiling, which is where that divergence
			// should be noticed.
			observed.Pull = serverapi.NewOptSandboxPullProgress(serverapi.SandboxPullProgress(pull))
		}
		// ogen's own encoder, not encoding/json. An unset ogen optional
		// marshals to zero bytes, which encoding/json rejects out of a
		// json.Marshaler with "unexpected end of JSON input" — so
		// json.Marshal(observed) worked for exactly as long as every report
		// carried a pull, and 500'd on the first phase that had nothing to
		// measure. Encode skips unset fields instead, which is what the wire
		// wants anyway.
		payload, err := observed.MarshalJSON()
		if err != nil {
			return err
		}
		progress = append(progress, store.SandboxProgressReport{SandboxID: sandboxID, Progress: payload})
	}
	if err := s.sandboxReporter.ReportSandboxProgress(ctx, poolID, input.ReportedAt, progress); err != nil {
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
