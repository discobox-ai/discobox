package secrets

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/hostscope"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A discobox approving a request, or giving a grant on a create, hands on a
// credential, and may hand on only what it was delegated (ADR 26-10-27-880
// §3). The bound is static: a live delegation grant the discobox holds, of the
// same secret, whose hosts cover every host handed on and which ends no
// earlier than the grant does. Of the delegations that hold, the grant is made
// under the longest-lived, which is checked again by its ID in the write's
// transaction and recorded on the grant as its DelegationID.

// refuseDelegatedApproval says why a discobox may not approve this request at
// all, whatever it was delegated, or nothing when it may try.
func refuseDelegatedApproval(req *model.SecretRequest) error {
	if req.Purpose == model.SecretGrantPurposeDelegate {
		return apperrors.NewStatusError(http.StatusForbidden,
			"a discobox never approves a request to delegate: handing on the power to hand on is a person's to approve")
	}
	// A request the proxy opened on meeting a sentinel it could not resolve
	// names no variable to deliver the credential in, and is a person's.
	if !req.FromProtocol() {
		return apperrors.NewStatusError(http.StatusForbidden,
			"a discobox approves only an agent's request; this one is a person's to answer")
	}
	return nil
}

// delegationsFor are the delegation grants a discobox may approve a request
// under, and the secret it answers with: the secret of a live delegation it
// holds that fits the request — the secret the approver named, and hosts that
// cover every one asked for. The approver does not choose among the project's
// secrets, only among what it was delegated; when that is more than one
// secret, it names which.
//
// Of that secret's delegations, those a grant's lifetime fits are returned
// (delegationsFitting), longest-lived first: the approval is made under the
// first. ttl is the lifetime named, and is read only when named.
func (s *Service) delegationsFor(ctx context.Context, projectID, approverID string, chosenID string, hosts []string, ttl int64, named bool) ([]*model.SecretGrant, *model.Secret, error) {
	delegations, err := s.store.ListLiveDelegationGrants(ctx, projectID, approverID)
	if err != nil {
		return nil, nil, err
	}
	if len(delegations) == 0 {
		return nil, nil, apperrors.NewStatusError(http.StatusForbidden,
			"this discobox holds no delegation grant, so it hands nothing on; ask a person for one with `discobox-access request --delegate`, or leave the request for a person")
	}
	// The secret the approver named is matched only against what it was
	// delegated — its ID, or a prefix of it, as IDs are matched elsewhere —
	// and never looked up among the project's: a secret it was not delegated
	// answers the same as one that does not exist.
	fits := map[string]*model.Secret{}
	bySecret := map[string][]*model.SecretGrant{}
	for i := range delegations {
		delegation := &delegations[i]
		if !hostscope.CoversEvery(delegation.Hosts, hosts) {
			continue
		}
		if chosenID != "" && !strings.HasPrefix(delegation.SecretID, chosenID) {
			continue
		}
		secret, ok := fits[delegation.SecretID]
		if !ok {
			if secret, err = s.store.GetSecret(ctx, projectID, delegation.SecretID); err != nil {
				continue
			}
		}
		fits[secret.ID] = secret
		bySecret[secret.ID] = append(bySecret[secret.ID], delegation)
	}
	switch len(fits) {
	case 0:
		return nil, nil, apperrors.NewStatusError(http.StatusForbidden, fmt.Sprintf(
			"no delegation grant this discobox holds hands on the credential this request asks for to %s; leave the request for a person", strings.Join(hosts, ", ")))
	case 1:
	default:
		// By name or ID: a discobox's listing holds the secrets it was
		// delegated, so either resolves.
		names := make([]string, 0, len(fits))
		for _, secret := range fits {
			names = append(names, secret.Name+" ("+secret.ID+")")
		}
		sort.Strings(names)
		return nil, nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf(
			"this discobox was delegated more than one secret that fits; name which with --secret: %s", strings.Join(names, ", ")))
	}
	var secret *model.Secret
	for _, only := range fits {
		secret = only
	}
	fitting, err := delegationsFitting(bySecret[secret.ID], ttl, named)
	if err != nil {
		return nil, nil, err
	}
	return fitting, secret, nil
}

// delegationsFitting are the delegations, of those that hold a grant's secret
// and hosts, its lifetime fits: one the grantor named must end no later than
// the delegation, and one it did not is fitted to what the delegation has left,
// which must be something. They are longest-lived first — the order a grant
// prefers them in, since one made under a delegation that lasts longer may
// last longer.
func delegationsFitting(delegations []*model.SecretGrant, ttl int64, named bool) ([]*model.SecretGrant, error) {
	now := time.Now().UTC()
	var fitting []*model.SecretGrant
	for _, delegation := range delegations {
		if named {
			if !outlastedBy(delegation, ttl, now) {
				continue
			}
		} else if _, err := fitTTL(delegation, ttl, now); err != nil {
			continue
		}
		fitting = append(fitting, delegation)
	}
	if len(fitting) == 0 {
		if named {
			return nil, outlastsDelegation()
		}
		return nil, lapsingDelegation()
	}
	slices.SortStableFunc(fitting, func(a, b *model.SecretGrant) int { return -lifetimeOrder(a, b) })
	return fitting, nil
}

// delegationsOf are the live delegations a discobox holds of one secret that
// cover every one of hosts: what it may hand that secret on under, there.
func (s *Service) delegationsOf(ctx context.Context, projectID, approverID, secretID string, hosts []string) ([]*model.SecretGrant, error) {
	delegations, err := s.store.ListLiveDelegationGrants(ctx, projectID, approverID)
	if err != nil {
		return nil, err
	}
	var out []*model.SecretGrant
	for i := range delegations {
		if delegations[i].SecretID == secretID && hostscope.CoversEvery(delegations[i].Hosts, hosts) {
			out = append(out, &delegations[i])
		}
	}
	return out, nil
}

// delegatedTTL checks, in the grant's transaction, that the delegation it was
// chosen under still bounds it — still the approver's, still live, still of
// this secret and covering the hosts — and returns the grant's lifetime within
// it: a lifetime nobody named is fitted to the delegation's remaining time,
// and one the approver named must fit or is refused. A delegation revoked or
// lapsed since it was chosen is not one this grant is made under, whatever
// else the approver holds.
//
// now is the instant the grant's lifetime runs from, the one mintGrantAs
// stamps its expiry from: a lifetime fitted at one reading of the clock and
// stamped from a later one would end after the delegation by the time between
// them.
func delegatedTTL(ctx context.Context, txStore *store.Store, projectID, approverID string, chosen *model.SecretGrant, secretID string, hosts []string, ttl int64, named bool, now time.Time) (int64, error) {
	gone := apperrors.NewStatusError(http.StatusForbidden,
		"the delegation grant this was made under is gone or no longer covers it; try again, or leave it for a person")
	delegation, ok := heldDelegation(ctx, txStore, projectID, approverID, chosen, secretID, hosts, now)
	if !ok {
		return 0, gone
	}
	if !named {
		return fitTTL(delegation, ttl, now)
	}
	if !outlastedBy(delegation, ttl, now) {
		return 0, outlastsDelegation()
	}
	return ttl, nil
}

// heldDelegation reads the delegation a grant was chosen under again, and
// reports whether it still bounds it: still the grantor's delegation, live, of
// the same secret, covering the hosts.
func heldDelegation(ctx context.Context, txStore *store.Store, projectID, approverID string, chosen *model.SecretGrant, secretID string, hosts []string, now time.Time) (*model.SecretGrant, bool) {
	delegation, err := txStore.GetSecretGrant(ctx, projectID, chosen.ID)
	if err != nil {
		return nil, false
	}
	if delegation.Purpose != model.SecretGrantPurposeDelegate || delegation.Scope != model.SecretGrantScopeSandbox ||
		delegation.ScopeKey != approverID || delegation.SecretID != secretID || !hostscope.CoversEvery(delegation.Hosts, hosts) ||
		(delegation.ExpiresAt != nil && !delegation.ExpiresAt.After(now)) {
		return nil, false
	}
	return delegation, true
}

// fitTTL is a lifetime nobody named, fitted to what a delegation has left. Zero
// is forever, which no delegation that lapses can give — and so is what a
// delegation with less than a second left would fit to, which is refused
// rather than minted as a grant that never expires.
func fitTTL(delegation *model.SecretGrant, ttl int64, now time.Time) (int64, error) {
	if delegation.ExpiresAt == nil {
		return ttl, nil
	}
	remaining := int64(delegation.ExpiresAt.Sub(now) / time.Second)
	if remaining < 1 {
		return 0, lapsingDelegation()
	}
	if ttl <= 0 || ttl > remaining {
		return remaining, nil
	}
	return ttl, nil
}

// lifetimeOrder compares two delegations by how long a grant made under them
// may last: one that never lapses longest, else the one that lapses last.
func lifetimeOrder(a, b *model.SecretGrant) int {
	switch {
	case a.ExpiresAt == nil && b.ExpiresAt == nil:
		return 0
	case a.ExpiresAt == nil:
		return 1
	case b.ExpiresAt == nil:
		return -1
	}
	return a.ExpiresAt.Compare(*b.ExpiresAt)
}

// outlastedBy reports whether a grant of ttl seconds — zero is forever — ends
// no later than the delegation it is made under.
func outlastedBy(delegation *model.SecretGrant, ttl int64, now time.Time) bool {
	if delegation.ExpiresAt == nil {
		return true
	}
	return ttl > 0 && !now.Add(time.Duration(ttl)*time.Second).After(*delegation.ExpiresAt)
}

func outlastsDelegation() error {
	return apperrors.NewStatusError(http.StatusForbidden,
		"a grant this discobox hands on may not outlast the delegation grant it is made under; leave the lifetime out to fit it, or grant it for less")
}

func lapsingDelegation() error {
	return apperrors.NewStatusError(http.StatusForbidden,
		"the delegation grant this would be made under is lapsing, with nothing left to hand on; leave it for a person")
}
