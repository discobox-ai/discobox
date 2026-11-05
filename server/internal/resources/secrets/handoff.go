package secrets

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// lockHandoff serializes with API-access changes, reparenting and deletion.
// Call before reading authority in the write transaction. SQLite's single
// writer and PostgreSQL row locks both prevent a stale read from committing.
func lockHandoff(ctx context.Context, txStore *store.Store, projectID, holderID, targetID string) error {
	rows, err := txStore.LockHandoffSandboxes(ctx, projectID, holderID, targetID)
	if err != nil {
		return err
	}
	var holder, target *model.Sandbox
	for i := range rows {
		if rows[i].ID == holderID {
			holder = &rows[i]
		}
		if rows[i].ID == targetID {
			target = &rows[i]
		}
	}
	if holder == nil || target == nil || !holder.DiscoboxAPI || holder.DesiredState == model.DesiredStateDeleted || holder.ID == target.ID || target.ParentSandboxID == nil || *target.ParentSandboxID != holderID || target.DesiredState == model.DesiredStateArchived || target.DesiredState == model.DesiredStateDeleted || target.State == model.SandboxStateArchived || target.State == model.SandboxStateDeleted {
		return apperrors.NewStatusError(http.StatusForbidden, "a discobox grants credentials only to a live discobox it currently leads, with API access enabled")
	}
	return nil
}

// fitDelegation chooses one whole fitting parent, never a union. Both the
// secret and candidate rows are locked before the final proof.
func fitDelegation(ctx context.Context, txStore *store.Store, holderID string, binding *model.SecretBinding, candidates []model.SecretBinding, ttl int64, named bool) (*model.Secret, int64, error) {
	var ids []string
	for _, candidate := range candidates {
		ids = append(ids, candidate.ID)
	}
	secret, current, err := txStore.LockHandoffDelegations(ctx, binding.ProjectID, binding.SecretID, ids)
	if err != nil {
		return nil, 0, err
	}
	now := time.Now().UTC()
	if named {
		if err := guardBindingTTL(secret, ttl); err != nil {
			return nil, 0, apperrors.NewStatusError(http.StatusForbidden, "the requested lifetime exceeds the secret's limit")
		}
	} else if secret.MaxBindingTTL > 0 && (ttl <= 0 || ttl > secret.MaxBindingTTL) {
		ttl = secret.MaxBindingTTL
	}
	holder, err := txStore.GetSandbox(ctx, binding.ProjectID, holderID)
	if err != nil {
		return nil, 0, err
	}
	slices.SortFunc(current, func(a, b model.SecretBinding) int {
		if c := -lifetimeOrder(&a, &b); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	for i := range current {
		parent := current[i]
		if !parent.Delegate || parent.SecretID != secret.ID || !parent.Live(now) || !store.Covers(store.SandboxBindingTargets(holder), &parent, true) {
			continue
		}
		fitted, err := fitToParent(&parent, ttl, named, now)
		if err != nil {
			continue
		}
		if len(binding.Routes) > 0 {
			if err := secretroute.ProveContained(secret.Routes, binding.Routes, parent.Routes); err != nil {
				continue
			}
		}
		binding.ParentID = parent.ID
		binding.ExpiresAt = expiryAt(now, fitted)
		binding.SecretName = secret.Name
		binding.Receipt = &model.SecretGrantReceipt{BindingID: binding.ID, RequestID: binding.RequestID, SecretID: secret.ID, SecretName: secret.Name, SandboxID: binding.TargetID, EnvVar: binding.EnvName, ParentID: parent.ID, Actor: binding.CreatedBy, At: now, ExpiresAt: binding.ExpiresAt, RouteLists: store.RouteLists(secret, binding, &parent)}
		return secret, fitted, nil
	}
	return nil, 0, apperrors.NewStatusError(http.StatusForbidden, "no live delegation covers this whole grant's routes and lifetime; use a provably narrower list or ask a person")
}

// CreateSandboxSecretBinding addresses the target in the path. It grants
// sentinel use only and returns just the committed grant's receipt.
func (s *Service) CreateSandboxSecretBinding(ctx context.Context, projectID, targetID string, in services.CreateSandboxSecretBindingBody) (*model.SecretGrantReceipt, error) {
	principal, _ := auth.PrincipalFromContext(ctx)
	if principal.Type != auth.PrincipalTypeSandbox {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "use the project binding endpoint for a person grant")
	}
	target, err := s.store.GetSandbox(ctx, projectID, targetID)
	if err != nil {
		return nil, apperrors.NotFound(err, "discobox not found")
	}
	input := services.SandboxBindingInput{Secret: in.Secret, EnvVar: in.EnvVar, Use: in.Use, Delegate: in.Delegate, Routes: in.Routes, TtlSeconds: in.TtlSeconds}
	if in.Delegate.Or(false) || in.Raw.Set {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "this endpoint grants sentinel use only")
	}
	prepared, err := s.PrepareSandboxBindings(ctx, projectID, target, []services.SandboxBindingInput{input})
	if err != nil {
		return nil, err
	}
	err = s.store.Transaction(ctx, func(txStore *store.Store, tx *gorm.DB) error {
		if err := StoreSandboxBindingsTx(ctx, txStore, prepared); err != nil {
			return err
		}
		return s.markBindingHolders(ctx, txStore, tx, *prepared.Bindings[0])
	})
	if err != nil {
		return nil, err
	}
	return prepared.Bindings[0].Receipt, nil
}
