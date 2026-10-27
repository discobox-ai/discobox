package secrets

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A discobox approving a request, or binding a secret on a discobox it
// creates, hands on a credential, and may hand on only what it was delegated
// (ADR 26-10-27-880 §3): a live delegate binding covering it — on it, its
// harness config or its project — of the same secret. What it hands on is a
// use binding on the discobox it created, a child of that delegate binding:
// gone when the parent goes, no longer-lived than it, and held to the
// parent's routes as well as its own, since a request carrying it must pass
// every list.

// delegationsFor are the delegate bindings a discobox may hand a secret on
// under, and the secret: the one the approver named — by ID, ID prefix or
// name — among those it was delegated, or the only one when it names none.
// The approver never looks among the project's secrets, only among what it
// was delegated, so a secret it was not delegated answers the same as one
// that does not exist. They are longest-lived first.
func (s *Service) delegationsFor(ctx context.Context, projectID, approverID, chosen string) ([]model.SecretBinding, *model.Secret, error) {
	delegations, err := s.delegateBindingsOf(ctx, projectID, approverID)
	if err != nil {
		return nil, nil, err
	}
	if len(delegations) == 0 {
		return nil, nil, apperrors.NewStatusError(http.StatusForbidden,
			"this discobox holds no delegate binding, so it hands nothing on; ask a person for one with `discobox-access request --delegate`, or leave the request for a person")
	}
	fits := map[string]*model.Secret{}
	bySecret := map[string][]model.SecretBinding{}
	for _, delegation := range delegations {
		secret, ok := fits[delegation.SecretID]
		if !ok {
			if secret, err = s.store.GetSecret(ctx, projectID, delegation.SecretID); err != nil {
				continue
			}
		}
		if chosen != "" && secret.Name != chosen && !strings.HasPrefix(secret.ID, chosen) {
			continue
		}
		fits[secret.ID] = secret
		bySecret[secret.ID] = append(bySecret[secret.ID], delegation)
	}
	switch len(fits) {
	case 0:
		return nil, nil, apperrors.NewStatusError(http.StatusForbidden,
			"this discobox holds no delegate binding of that credential, so it cannot hand it on; leave it for a person")
	case 1:
	default:
		names := make([]string, 0, len(fits))
		for _, secret := range fits {
			names = append(names, secret.Name+" ("+secret.ID+")")
		}
		sort.Strings(names)
		return nil, nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf(
			"this discobox was delegated more than one secret; name which with --secret: %s", strings.Join(names, ", ")))
	}
	var secret *model.Secret
	for _, only := range fits {
		secret = only
	}
	parents := bySecret[secret.ID]
	slices.SortStableFunc(parents, func(a, b model.SecretBinding) int { return -lifetimeOrder(&a, &b) })
	return parents, secret, nil
}

// delegateBindingsOf are the live delegate bindings covering a discobox, at
// any level.
func (s *Service) delegateBindingsOf(ctx context.Context, projectID, sandboxID string) ([]model.SecretBinding, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	effective, err := s.store.ResolveSandboxBindings(ctx, sandbox, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	out := make([]model.SecretBinding, 0, len(effective.Delegate))
	for _, delegation := range effective.Delegate {
		out = append(out, delegation.Binding)
	}
	return out, nil
}

// heldParent reads, in the transaction that hands a secret on, the first of
// the chosen delegate bindings that still covers the discobox handing it on:
// present, live, a delegation of the secret, on a level covering it. One
// removed or lapsed since it was chosen is not one this is made under.
func heldParent(ctx context.Context, txStore *store.Store, projectID, holderID, secretID string, chosen []model.SecretBinding, now time.Time) (*model.SecretBinding, error) {
	holder, err := txStore.GetSandbox(ctx, projectID, holderID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	for i := range chosen {
		parent, err := txStore.SecretBindingByExactID(ctx, projectID, chosen[i].ID)
		if err != nil {
			continue
		}
		if parent.Delegate && parent.SecretID == secretID && parent.Live(now) && store.Covers(store.SandboxBindingTargets(holder), parent, true) {
			return parent, nil
		}
	}
	return nil, apperrors.NewStatusError(http.StatusForbidden,
		"the delegate binding this would be handed on under is gone or no longer covers this discobox; try again, or leave it for a person")
}

// fitToParent is the lifetime of a binding handed on under parent: one the
// grantor named must end no later than the parent, and one it did not is
// fitted to what the parent has left. Zero is forever, which a parent that
// lapses cannot give, and so is what a parent with under a second left would
// fit to: that is refused rather than made as a binding that never expires.
func fitToParent(parent *model.SecretBinding, ttl int64, named bool, now time.Time) (int64, error) {
	if parent.ExpiresAt == nil {
		return ttl, nil
	}
	remaining := int64(parent.ExpiresAt.Sub(now) / time.Second)
	if remaining < 1 {
		return 0, apperrors.NewStatusError(http.StatusForbidden,
			"the delegate binding this would be handed on under is lapsing, with nothing left to hand on; leave it for a person")
	}
	if named {
		if ttl <= 0 || ttl > remaining {
			return 0, apperrors.NewStatusError(http.StatusForbidden,
				"a binding this discobox hands on may not outlast the delegate binding it is made under; leave the lifetime out to fit it, or make it for less")
		}
		return ttl, nil
	}
	if ttl <= 0 || ttl > remaining {
		return remaining, nil
	}
	return ttl, nil
}

// lifetimeOrder compares two bindings by how long one handed on under them
// may last: one that never lapses longest, else the one that lapses last.
func lifetimeOrder(a, b *model.SecretBinding) int {
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
