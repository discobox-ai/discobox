package store

import (
	"context"
	"slices"
	"time"

	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// BodyPlaceRefusal is the first of lists (a credential's RouteLists) that
// allows nowhere a body place an open route of grant names, as its index,
// with secretroute.BodyPlacesAllowed's reason; -1 and nil when none refuses.
// Every list is held at once and `*` grants no body place, so a grant a list
// refuses is one the proxy refuses on every use. The one predicate both
// directions share: a binding written against its secret (#1097), and a
// secret written under its bindings (#1105).
func BodyPlaceRefusal(grant []secretroute.Route, lists [][]secretroute.Route) (int, error) {
	for i, list := range lists {
		if err := secretroute.BodyPlacesAllowed(grant, list); err != nil {
			return i, err
		}
	}
	return -1, nil
}

// StrandedBinding is a standing binding a change to its secret's route lists
// strands, and why: the body place a route of its names that a list it is now
// held to allows nowhere. Stranded are those of its routes, in order.
type StrandedBinding struct {
	Binding  model.SecretBinding
	Stranded []model.SecretRoute
	Reason   error
}

// StrandedBodyPlaceBindings lists the standing bindings of secret that a
// change to the secret's own routes or route constraints, from prior's,
// strands (#1105): a route of the binding names a body place that every list
// it was held to allowed somewhere, and some list it is now held to allows
// nowhere on that route's hosts and methods (secretroute.BodyPlacesAllowed).
// Such a route would stay reported as granted while the proxy refused it on
// every use; each is judged on its own (Stranded), so a binding's other
// routes, which still work, can be kept. One the change does not make unusable is not its to
// answer for: a route already refused before it, or a binding held before it
// to an empty list, which sends the credential nowhere at all (a route-less
// secret, a retired exchange secret). A binding handed on is judged under its
// parent as its parent is left without its stranded routes, parents coming
// first since bindings come oldest first. A lapsed one, or one under a lapsed
// parent, is not standing and is left to expiry.
//
// Called inside the transaction that writes secret, so it reads the bindings
// that write is held against.
func (s *Store) StrandedBodyPlaceBindings(ctx context.Context, prior, secret *model.Secret, now time.Time) ([]StrandedBinding, error) {
	bindings, err := s.ListSecretBindings(ctx, secret.ProjectID, SecretBindingFilter{SecretID: secret.ID})
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*model.SecretBinding, len(bindings))
	left := make(map[string]*model.SecretBinding, len(bindings))
	for i := range bindings {
		byID[bindings[i].ID] = &bindings[i]
		left[bindings[i].ID] = &bindings[i]
	}
	var stranded []StrandedBinding
	for i := range bindings {
		binding := &bindings[i]
		var parent, parentLeft *model.SecretBinding
		if binding.ParentID != "" {
			if parent, parentLeft = byID[binding.ParentID], left[binding.ParentID]; parent == nil {
				continue
			}
		}
		if expires := BindingExpiry(binding, parent); expires != nil && !now.Before(*expires) {
			continue
		}
		before := RouteLists(prior, binding, parent)
		if slices.ContainsFunc(before, func(l []secretroute.Route) bool { return len(l) == 0 }) {
			continue
		}
		after := RouteLists(secret, binding, parentLeft)
		var kept, lost []model.SecretRoute
		var reason error
		for _, route := range binding.Routes {
			_, err := BodyPlaceRefusal([]secretroute.Route{route}, after)
			if was, _ := BodyPlaceRefusal([]secretroute.Route{route}, before); err != nil && was < 0 {
				if reason == nil {
					reason = err
				}
				lost = append(lost, route)
				continue
			}
			kept = append(kept, route)
		}
		if reason == nil {
			continue
		}
		narrowed := *binding
		narrowed.Routes = kept
		left[binding.ID] = &narrowed
		stranded = append(stranded, StrandedBinding{Binding: *binding, Stranded: lost, Reason: reason})
	}
	return stranded, nil
}
