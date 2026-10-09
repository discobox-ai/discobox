package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/hostscope"
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
	return s.store.ListLiveAgentCredentials(ctx, sandbox.ProjectID, sandbox.ID, store.SandboxGrantScopes(sandbox))
}

// CreateSandboxCredentialRequest records an agent's ask as a pending
// SecretRequest and returns immediately. Approval is a human act with human
// latency, so this never waits for one; the caller polls
// GetSandboxCredentialRequest.
//
// An identical pending ask is reused rather than duplicated, so an agent that
// retries — or a wrapper that asks again on each attempt — produces one inbox
// item instead of a pile. Identical means approving either would grant the
// same thing (asksTheSame): an ask for other uses of the same credential is a
// new request, never folded into the open one, where its uses would be
// dropped while the agent was told it had asked for them.
func (s *Service) CreateSandboxCredentialRequest(ctx context.Context, poolID string, input services.CreateSandboxCredentialRequestBody) (*model.SecretRequest, error) {
	sandbox, err := s.sandboxOwnedByPool(ctx, poolID, strings.TrimSpace(input.SandboxId))
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(input.Name)
	envName := strings.TrimSpace(input.EnvVar)
	hosts, _ := askedHosts(input.Hosts)
	// A well-known credential names its own name, variable, and hosts, which an
	// ask must agree with (see wellknown.go).
	wellKnownID := strings.TrimSpace(input.ID.Or(""))
	if wellKnownID != "" {
		if name, envName, hosts, err = wellKnownAsk(wellKnownID, name, envName, hosts); err != nil {
			return nil, err
		}
	}
	if name == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "credential name is required")
	}
	if envName == "" || strings.ContainsAny(envName, "=\x00") {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "credential request requires a valid environment variable name")
	}
	// A host is mandatory here and nowhere else: approving this request mints
	// a grant, and a grant minted by this flow may not be host-unscoped
	// (ADR 0031 §5). Refusing at the ask is better than discovering it at the
	// approval, where a human has already decided to say yes.
	if len(hosts) == 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "credential request requires a destination host")
	}
	for _, host := range hosts {
		if err := reservedHostAsk(wellKnownID, host); err != nil {
			return nil, err
		}
	}
	uses, err := requestedUses(input.Uses)
	if err != nil {
		return nil, err
	}
	// The lifetime asked for is recorded, not checked against any secret: which
	// secret answers the request is the approval's choice, and so is whether
	// its limit is raised to meet the ask.
	//
	// It is bounded on both sides all the same, because an ask becomes the
	// answer a human is shown already chosen (ADR 0031 §5's approval is one
	// keystroke). Unbounded, "forever" arrives under another name — a ten-year
	// ask, or one large enough to overflow the duration a window converts it to
	// and land back near zero. The ceiling is the protocol's own
	// (agentcreds.MaxGrantTTLSeconds), so the sandbox-side CLI refuses the same
	// asks this does rather than each end guessing.
	grantTTL := input.GrantTTLSeconds.Or(0)
	if grantTTL < 0 || grantTTL > agentcreds.MaxGrantTTLSeconds {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("a requested grant lifetime runs from 1 second to %d (thirty days); leave it out to ask for nothing in particular", int64(agentcreds.MaxGrantTTLSeconds)))
	}
	// What the credential is asked for is the grant's purpose once approved,
	// so it is read the way a grant's is.
	purpose, err := grantPurpose(string(input.Purpose.Or("")))
	if err != nil {
		return nil, err
	}

	requestedBy := agentRequesterID(sandbox.ID)
	pending, err := s.store.FindPendingAgentCredentialRequests(ctx, sandbox.ProjectID, sandbox.ID, envName, hosts, wellKnownID, purpose)
	if err != nil {
		return nil, err
	}
	for i := range pending {
		if asksTheSame(pending[i], uses, grantTTL) {
			return &pending[i], nil
		}
	}

	req := &model.SecretRequest{
		ProjectID:   sandbox.ProjectID,
		RequestedBy: requestedBy,
		SandboxID:   sandbox.ID,
		// Every secret is a token, and a token is what the swap carries:
		// ResolveSandboxSecret emits Value.Token, so nothing a request can name
		// is a credential the proxy could not substitute.
		Type:          model.SecretTypeToken,
		Hosts:         hosts,
		Name:          name,
		EnvName:       envName,
		Justification: strings.TrimSpace(input.Justification.Or("")),
		Uses:          uses,
		GrantTTL:      grantTTL,
		Purpose:       purpose,
		WellKnownID:   wellKnownID,
		Status:        model.SecretRequestStatusPending,
	}
	if err := s.store.CreateSecretRequest(ctx, req); err != nil {
		return nil, err
	}
	return req, nil
}

// asksTheSame reports whether an open request asks for what a new ask does:
// the same uses, in the same order, for the same lifetime. The justification
// is left out; it argues for a grant and does not change which one approval
// mints, so a retry reworded is still a retry.
func asksTheSame(open model.SecretRequest, uses []model.SecretUse, grantTTL int64) bool {
	if open.GrantTTL != grantTTL || len(open.Uses) != len(uses) {
		return false
	}
	for i := range uses {
		if open.Uses[i].Description != uses[i].Description {
			return false
		}
	}
	return true
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

// ListCredentialVerdicts returns the project's recorded verdicts matching
// filter, newest first. It deliberately does not look the sandbox up: a
// verdict is kept past its sandbox's purge (ADR 0091), so requiring the
// sandbox to exist would hide exactly the trails most worth reading.
func (s *Service) ListCredentialVerdicts(ctx context.Context, projectID string, filter store.CredentialVerdictFilter) ([]model.CredentialVerdict, error) {
	return s.store.ListCredentialVerdicts(ctx, projectID, filter)
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
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	if strings.TrimSpace(sandbox.PoolID) != strings.TrimSpace(poolID) {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "sandbox not found")
	}
	return sandbox, nil
}

// ApprovedUse names what a request carrying this use may be judged against:
// the sentence a person approved, the credential in the words they read it as,
// and the one of the grant's hosts this request falls under (ADR 26-10-02-393
// §2) — where it is approved for, not a list the judge must match itself.
//
// It is the control plane's to answer and not the pool's to assert (ADR 26-09-22-838
// §4). It refuses unless the use, the credential, the discobox and the
// destination all still belong to one live grant — the same listing a resolve
// is matched against, so a use that can be judged is a use that could be
// spent. Everything it reads is current: a grant edited or revoked since the
// activation was minted is read as it is now, which is what makes asking again
// after a verdict worth anything.
func (s *Service) ApprovedUse(ctx context.Context, poolID, sandboxID, useID, host string) (services.ApprovedUse, error) {
	useID = strings.TrimSpace(useID)
	if useID == "" {
		return services.ApprovedUse{}, apperrors.NewStatusError(http.StatusBadRequest, "use ID is required")
	}
	sandbox, err := s.sandboxOwnedByPool(ctx, poolID, sandboxID)
	if err != nil {
		return services.ApprovedUse{}, err
	}
	credentials, err := s.store.ListLiveAgentCredentials(ctx, sandbox.ProjectID, sandbox.ID, store.SandboxGrantScopes(sandbox))
	if err != nil {
		return services.ApprovedUse{}, err
	}
	host = normalizeHost(host)
	for _, credential := range credentials {
		use, ok := credential.Grant.FindUse(useID)
		if !ok {
			continue
		}
		// The use is live. Whether it covers where this request is going is a
		// separate question, and the answer is no rather than a different use:
		// an approved use is approved for somewhere.
		approvedFor, covered := hostscope.Covering(credential.Grant.Hosts, host)
		if !covered {
			return services.ApprovedUse{}, apperrors.NewStatusError(http.StatusForbidden,
				fmt.Sprintf("that use is not approved for %s", host))
		}
		return approvedCredentialUse(credential, use, approvedFor), nil
	}
	// Not a credential's use. It may still be a host trust's: a person who
	// pins a host approves uses for it the same way, and every request to that
	// host is judged against them whether or not it carries a credential
	// (ADR 0149 §5). Those uses live on the trust rather than on a grant,
	// which is why this is a second lookup rather than a wider first one.
	if use, ok, err := s.trustedUse(ctx, sandbox, useID, host); err != nil || ok {
		return use, err
	}
	// Revoked, expired, edited away, or never this discobox's. They are one
	// answer here on purpose: which it was is the approval trail's to say, and
	// saying it back to a pool would describe grants it is not party to.
	return services.ApprovedUse{}, apperrors.NewStatusError(http.StatusForbidden, "no live approved use by that ID")
}

// ApprovedCredentialUse names what a command run under this use may be judged
// against (ADR 26-09-22-838 §3): ApprovedUse for a command rather than a
// request. There is no destination to check yet, since nothing has been sent;
// the hosts are the grant's, every one of them, and the request that follows
// is held to them. Only a
// credential's use takes a value, so a host trust's is not one.
func (s *Service) ApprovedCredentialUse(ctx context.Context, poolID, sandboxID, useID string) (services.ApprovedUse, error) {
	useID = strings.TrimSpace(useID)
	if useID == "" {
		return services.ApprovedUse{}, apperrors.NewStatusError(http.StatusBadRequest, "use ID is required")
	}
	sandbox, err := s.sandboxOwnedByPool(ctx, poolID, sandboxID)
	if err != nil {
		return services.ApprovedUse{}, err
	}
	credentials, err := s.store.ListLiveAgentCredentials(ctx, sandbox.ProjectID, sandbox.ID, store.SandboxGrantScopes(sandbox))
	if err != nil {
		return services.ApprovedUse{}, err
	}
	for _, credential := range credentials {
		if use, ok := credential.Grant.FindUse(useID); ok {
			return approvedCredentialUse(credential, use, strings.Join(credential.Grant.Hosts, ", ")), nil
		}
	}
	return services.ApprovedUse{}, apperrors.NewStatusError(http.StatusForbidden, "no live approved use by that ID")
}

func approvedCredentialUse(credential store.AgentCredential, use model.SecretUse, host string) services.ApprovedUse {
	return services.ApprovedUse{
		Purpose:    use.Description,
		Credential: credential.Name,
		Host:       host,
		GrantID:    credential.Grant.ID,
	}
}

// trustedUse names a use a host trust was granted for. It reports ok only for
// a use of a live trust of this discobox, for the host the request is going
// to; a trust is pinned to one endpoint, so the host must be that endpoint's
// and not merely beneath it the way a credential's grant allows.
//
// There is no credential to name: what was approved is reaching the host at
// all, so the judge is asked about the use and the destination alone.
func (s *Service) trustedUse(ctx context.Context, sandbox *model.Sandbox, useID, host string) (services.ApprovedUse, bool, error) {
	trusts, err := s.store.ListLiveSandboxHostTrusts(ctx, sandbox.ProjectID, sandbox.ID, time.Now().UTC())
	if err != nil {
		return services.ApprovedUse{}, false, err
	}
	for _, trust := range trusts {
		for _, use := range trust.Uses {
			if use.UseID != useID || useID == "" {
				continue
			}
			if trusted := hostscope.Normalize(trust.Host); trusted != host {
				return services.ApprovedUse{}, false, apperrors.NewStatusError(http.StatusForbidden,
					fmt.Sprintf("that use is not approved for %s", host))
			}
			return services.ApprovedUse{Purpose: use.Description, Host: hostscope.Normalize(trust.Host)}, true, nil
		}
	}
	return services.ApprovedUse{}, false, nil
}
