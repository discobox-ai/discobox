package secrets

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/hostscope"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// SandboxGrants are the use grants a discobox is created with and the
// bindings that deliver them, built and checked but not yet stored.
type SandboxGrants struct {
	Grants   []*model.SecretGrant
	Bindings []*model.SandboxSecret
	// Delegations are, when a discobox gives the grants, the delegation grant
	// each grant is made under, by index (ADR 26-10-27-880 §3); nil when a
	// person gives them. The create holds each grant to its delegation again
	// in its transaction (HoldDelegations).
	Delegations []*model.SecretGrant
	// Grantor is the discobox giving the grants, when one is.
	Grantor string
}

// PrepareSandboxGrants checks the credentials a discobox is being created
// with and builds the grants, and the bindings that deliver them, that give
// them to it (ADR 26-10-27-880 §5). They are stored by the create, in the
// transaction that stores the discobox, so a create whose grants cannot all
// be minted creates nothing. A binding is an ordinary injected one: the new
// discobox finds a sentinel in the variable named, which its pool swaps on
// the hosts the grant covers.
//
// Each is held to what a person's grant of the same shape is held to: a
// concrete host, every one within the secret's own binding, and a lifetime
// within its limit. One environment variable carries one credential.
//
// A discobox giving them is held to what it may hand on, as when it approves a
// request: each grant is made under a live delegation grant it holds of that
// secret, covering the hosts and fitting the lifetime — the longest-lived that
// does. A lifetime it did not name is fitted to that delegation.
func (s *Service) PrepareSandboxGrants(ctx context.Context, projectID string, sandbox *model.Sandbox, requested []apimodel.SandboxGrant) (SandboxGrants, error) {
	sandboxID := sandbox.ID
	var out SandboxGrants
	if len(requested) == 0 {
		return out, nil
	}
	st := s.store
	principal, _ := auth.PrincipalFromContext(ctx)
	grantedBy := grantedByOf(principal)
	byDiscobox := principal.Type == auth.PrincipalTypeSandbox
	if byDiscobox {
		out.Grantor = principal.SandboxID
	}
	now := time.Now().UTC()
	boundTo := map[string]string{}
	for _, in := range requested {
		if byDiscobox {
			var err error
			if in, err = s.delegatedGrant(ctx, projectID, principal.SandboxID, in); err != nil {
				return SandboxGrants{}, err
			}
		}
		secret, envName, hosts, err := sandboxGrantTarget(ctx, st, projectID, in)
		if err != nil {
			return SandboxGrants{}, err
		}
		if len(hosts) == 0 {
			return SandboxGrants{}, apperrors.NewStatusError(http.StatusBadRequest,
				fmt.Sprintf("a grant of %s for a new discobox requires a host; a wildcard grant stays an explicit administrative act", secret.Name))
		}
		if err := guardGrantHosts(secret, hosts); err != nil {
			return SandboxGrants{}, err
		}
		// A lifetime fitted to a delegation only ever shortens, never to
		// forever, so one within the secret's limit stays within it.
		ttl := in.GrantTTLSeconds.Or(secret.MaxGrantTTL)
		var delegation *model.SecretGrant
		if byDiscobox {
			delegations, err := s.delegationsOf(ctx, projectID, principal.SandboxID, secret.ID, hosts)
			if err != nil {
				return SandboxGrants{}, err
			}
			if len(delegations) == 0 {
				return SandboxGrants{}, apperrors.NewStatusError(http.StatusForbidden, fmt.Sprintf(
					"no delegation grant this discobox holds hands on %s to %s, so it cannot give it to the discobox it creates; create it without the grant and let it ask", secret.Name, strings.Join(hosts, ", ")))
			}
			named := in.GrantTTLSeconds.IsSet()
			if delegations, err = delegationsFitting(delegations, ttl, named); err != nil {
				return SandboxGrants{}, err
			}
			delegation = delegations[0]
			if !named {
				if ttl, err = fitTTL(delegation, ttl, now); err != nil {
					return SandboxGrants{}, err
				}
			}
		}
		if err := guardGrantTTL(secret, ttl); err != nil {
			return SandboxGrants{}, err
		}
		grant := &model.SecretGrant{
			ProjectID: projectID,
			SecretID:  secret.ID,
			Scope:     model.SecretGrantScopeSandbox,
			ScopeKey:  sandboxID,
			Hosts:     hosts,
			GrantedBy: grantedBy,
			Purpose:   model.SecretGrantPurposeUse,
		}
		if delegation != nil {
			grant.DelegationID = delegation.ID
		}
		if ttl > 0 {
			expires := now.Add(time.Duration(ttl) * time.Second)
			grant.ExpiresAt = &expires
		}
		out.Grants = append(out.Grants, grant)
		out.Delegations = append(out.Delegations, delegation)

		switch previous, bound := boundTo[envName]; {
		case !bound:
			binding, err := st.NewSandboxBinding(ctx, projectID, sandboxID, envName, secret)
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

// delegatedGrant resolves the secret a grant a discobox gives names, among the
// secrets it was delegated and nothing else, before anything reads it: a
// secret named by ID, or a prefix of one, as IDs are matched elsewhere. A
// secret it was not delegated — or one that does not exist —
// answers with the same refusal, so naming secrets cannot tell it which the
// project holds, how they are bound, or what they are called (as for an
// approval's --secret).
func (s *Service) delegatedGrant(ctx context.Context, projectID, grantorID string, in apimodel.SandboxGrant) (apimodel.SandboxGrant, error) {
	notDelegated := apperrors.NewStatusError(http.StatusForbidden,
		"this discobox holds no delegation grant of that credential, so it cannot give it to the discobox it creates; create it without the grant and let it ask")
	delegations, err := s.store.ListLiveDelegationGrants(ctx, projectID, grantorID)
	if err != nil {
		return in, err
	}
	delegated := map[string]bool{}
	for i := range delegations {
		delegated[delegations[i].SecretID] = true
	}
	named := strings.TrimSpace(in.SecretId.Or(""))
	if named == "" {
		return in, nil
	}
	var matches []string
	for secretID := range delegated {
		if strings.HasPrefix(secretID, named) {
			matches = append(matches, secretID)
		}
	}
	switch len(matches) {
	case 0:
		return in, notDelegated
	case 1:
		in.SecretId.SetTo(matches[0])
		return in, nil
	}
	sort.Strings(matches)
	return in, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf(
		"%q names more than one secret this discobox was delegated; give more of its ID: %s", named, strings.Join(matches, ", ")))
}

// HoldDelegations holds each grant a discobox gives on a create to the
// delegation it was made under, read again in the create's transaction: still
// the grantor's, live, covering the hosts, and lasting at least as long as the
// grant. A delegation revoked or changed since
// the grants were prepared refuses the create, which then creates nothing.
func HoldDelegations(ctx context.Context, txStore *store.Store, grants SandboxGrants) error {
	now := time.Now().UTC()
	for i, chosen := range grants.Delegations {
		if chosen == nil {
			continue
		}
		grant := grants.Grants[i]
		delegation, ok := heldDelegation(ctx, txStore, grant.ProjectID, grants.Grantor, chosen, grant.SecretID, grant.Hosts, now)
		if !ok {
			return apperrors.NewStatusError(http.StatusForbidden,
				"a delegation grant this create's grants were made under is gone or no longer covers them; create it again, or without the grants")
		}
		if delegation.ExpiresAt != nil && (grant.ExpiresAt == nil || grant.ExpiresAt.After(*delegation.ExpiresAt)) {
			return outlastsDelegation()
		}
	}
	return nil
}

// sandboxGrantTarget is the secret, variable, and hosts one grant for a new
// discobox names. The hosts default to the secret's host.
func sandboxGrantTarget(ctx context.Context, st *store.Store, projectID string, in apimodel.SandboxGrant) (*model.Secret, string, []string, error) {
	bad := func(format string, args ...any) (*model.Secret, string, []string, error) {
		return nil, "", nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf(format, args...))
	}
	hosts, named := askedHosts(in.Hosts)
	secretID, envName := strings.TrimSpace(in.SecretId.Or("")), strings.TrimSpace(in.EnvVar.Or(""))
	if secretID == "" {
		return bad("a grant for a new discobox names a secret")
	}
	secret, err := st.GetSecret(ctx, projectID, secretID)
	if err != nil {
		return nil, "", nil, apperrors.NotFound(err, "secret not found")
	}
	if envName == "" || strings.ContainsAny(envName, "=\x00") {
		return bad("a grant for a new discobox requires the environment variable it receives the credential in")
	}
	if !named {
		hosts = hostscope.List(secret.Host)
	}
	return secret, envName, hosts, nil
}
