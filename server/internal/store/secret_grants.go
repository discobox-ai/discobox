package store

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/server/internal/model"
)

// GrantScope pairs a scope with the key it resolves against, used to look up a
// live grant covering a resolving principal.
type GrantScope struct {
	Scope    string
	ScopeKey string
}

func (s *Store) CreateSecretGrant(ctx context.Context, grant *model.SecretGrant) error {
	_, err := withResourceEvent(ctx, s, model.EventActionCreated, func(tx *gorm.DB) (*model.SecretGrant, error) {
		if err := tx.Create(grant).Error; err != nil {
			return nil, err
		}
		return grant, nil
	})
	return err
}

func (s *Store) GetSecretGrant(ctx context.Context, projectID, grantID string) (*model.SecretGrant, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.SecretGrant](read.Where("project_id = ?", projectID), "id", grantID)
}

// ListSecretGrants returns a project's grants, optionally filtered to a single
// secret.
func (s *Store) ListSecretGrants(ctx context.Context, projectID, secretID string) ([]model.SecretGrant, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.Where("project_id = ?", projectID)
	if secretID != "" {
		query = query.Where("secret_id = ?", secretID)
	}
	var out []model.SecretGrant
	err = query.Order("granted_at ASC").Find(&out).Error
	return out, err
}

// deleteSecretGrantsBySecret removes every grant for a secret. Called when the
// secret is deleted so no standing authorization dangles.
func (s *Store) deleteSecretGrantsBySecret(tx *gorm.DB, secretID string) error {
	return tx.Where("secret_id = ?", secretID).Delete(&model.SecretGrant{}).Error
}

// DeleteSecretGrant revokes a grant.
func (s *Store) DeleteSecretGrant(ctx context.Context, projectID, grantID string) error {
	_, err := withResourceEvent(ctx, s, model.EventActionDeleted, func(tx *gorm.DB) (*model.SecretGrant, error) {
		grant, err := firstByID[model.SecretGrant](tx.Where("project_id = ?", projectID), "id", grantID)
		if err != nil {
			return nil, err
		}
		if err := tx.Delete(grant).Error; err != nil {
			return nil, err
		}
		return grant, nil
	})
	return err
}

// FindLiveGrant returns the most specific unexpired grant for a secret whose
// scope key matches one of the candidate scopes and whose host constraint
// permits the requested host. Returns ErrNotFound when none applies. Scope
// specificity narrows from sandbox to agentConfig to project; an exact host
// beats a wildcard grant.
func (s *Store) FindLiveGrant(ctx context.Context, projectID, secretID, host string, scopes []GrantScope) (*model.SecretGrant, error) {
	if len(scopes) == 0 {
		return nil, ErrNotFound
	}
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var candidates []model.SecretGrant
	err = read.
		Where("project_id = ? AND secret_id = ? AND (host = '' OR host = ?) AND (expires_at IS NULL OR expires_at > ?)",
			projectID, secretID, host, now).
		Find(&candidates).Error
	if err != nil {
		return nil, err
	}

	scopeRank := map[string]int{
		model.SecretGrantScopeSandbox:     0,
		model.SecretGrantScopeAgentConfig: 1,
		model.SecretGrantScopeProject:     2,
	}
	allowed := make(map[GrantScope]struct{}, len(scopes))
	for _, sc := range scopes {
		if sc.ScopeKey == "" {
			continue
		}
		allowed[sc] = struct{}{}
	}

	var best *model.SecretGrant
	for i := range candidates {
		c := &candidates[i]
		if _, ok := allowed[GrantScope{Scope: c.Scope, ScopeKey: c.ScopeKey}]; !ok {
			continue
		}
		if best == nil || isMoreSpecificGrant(c, best, scopeRank, host) {
			best = c
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

// isMoreSpecificGrant reports whether candidate beats current: a narrower scope
// wins, then an exact host beats a wildcard host.
func isMoreSpecificGrant(candidate, current *model.SecretGrant, scopeRank map[string]int, host string) bool {
	if scopeRank[candidate.Scope] != scopeRank[current.Scope] {
		return scopeRank[candidate.Scope] < scopeRank[current.Scope]
	}
	candidateExactHost := candidate.Host == host && host != ""
	currentExactHost := current.Host == host && host != ""
	return candidateExactHost && !currentExactHost
}
