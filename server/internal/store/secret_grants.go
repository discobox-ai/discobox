package store

import (
	"context"
	"sort"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/hostscope"
	"github.com/discobox-ai/discobox/server/internal/model"
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

// UpdateSecretGrant saves changes to an existing grant in place, keeping its ID
// so anything that references the grant stays valid.
func (s *Store) UpdateSecretGrant(ctx context.Context, grant *model.SecretGrant) error {
	_, err := withResourceEvent(ctx, s, model.EventActionUpdated, func(tx *gorm.DB) (*model.SecretGrant, error) {
		if err := tx.Save(grant).Error; err != nil {
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
// specificity narrows from sandbox to harnessConfig to project; an exact host
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
	// The host is matched in Go rather than in SQL: a grant covers its own
	// host and everything beneath it (hostscope.Covers), which is a relation
	// SQL equality cannot express and which must read identically here, in the
	// pool agent's activation check, and in the guard on what a grant may point
	// a secret at.
	var candidates []model.SecretGrant
	err = read.
		Where("project_id = ? AND secret_id = ? AND (expires_at IS NULL OR expires_at > ?)",
			projectID, secretID, now).
		Find(&candidates).Error
	if err != nil {
		return nil, err
	}

	scopeRank := map[string]int{
		model.SecretGrantScopeSandbox:       0,
		model.SecretGrantScopeHarnessConfig: 1,
		model.SecretGrantScopeProject:       2,
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
		if !hostscope.Covers(c.Host, host) {
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

// ListLiveAgentGrants returns the live grants that carry uses and cover one of
// the given scopes, narrowest first.
//
// It is not FindLiveGrant: that call answers "may this credential go to this
// host", matching a destination the proxy observed. This one answers "what may
// this discobox's agent ask for", where the host is part of the reply rather
// than the question, and only a grant with declared uses is an answer — a
// standing grant that happens to cover the same secret is not an approval of
// any use (ADR 0031 §5).
func (s *Store) ListLiveAgentGrants(ctx context.Context, projectID string, scopes []GrantScope) ([]model.SecretGrant, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	allowed := make(map[GrantScope]int, len(scopes))
	for rank, sc := range scopes {
		if sc.ScopeKey != "" {
			allowed[sc] = rank
		}
	}
	var candidates []model.SecretGrant
	err = read.
		Where("project_id = ? AND (expires_at IS NULL OR expires_at > ?)", projectID, time.Now().UTC()).
		Order("granted_at DESC").
		Find(&candidates).Error
	if err != nil {
		return nil, err
	}
	var out []model.SecretGrant
	for i := range candidates {
		if len(candidates[i].Uses) == 0 {
			continue
		}
		if _, ok := allowed[GrantScope{Scope: candidates[i].Scope, ScopeKey: candidates[i].ScopeKey}]; !ok {
			continue
		}
		out = append(out, candidates[i])
	}
	// Narrowest first, so the grant somebody wrote about this one discobox
	// claims its environment variable ahead of one written about the project.
	sort.SliceStable(out, func(i, j int) bool {
		return allowed[GrantScope{Scope: out[i].Scope, ScopeKey: out[i].ScopeKey}] <
			allowed[GrantScope{Scope: out[j].Scope, ScopeKey: out[j].ScopeKey}]
	})
	return out, nil
}

// isMoreSpecificGrant reports whether candidate beats current: a narrower scope
// wins, then an exact host beats a wildcard host.
func isMoreSpecificGrant(candidate, current *model.SecretGrant, scopeRank map[string]int, host string) bool {
	if scopeRank[candidate.Scope] != scopeRank[current.Scope] {
		return scopeRank[candidate.Scope] < scopeRank[current.Scope]
	}
	// Between two grants that both cover the destination, the narrower one
	// wins: the host itself, then a parent of it, then the wildcard.
	return hostscope.Specificity(candidate.Host, host) < hostscope.Specificity(current.Host, host)
}
