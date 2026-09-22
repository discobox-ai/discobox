package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/wellknown"
)

// SandboxGrants are the use grants a discobox is created with and the agent
// bindings that deliver them, built and checked but not yet stored.
type SandboxGrants struct {
	Grants   []*model.SecretGrant
	Bindings []*model.SandboxSecret
}

// PrepareSandboxGrants checks the uses a discobox is being created with and
// builds the grants and bindings that give them to it (ADR 0140 §4). They are
// stored by the create, in the transaction that stores the discobox, so a
// create whose grants cannot all be minted creates nothing.
//
// Each is held to what a person's grant of the same shape is held to: a
// concrete host within the secret's own binding, a lifetime within its limit,
// and at least one use. One environment variable carries one credential.
func PrepareSandboxGrants(ctx context.Context, st *store.Store, projectID, sandboxID string, requested []apimodel.SandboxGrant) (SandboxGrants, error) {
	var out SandboxGrants
	if len(requested) == 0 {
		return out, nil
	}
	principal, _ := auth.PrincipalFromContext(ctx)
	grantedBy := grantedByOf(principal)
	boundTo := map[string]string{}
	for _, in := range requested {
		secret, envName, host, err := sandboxGrantTarget(ctx, st, projectID, in)
		if err != nil {
			return SandboxGrants{}, err
		}
		if host == "" {
			return SandboxGrants{}, apperrors.NewStatusError(http.StatusBadRequest,
				fmt.Sprintf("a grant of %s for a new discobox requires a host; a wildcard grant stays an explicit administrative act", secret.Name))
		}
		if err := guardGrantHost(secret, host); err != nil {
			return SandboxGrants{}, err
		}
		ttl := in.GrantTTLSeconds.Or(secret.MaxGrantTTL)
		if err := guardGrantTTL(secret, ttl); err != nil {
			return SandboxGrants{}, err
		}
		uses, err := mintUseIDs(convertAPIUses(in.Uses))
		if err != nil {
			return SandboxGrants{}, err
		}
		if len(uses) == 0 {
			return SandboxGrants{}, apperrors.NewStatusError(http.StatusBadRequest, "a grant for a new discobox requires at least one use")
		}
		grant := &model.SecretGrant{
			ProjectID: projectID,
			SecretID:  secret.ID,
			Scope:     model.SecretGrantScopeSandbox,
			ScopeKey:  sandboxID,
			Host:      host,
			GrantedBy: grantedBy,
			Uses:      uses,
			EnvName:   envName,
			Purpose:   model.SecretGrantPurposeUse,
		}
		if ttl > 0 {
			expires := time.Now().UTC().Add(time.Duration(ttl) * time.Second)
			grant.ExpiresAt = &expires
		}
		out.Grants = append(out.Grants, grant)

		switch previous, bound := boundTo[envName]; {
		case !bound:
			binding, err := newAgentBinding(projectID, sandboxID, envName, secret)
			if err != nil {
				return SandboxGrants{}, err
			}
			out.Bindings = append(out.Bindings, binding)
			boundTo[envName] = secret.ID
		case previous != secret.ID:
			return SandboxGrants{}, apperrors.NewStatusError(http.StatusBadRequest,
				fmt.Sprintf("%s is given two different credentials; one environment variable carries one", envName))
		}
	}
	return out, nil
}

// sandboxGrantTarget is the secret, variable, and host one grant for a new
// discobox names: by a well-known ID, whose marked secret answers it and whose
// variable and host it carries, or by a secret and a variable spelled out.
//
// A gate is not given this way. Giving a discobox the discobox API lets it
// create discoboxes and give them credentials in turn, so a person grants it,
// by approving a discobox's own request (ADR 0140 §4).
func sandboxGrantTarget(ctx context.Context, st *store.Store, projectID string, in apimodel.SandboxGrant) (*model.Secret, string, string, error) {
	bad := func(format string, args ...any) (*model.Secret, string, string, error) {
		return nil, "", "", apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf(format, args...))
	}
	id := strings.TrimSpace(in.WellKnownId.Or(""))
	secretID, envName := strings.TrimSpace(in.SecretId.Or("")), strings.TrimSpace(in.EnvVar.Or(""))
	if id == "" {
		if secretID == "" {
			return bad("a grant for a new discobox names a secret, or a well-known credential by its ID")
		}
		secret, err := st.GetSecret(ctx, projectID, secretID)
		if err != nil {
			return nil, "", "", apperrors.NotFound(err, "secret not found")
		}
		// Named by its secret's ID, a gate is still a gate: the rule is about
		// the credential, not the spelling.
		if isGateSecret(secret) {
			return nil, "", "", gateGivenOnlyByAPerson(secret.WellKnownID)
		}
		if envName == "" || strings.ContainsAny(envName, "=\x00") {
			return bad("a grant for a new discobox requires the environment variable its agent receives it in")
		}
		return secret, envName, normalizeHost(in.Host.Or(secret.Host)), nil
	}
	if secretID != "" || envName != "" {
		return bad("%s names its own secret and variable; give the ID or spell them out, not both", id)
	}
	if known, ok := wellknown.Lookup(id); ok && known.Gate {
		return nil, "", "", gateGivenOnlyByAPerson(id)
	}
	_, envName, host, err := wellKnownAsk(id, "", "", normalizeHost(in.Host.Or("")))
	if err != nil {
		return nil, "", "", err
	}
	secret, err := st.FindSecretByWellKnownID(ctx, projectID, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return bad("no secret fulfills %s yet: a person marks one by approving the first request for it", id)
		}
		return nil, "", "", err
	}
	return secret, envName, host, nil
}

// gateGivenOnlyByAPerson refuses handing a gate on. A discobox that could give
// the discobox API could give every credential onward without a person seeing
// it, so the API is granted only by a person approving a discobox's own
// request for it (ADR 0140 §4) — never at create, and never by a discobox
// answering the inbox.
func gateGivenOnlyByAPerson(id string) error {
	return apperrors.NewStatusError(http.StatusForbidden,
		fmt.Sprintf("%s is granted by a person, approving a discobox's own request for it", id))
}
