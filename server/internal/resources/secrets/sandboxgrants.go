package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/hostscope"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/wellknown"
	"golang.org/x/sync/errgroup"
)

// SandboxGrants are the use grants a discobox is created with and the agent
// bindings that deliver them, built and checked but not yet stored.
type SandboxGrants struct {
	Grants   []*model.SecretGrant
	Bindings []*model.SandboxSecret
	// Delegations are, when a discobox gives the uses, the delegation grant
	// each grant is made under, by index (ADR 26-09-30-782 §1); nil when a
	// person gives them. The create holds each grant to its delegation again
	// in its transaction (HoldDelegations).
	Delegations []*model.SecretGrant
	// Grantor is the discobox giving the uses, when one is.
	Grantor string
}

// PrepareSandboxGrants checks the uses a discobox is being created with and
// builds the grants and bindings that give them to it (ADR 0140 §4). They are
// stored by the create, in the transaction that stores the discobox, so a
// create whose grants cannot all be minted creates nothing.
//
// Each is held to what a person's grant of the same shape is held to: a
// concrete host, every one within the secret's own binding, a lifetime within its limit,
// and at least one use. One environment variable carries one credential.
//
// A discobox giving them is held to what it may hand on, as when it approves a
// request (ADR 26-09-30-782 §1): each grant is made under a live delegation
// grant it holds of that secret, covering the hosts and fitting the lifetime,
// whose uses the project's judge finds its uses within — the longest-lived
// that does, asked once every grant has passed what can refuse it without the
// judge. A lifetime it did not name is then fitted to that delegation.
func (s *Service) PrepareSandboxGrants(ctx context.Context, projectID, sandboxID string, requested []apimodel.SandboxGrant) (SandboxGrants, error) {
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
	var credentials []string
	// What each grant a discobox gives may be made under, and whether its
	// lifetime was named, by index: which delegation it is made under, and so
	// how long a lifetime nobody named lasts, waits for the judge.
	var fitting [][]*model.SecretGrant
	var namedTTL []bool
	var ttls []int64
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
		// A lifetime fitted to a delegation later only ever shortens, never
		// to forever, so one within the secret's limit now stays within it.
		ttl := in.GrantTTLSeconds.Or(secret.MaxGrantTTL)
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
			fitting = append(fitting, delegations)
			namedTTL = append(namedTTL, named)
			ttls = append(ttls, ttl)
		}
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
			Hosts:     hosts,
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
		out.Delegations = append(out.Delegations, nil)
		credential := secret.Name
		if id := strings.TrimSpace(in.WellKnownId.Or("")); id != "" {
			credential = id
		}
		credentials = append(credentials, credential)

		switch previous, bound := boundTo[envName]; {
		case !bound:
			binding, err := st.NewAgentBinding(ctx, projectID, sandboxID, envName, secret)
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
	if byDiscobox {
		// Grants are asked about at once, not one after another: the create
		// is a discobox's own call, held open by its pool's gate for two
		// minutes, and each grant's asks are bounded together to fit inside
		// that (services.DelegationBound) — one grant after another, two slow
		// ones would not. The first refusal cancels the rest, and is the
		// answer.
		judging, judgingCtx := errgroup.WithContext(ctx)
		for i, grant := range out.Grants {
			judging.Go(func() error {
				delegation, err := s.judgeDelegations(judgingCtx, projectID, principal.SandboxID, fitting[i], credentials[i], grant.Hosts, grant.Uses, "", sandboxID)
				out.Delegations[i] = delegation
				return err
			})
		}
		if err := judging.Wait(); err != nil {
			return SandboxGrants{}, err
		}
		now := time.Now().UTC()
		for i, grant := range out.Grants {
			if namedTTL[i] {
				continue
			}
			ttl, err := fitTTL(out.Delegations[i], ttls[i], now)
			if err != nil {
				return SandboxGrants{}, err
			}
			grant.ExpiresAt = nil
			if ttl > 0 {
				expires := now.Add(time.Duration(ttl) * time.Second)
				grant.ExpiresAt = &expires
			}
		}
	}
	return out, nil
}

// delegatedGrant resolves the secret a grant a discobox gives names, among the
// secrets it was delegated and nothing else, before anything reads it: a
// secret named by ID, or a prefix of one, as IDs are matched elsewhere; a
// well-known credential by the secret marked for it, which must be one it was
// delegated. A secret it was not delegated — or one that does not exist —
// answers with the same refusal, so naming secrets cannot tell it which the
// project holds, how they are bound, or what they are called (as for an
// approval's --secret-id).
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
	if id := strings.TrimSpace(in.WellKnownId.Or("")); id != "" {
		marked, err := s.store.FindSecretByWellKnownID(ctx, projectID, id)
		if err != nil || !delegated[marked.ID] {
			return in, notDelegated
		}
		return in, nil
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
// the grantor's, live, covering the hosts, with the uses the judge read, and
// lasting at least as long as the grant. A delegation revoked or changed since
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
// discobox names: by a well-known ID, whose marked secret answers it and whose
// variable and host it carries, or by a secret and a variable spelled out. The
// hosts default to the secret's host, or the well-known credential's first.
//
// A gate is not given this way. Giving a discobox the discobox API lets it
// create discoboxes and give them credentials in turn, so a person grants it,
// by approving a discobox's own request (ADR 0140 §4).
func sandboxGrantTarget(ctx context.Context, st *store.Store, projectID string, in apimodel.SandboxGrant) (*model.Secret, string, []string, error) {
	bad := func(format string, args ...any) (*model.Secret, string, []string, error) {
		return nil, "", nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf(format, args...))
	}
	hosts, named := askedHosts(in.Hosts)
	id := strings.TrimSpace(in.WellKnownId.Or(""))
	secretID, envName := strings.TrimSpace(in.SecretId.Or("")), strings.TrimSpace(in.EnvVar.Or(""))
	if id == "" {
		if secretID == "" {
			return bad("a grant for a new discobox names a secret, or a well-known credential by its ID")
		}
		secret, err := st.GetSecret(ctx, projectID, secretID)
		if err != nil {
			return nil, "", nil, apperrors.NotFound(err, "secret not found")
		}
		// Named by its secret's ID, a gate is still a gate: the rule is about
		// the credential, not the spelling.
		if isGateSecret(secret) {
			return nil, "", nil, gateGivenOnlyByAPerson(secret.WellKnownID)
		}
		if envName == "" || strings.ContainsAny(envName, "=\x00") {
			return bad("a grant for a new discobox requires the environment variable its agent receives it in")
		}
		if !named {
			hosts = hostscope.List(secret.Host)
		}
		return secret, envName, hosts, nil
	}
	if secretID != "" || envName != "" {
		return bad("%s names its own secret and variable; give the ID or spell them out, not both", id)
	}
	if known, ok := wellknown.Lookup(id); ok && known.Gate {
		return nil, "", nil, gateGivenOnlyByAPerson(id)
	}
	_, envName, hosts, err := wellKnownAsk(id, "", "", hosts)
	if err != nil {
		return nil, "", nil, err
	}
	secret, err := st.FindSecretByWellKnownID(ctx, projectID, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return bad("no secret fulfills %s yet: a person marks one by approving the first request for it", id)
		}
		return nil, "", nil, err
	}
	return secret, envName, hosts, nil
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
