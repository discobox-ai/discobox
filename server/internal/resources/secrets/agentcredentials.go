package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/secretformat"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/x/id"
)

// The agent credentials broker: the control-plane half of ADR 0031. A pool
// agent calls it on behalf of one of its own sandboxes, and every entry point
// re-derives the sandbox from the calling pool rather than trusting the caller
// about which sandbox it is speaking for.
//
// Nothing here hands out a value. The broker records asks, reports approvals,
// and publishes the stable sentinel binding that the pool agent's ephemeral
// sentinels translate back to; cleartext leaves only through
// ResolveSandboxSecret, which is unchanged by this flow.

// ListSandboxCredentials returns the sandbox's agent-requested bindings whose
// grants are still live, with their approved uses.
func (s *Service) ListSandboxCredentials(ctx context.Context, poolID, sandboxID string) ([]store.AgentCredential, error) {
	sandbox, err := s.sandboxOwnedByPool(ctx, poolID, sandboxID)
	if err != nil {
		return nil, err
	}
	// The same scopes a resolve is matched against: what this discobox is, what
	// harness it runs, and the project it belongs to.
	return s.store.ListLiveAgentCredentials(ctx, sandbox.ProjectID, sandbox.ID, agentGrantScopes(sandbox))
}

// agentGrantScopes is what a discobox's agent may be granted through: itself,
// the harness config it runs, and its project.
func agentGrantScopes(sandbox *model.Sandbox) []store.GrantScope {
	scopes := []store.GrantScope{{Scope: model.SecretGrantScopeSandbox, ScopeKey: sandbox.ID}}
	if sandbox.HarnessConfigID != nil && strings.TrimSpace(*sandbox.HarnessConfigID) != "" {
		scopes = append(scopes, store.GrantScope{
			Scope:    model.SecretGrantScopeHarnessConfig,
			ScopeKey: strings.TrimSpace(*sandbox.HarnessConfigID),
		})
	}
	return append(scopes, store.GrantScope{Scope: model.SecretGrantScopeProject, ScopeKey: sandbox.ProjectID})
}

// CreateSandboxCredentialRequest records an agent's ask as a pending
// SecretRequest and returns immediately. Approval is a human act with human
// latency, so this never waits for one; the caller polls
// GetSandboxCredentialRequest.
//
// An identical pending ask is reused rather than duplicated, so an agent that
// retries — or a wrapper that asks again on each attempt — produces one inbox
// item instead of a pile.
func (s *Service) CreateSandboxCredentialRequest(ctx context.Context, poolID string, input services.CreateSandboxCredentialRequestBody) (*model.SecretRequest, error) {
	sandbox, err := s.sandboxOwnedByPool(ctx, poolID, strings.TrimSpace(input.SandboxId))
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "credential name is required")
	}
	envName := strings.TrimSpace(input.EnvVar)
	if envName == "" || strings.ContainsAny(envName, "=\x00") {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "credential request requires a valid environment variable name")
	}
	// The host is mandatory here and nowhere else: approving this request mints
	// a grant, and a grant minted by this flow may not be host-unscoped
	// (ADR 0031 §5). Refusing at the ask is better than discovering it at the
	// approval, where a human has already decided to say yes.
	host := normalizeHost(input.Host)
	if host == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "credential request requires a destination host")
	}
	uses, err := requestedUses(input.Uses)
	if err != nil {
		return nil, err
	}

	requestedBy := agentRequesterID(sandbox.ID)
	existing, err := s.store.FindPendingAgentCredentialRequest(ctx, sandbox.ProjectID, sandbox.ID, envName, host)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	req := &model.SecretRequest{
		ProjectID:   sandbox.ProjectID,
		RequestedBy: requestedBy,
		SandboxID:   sandbox.ID,
		// Every secret is a token now, and a token is what the swap carries:
		// ResolveSandboxSecret emits Value.Token, so nothing a request can name
		// is a credential the proxy could not substitute.
		Type:          model.SecretTypeToken,
		Host:          host,
		Name:          name,
		EnvName:       envName,
		Justification: strings.TrimSpace(input.Justification.Or("")),
		Uses:          uses,
		Status:        model.SecretRequestStatusPending,
	}
	if err := s.store.CreateSecretRequest(ctx, req); err != nil {
		return nil, err
	}
	return req, nil
}

// GetSandboxCredentialRequest reads one of a sandbox's own credential requests
// and, once approved, the grant carrying the use IDs it may present.
func (s *Service) GetSandboxCredentialRequest(ctx context.Context, poolID, sandboxID, requestID string) (*model.SecretRequest, *model.SecretGrant, error) {
	sandbox, err := s.sandboxOwnedByPool(ctx, poolID, sandboxID)
	if err != nil {
		return nil, nil, err
	}
	req, err := s.store.GetSecretRequest(ctx, sandbox.ProjectID, requestID)
	if err != nil {
		return nil, nil, apiError(err, "secret request not found")
	}
	// A sandbox may poll only its own asks. Requests are project-scoped rows and
	// a pool hosts many sandboxes, so without this a compromised sandbox could
	// read every request in the project by guessing IDs.
	if req.SandboxID != sandbox.ID || !req.FromProtocol() {
		return nil, nil, apperrors.NewStatusError(http.StatusNotFound, "secret request not found")
	}
	if req.Status != model.SecretRequestStatusApproved || req.GrantID == "" {
		return req, nil, nil
	}
	grant, err := s.store.GetSecretGrant(ctx, sandbox.ProjectID, req.GrantID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The grant was revoked after approval. The request stays approved
			// (it is history), but there is nothing to use.
			return req, nil, nil
		}
		return nil, nil, err
	}
	return req, grant, nil
}

// bindAgentCredential gives the sandbox its stable binding for an approved
// credential, creating it if the sandbox has none yet.
//
// The binding is deliberately not the harness-secret shape. It is marked
// AgentRequested, so it is never written into the sandbox environment or
// secrets.json and never registered with the proxy; the only value that reaches
// the sandbox is an ephemeral sentinel the pool agent mints per use and
// translates back to this one (ADR 0031 §4).
//
// A repeat approval for the same environment variable reuses the binding, so a
// sentinel an earlier activation was minted from stays resolvable.
func (s *Service) bindAgentCredential(ctx context.Context, req *model.SecretRequest, secret *model.Secret) error {
	return s.bindAgentSecret(ctx, req.ProjectID, req.SandboxID, req.EnvName, secret)
}

// bindAgentSecret is the binding itself, without a request in front of it: the
// same shape whether an agent asked for the credential or somebody granted it
// ahead of time.
func (s *Service) bindAgentSecret(ctx context.Context, projectID, sandboxID, envName string, secret *model.Secret) error {
	existing, err := s.store.FindAgentSandboxSecret(ctx, projectID, sandboxID, envName)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if existing != nil {
		if existing.SecretID == secret.ID {
			return nil
		}
		// One environment variable, one credential. Rebinding it would leave any
		// live activation resolving to a different secret, and silently changing
		// which credential an agent's next command carries is exactly the
		// surprise this flow exists to prevent.
		return apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("sandbox already has an agent credential bound to %s from a different secret; revoke that grant first", envName))
	}
	sentinel, err := secretformat.MintSentinel(secret.Format)
	if err != nil {
		return err
	}
	return s.store.CreateSandboxSecret(ctx, &model.SandboxSecret{
		ProjectID:      projectID,
		SandboxID:      sandboxID,
		SecretID:       secret.ID,
		EnvName:        envName,
		Sentinel:       sentinel,
		AgentRequested: true,
	})
}

// requestedUses validates and normalizes the uses an agent asked for. Supplied
// IDs are dropped: a use ID is minted by the approval, so an agent cannot name
// the use it will later present (ADR 0031 §5).
func requestedUses(input []apimodel.SecretUse) ([]model.SecretUse, error) {
	if len(input) == 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "credential request requires at least one declared use")
	}
	uses := make([]model.SecretUse, 0, len(input))
	for _, use := range input {
		description := strings.TrimSpace(use.Description)
		if description == "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "each declared use requires a description")
		}
		uses = append(uses, model.SecretUse{Description: description})
	}
	return uses, nil
}

// convertAPIUses maps approver-edited uses onto the model. IDs are dropped here
// too: mintUseIDs is the only thing that ever sets one.
func convertAPIUses(input []apimodel.SecretUse) []model.SecretUse {
	uses := make([]model.SecretUse, 0, len(input))
	for _, use := range input {
		if description := strings.TrimSpace(use.Description); description != "" {
			uses = append(uses, model.SecretUse{Description: description})
		}
	}
	return uses
}

// prefixSecretUse identifies one approved way to use a granted credential. A
// use ID is minted by the approval, never by the requester, so an agent cannot
// name the use it will later present (ADR 0031 §5).
//
// It sits here rather than beside every other prefix because those live in
// github.com/discobox-ai/x/id, outside this repository; move it there and drop
// this const once that package carries it.
const prefixSecretUse = "use"

// mintUseIDs stamps a fresh ID onto every approved use. It runs at approval for
// both the confirmed and the edited case, so no path can produce a grant use
// whose ID came from outside.
func mintUseIDs(uses []model.SecretUse) ([]model.SecretUse, error) {
	out := make([]model.SecretUse, 0, len(uses))
	for _, use := range uses {
		useID, err := id.New(prefixSecretUse)
		if err != nil {
			return nil, err
		}
		out = append(out, model.SecretUse{UseID: useID, Description: strings.TrimSpace(use.Description)})
	}
	return out, nil
}

// agentRequesterID is the principal recorded on a protocol-originated request.
// It is distinct from the reactive path's "sandbox:<id>" so the two species stay
// legible in the approval inbox and in FindPendingSecretRequest's dedup domain.
func agentRequesterID(sandboxID string) string { return "agent:" + sandboxID }

// sandboxOwnedByPool resolves a sandbox and verifies the calling pool hosts it,
// the same ownership check ResolveSandboxSecret makes. A pool may only ever
// speak for its own sandboxes.
func (s *Service) sandboxOwnedByPool(ctx context.Context, poolID, sandboxID string) (*model.Sandbox, error) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "sandbox ID is required")
	}
	sandbox, err := s.store.GetSandboxByID(ctx, sandboxID)
	if err != nil {
		return nil, apiError(err, "sandbox not found")
	}
	if strings.TrimSpace(sandbox.PoolID) != strings.TrimSpace(poolID) {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "sandbox not found")
	}
	return sandbox, nil
}

// AgentCredentialRequestStatus maps a request's approval state onto the
// protocol's vocabulary. The protocol says "granted", the control plane says
// "approved", and only the control plane knows that an approval whose grant has
// since been revoked is no longer a grant.
func AgentCredentialRequestStatus(req *model.SecretRequest, grant *model.SecretGrant) string {
	switch req.Status {
	case model.SecretRequestStatusApproved:
		if grant == nil {
			return model.SecretRequestStatusDenied
		}
		return "granted"
	case model.SecretRequestStatusDenied:
		return model.SecretRequestStatusDenied
	default:
		return model.SecretRequestStatusPending
	}
}
