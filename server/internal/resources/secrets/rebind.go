package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/bindingplan"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A person may rebind one of a discobox's variables from one secret to
// another (ADR 26-11-19-927), directly (CreateSecretBinding) or by approving
// a request (commitApproval), and either way names the binding it replaces,
// so what is swapped is what the person saw. The replaced binding loses its
// use in the same transaction that writes the new one (bindingplan.Rebind):
// one that also delegates is split and keeps its delegation and its
// children; the old secret's additions in the variable go. The variable's
// sentinel row is repointed when the discobox is materialized again, and
// keeps its sentinel unless the two secrets' formats differ. One rebound
// event, on the new binding, names both (store.BindingChange.Replaced).

// rebindPath is which door a rebind comes through: a person's direct create
// may take over more than a person's approval of the discobox's own ask (ADR
// 26-11-19-927 §3).
type rebindPath int

const (
	rebindDirect rebindPath = iota
	rebindApproval
)

// rebind clears want's variable on its discobox of the discobox's own binding
// of another secret, when replaces names it, in the caller's transaction, and
// returns that binding as it stood for the rebound event (nil when the
// variable held no other secret) and every binding it touched, for marking.
// Which binding delivers the variable is the store's resolution
// (EffectiveBindings.UseFor), the one a preview reads. With replaces empty, a
// variable the discobox's own binding of another secret delivers is refused
// naming it; with it set, a binding that no longer delivers the variable, one
// of want's own secret, and one this path may not touch are refused, each
// saying which. What becomes of each binding is bindingplan.Rebind's.
func (s *Service) rebind(ctx context.Context, txStore *store.Store, want *model.SecretBinding, replaces string, path rebindPath, change store.BindingChange) (*model.SecretBinding, []model.SecretBinding, error) {
	sandbox, err := txStore.GetSandbox(ctx, want.ProjectID, want.TargetID)
	if err != nil {
		return nil, nil, apperrors.NotFound(err, "sandbox not found")
	}
	effective, err := txStore.ResolveSandboxBindings(ctx, sandbox, time.Now().UTC())
	if err != nil {
		return nil, nil, err
	}
	var current *model.SecretBinding
	if applied, ok := effective.UseFor(want.EnvName); ok && applied.Binding.TargetKind == model.SecretBindingTargetSandbox && applied.Binding.SecretID != want.SecretID {
		current = &applied.Binding
	}
	replaces = strings.TrimSpace(replaces)
	if replaces == "" {
		if current == nil {
			return nil, nil, nil
		}
		return nil, nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
			"%s already holds %s in this discobox (binding %s); a person rebinds it to this secret by naming that binding it replaces, or asks for another variable",
			want.EnvName, s.secretLabel(ctx, txStore, current), current.ID))
	}
	if named, err := txStore.SecretBindingByExactID(ctx, want.ProjectID, replaces); err == nil && named.SecretID == want.SecretID {
		return nil, nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
			"binding %s is of the secret being bound, so there is nothing to rebind; grant it without naming a binding to replace", named.ID))
	}
	if current == nil {
		return nil, nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
			"binding %s no longer delivers %s, and no binding of another secret does in this discobox; review it again", replaces, want.EnvName))
	}
	if replaces != current.ID {
		return nil, nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
			"binding %s no longer delivers %s; binding %s of %s does now; review it again", replaces, want.EnvName, current.ID, s.secretLabel(ctx, txStore, current)))
	}
	// Read again locked: a person's edit committed since the resolution is
	// either seen here or waits for this rebind.
	locked, err := txStore.LockSecretBinding(ctx, current.ProjectID, current.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (!locked.Use || locked.EnvName != want.EnvName || locked.SecretID != current.SecretID)) {
		return nil, nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
			"binding %s changed while it was being rebound; review it again", current.ID))
	}
	if err != nil {
		return nil, nil, err
	}
	standing, err := txStore.ListSecretBindings(ctx, want.ProjectID, store.SecretBindingFilter{
		SecretID: locked.SecretID, TargetKind: model.SecretBindingTargetSandbox, TargetID: want.TargetID})
	if err != nil {
		return nil, nil, err
	}
	byID := map[string]*model.SecretBinding{}
	var mine []bindingplan.Binding
	for i := range standing {
		if standing[i].ID == locked.ID {
			standing[i] = *locked
		}
		byID[standing[i].ID] = &standing[i]
		mine = append(mine, planBinding(&standing[i]))
	}
	plan := bindingplan.Rebind(mine, locked.ID, want.EnvName)
	// Every binding the rebind would touch is checked before any is
	// written: an approval takes over none handed on under a delegation, an
	// addition a lead granted included, nor one handing over the value.
	if path == rebindApproval {
		for _, step := range plan.Steps {
			if why := bindingplan.ApprovalRefusal(step.Binding); why != "" {
				return nil, nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
					"binding %s in %s is %s, and an approval never takes such a binding over; rebind %s directly (admin binding create --replaces %s)",
					step.Binding.ID, want.EnvName, why, want.EnvName, locked.ID))
			}
		}
	}
	before := *locked
	plain := store.BindingChange{Actor: change.Actor, Cause: change.Cause}
	var touched []model.SecretBinding
	for _, step := range plan.Steps {
		binding := byID[step.Binding.ID]
		switch {
		case step.Fate == bindingplan.LosesUse:
			was := *binding
			binding.Use, binding.EnvName, binding.Raw = false, "", ""
			if err := txStore.UpdateSecretBinding(ctx, binding, plain); err != nil {
				return nil, nil, bindingConflict(err)
			}
			touched = append(touched, was, *binding)
		case binding.ID == locked.ID:
			// The replaced binding records no event of its own: the
			// rebound event names it.
			deleted, err := txStore.DeleteReboundBinding(ctx, binding.ProjectID, binding.ID, plain)
			if err != nil {
				return nil, nil, err
			}
			touched = append(touched, deleted...)
		default:
			deleted, err := txStore.DeleteSecretBinding(ctx, binding.ProjectID, binding.ID, plain)
			if err != nil {
				return nil, nil, err
			}
			touched = append(touched, deleted...)
		}
	}
	return &before, touched, nil
}

// secretLabel names a binding's secret for a refusal: its name, else its ID.
func (s *Service) secretLabel(ctx context.Context, txStore *store.Store, binding *model.SecretBinding) string {
	if secret, err := txStore.GetSecret(ctx, binding.ProjectID, binding.SecretID); err == nil && secret.Name != "" {
		return "secret " + secret.Name
	}
	return "secret " + binding.SecretID
}
