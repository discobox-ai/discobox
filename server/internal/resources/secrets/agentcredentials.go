package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// The agent credentials broker: the control-plane half of the agent
// credentials protocol (ADR 0031, as ADR 26-10-27-880 leaves it). A pool agent
// calls it on behalf of one of its own sandboxes, and every entry point
// re-derives the sandbox from the calling pool rather than trusting the caller
// about which sandbox it is speaking for.
//
// Nothing here hands out a value. The broker records asks and reports what a
// sandbox holds; an approved use is delivered as every credential is, a
// sentinel in the variable it named, and cleartext leaves only through
// ResolveSandboxSecret.

// ListSandboxCredentials returns what a sandbox holds: each variable bound on
// it whose secret a live use grant covering it authorizes, and each live
// delegation grant it holds, which delivers nothing to it.
func (s *Service) ListSandboxCredentials(ctx context.Context, poolID, sandboxID string) ([]services.SandboxCredential, error) {
	sandbox, err := s.sandboxOwnedByPool(ctx, poolID, sandboxID)
	if err != nil {
		return nil, err
	}
	// The same scopes a resolve is matched against: what this discobox is, what
	// harness it runs, and the project it belongs to.
	grants, err := s.store.ListLiveGrantsCovering(ctx, sandbox.ProjectID, store.SandboxGrantScopes(sandbox))
	if err != nil {
		return nil, err
	}
	bindings, err := s.store.ListSandboxSecrets(ctx, sandbox.ProjectID, sandbox.ID)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	secretName := func(secretID string) (string, bool) {
		if name, ok := names[secretID]; ok {
			return name, name != ""
		}
		secret, err := s.store.GetSecret(ctx, sandbox.ProjectID, secretID)
		if err != nil {
			names[secretID] = ""
			return "", false
		}
		names[secretID] = secret.Name
		return secret.Name, true
	}
	var out []services.SandboxCredential
	for _, binding := range bindings {
		// The narrowest live use grant of the secret is the one a resolve
		// would find.
		for i := range grants {
			grant := &grants[i]
			if grant.SecretID != binding.SecretID || !grant.MayUse() {
				continue
			}
			name, ok := secretName(grant.SecretID)
			if !ok {
				break
			}
			out = append(out, sandboxCredential(name, binding.EnvName, grant))
			break
		}
	}
	for i := range grants {
		grant := &grants[i]
		if grant.MayUse() || grant.Scope != model.SecretGrantScopeSandbox || grant.ScopeKey != sandbox.ID {
			continue
		}
		if name, ok := secretName(grant.SecretID); ok {
			out = append(out, sandboxCredential(name, "", grant))
		}
	}
	return out, nil
}

func sandboxCredential(name, envName string, grant *model.SecretGrant) services.SandboxCredential {
	return services.SandboxCredential{
		Name:      name,
		EnvName:   envName,
		SecretID:  grant.SecretID,
		GrantID:   grant.ID,
		Hosts:     grant.Hosts,
		Purpose:   grant.Purpose,
		ExpiresAt: grant.ExpiresAt,
	}
}

// CreateSandboxCredentialRequest records an agent's ask as a pending
// SecretRequest and returns immediately. Approval is a human act with human
// latency, so this never waits for one; the caller polls
// GetSandboxCredentialRequest.
//
// An identical pending ask is reused rather than duplicated, so an agent that
// retries produces one inbox item instead of a pile. Identical means approving
// either would grant the same thing: the same variable, hosts, purpose and
// lifetime. The justification is left out; it argues for a grant and does not
// change which one approval mints, so a retry reworded is still a retry.
func (s *Service) CreateSandboxCredentialRequest(ctx context.Context, poolID string, input services.CreateSandboxCredentialRequestBody) (*model.SecretRequest, error) {
	sandbox, err := s.sandboxOwnedByPool(ctx, poolID, strings.TrimSpace(input.SandboxId))
	if err != nil {
		return nil, err
	}
	// What the credential is asked for is the grant's purpose once approved,
	// so it is read the way a grant's is.
	purpose, err := grantPurpose(string(input.Purpose.Or("")))
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(input.Name)
	envName := strings.TrimSpace(input.EnvVar.Or(""))
	hosts, _ := askedHosts(input.Hosts)
	if name == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "credential name is required")
	}
	// A use is delivered in the variable it names; a delegation delivers
	// nothing to the discobox that asks, so naming one is a mistake about
	// what it asks for.
	switch {
	case purpose == model.SecretGrantPurposeDelegate && envName != "":
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"a request to delegate names no environment variable: a delegation delivers nothing to the discobox that asks")
	case purpose == model.SecretGrantPurposeUse && (envName == "" || strings.ContainsAny(envName, "=\x00")):
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "credential request requires a valid environment variable name")
	}
	// A host is mandatory here and nowhere else: approving this request mints
	// a grant, and a grant minted by this flow may not be host-unscoped
	// (ADR 0031 §5). Refusing at the ask is better than discovering it at the
	// approval, where a human has already decided to say yes.
	if len(hosts) == 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "credential request requires a destination host")
	}
	// The lifetime asked for is recorded, not checked against any secret: which
	// secret answers the request is the approval's choice, and so is whether
	// its limit is raised to meet the ask.
	//
	// It is bounded on both sides all the same, because an ask becomes the
	// answer a human is shown already chosen. Unbounded, "forever" arrives
	// under another name — a ten-year ask, or one large enough to overflow the
	// duration a window converts it to and land back near zero. The ceiling is
	// the protocol's own (agentcreds.MaxGrantTTLSeconds), so the sandbox-side
	// CLI refuses the same asks this does rather than each end guessing.
	grantTTL := input.GrantTTLSeconds.Or(0)
	if grantTTL < 0 || grantTTL > agentcreds.MaxGrantTTLSeconds {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("a requested grant lifetime runs from 1 second to %d (thirty days); leave it out to ask for nothing in particular", int64(agentcreds.MaxGrantTTLSeconds)))
	}

	pending, err := s.store.FindPendingAgentCredentialRequests(ctx, sandbox.ProjectID, sandbox.ID, envName, hosts, purpose)
	if err != nil {
		return nil, err
	}
	for i := range pending {
		if pending[i].GrantTTL == grantTTL && pending[i].Name == name {
			return &pending[i], nil
		}
	}

	req := &model.SecretRequest{
		ProjectID:   sandbox.ProjectID,
		RequestedBy: agentRequesterID(sandbox.ID),
		SandboxID:   sandbox.ID,
		// Every secret is a token, and a token is what the swap carries:
		// ResolveSandboxSecret emits Value.Token, so nothing a request can name
		// is a credential the proxy could not substitute.
		Type:          model.SecretTypeToken,
		Hosts:         hosts,
		Name:          name,
		EnvName:       envName,
		Justification: strings.TrimSpace(input.Justification.Or("")),
		GrantTTL:      grantTTL,
		Purpose:       purpose,
		Status:        model.SecretRequestStatusPending,
	}
	if err := s.store.CreateSecretRequest(ctx, req); err != nil {
		return nil, err
	}
	return req, nil
}

// GetSandboxCredentialRequest reads one of a sandbox's own credential requests
// and, once approved, the grant it was answered with.
func (s *Service) GetSandboxCredentialRequest(ctx context.Context, poolID, sandboxID, requestID string) (*model.SecretRequest, *model.SecretGrant, error) {
	sandbox, err := s.sandboxOwnedByPool(ctx, poolID, sandboxID)
	if err != nil {
		return nil, nil, err
	}
	req, err := s.store.GetSecretRequest(ctx, sandbox.ProjectID, requestID)
	if err != nil {
		return nil, nil, apperrors.NotFound(err, "secret request not found")
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
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	if strings.TrimSpace(sandbox.PoolID) != strings.TrimSpace(poolID) {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "sandbox not found")
	}
	return sandbox, nil
}
