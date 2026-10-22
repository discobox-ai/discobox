package store

import (
	"context"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/pkg/hostscope"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// GrantScope pairs a scope with the key it resolves against, used to look up a
// live grant covering a resolving principal.
type GrantScope struct {
	Scope    string
	ScopeKey string
}

// SandboxGrantScopes is every scope a grant may cover a discobox through:
// itself, the harness config it runs, and its project, narrowest first.
func SandboxGrantScopes(sandbox *model.Sandbox) []GrantScope {
	scopes := []GrantScope{{Scope: model.SecretGrantScopeSandbox, ScopeKey: sandbox.ID}}
	if sandbox.HarnessConfigID != nil && strings.TrimSpace(*sandbox.HarnessConfigID) != "" {
		scopes = append(scopes, GrantScope{
			Scope:    model.SecretGrantScopeHarnessConfig,
			ScopeKey: strings.TrimSpace(*sandbox.HarnessConfigID),
		})
	}
	return append(scopes, GrantScope{Scope: model.SecretGrantScopeProject, ScopeKey: sandbox.ProjectID})
}

// CreateSecretGrant stores a grant, refusing with ErrNotFound one scoped to a
// sandbox or harness config the project does not hold. Deleting either takes
// its grants with it (deleteSecretGrantsByScopeTx), so a grant written for one
// already gone — a request approved after its discobox was purged — would be
// the orphan that delete exists to prevent.
//
// The target is read FOR SHARE, and DeleteSandbox and DeleteHarnessConfig lock
// it FOR UPDATE before deleting its grants, so the two serialize: a delete that
// locks first leaves this nothing to find, and one that waits deletes the grant
// this wrote. SQLite's single writer serializes them without the clause.
func (s *Store) CreateSecretGrant(ctx context.Context, grant *model.SecretGrant) error {
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		var target any
		switch grant.Scope {
		case model.SecretGrantScopeSandbox:
			target = &model.Sandbox{}
		case model.SecretGrantScopeHarnessConfig:
			target = &model.HarnessConfig{}
		}
		if target != nil {
			var ids []string
			if err := tx.Model(target).Clauses(clause.Locking{Strength: "SHARE"}).
				Where("project_id = ? AND id = ?", grant.ProjectID, grant.ScopeKey).
				Pluck("id", &ids).Error; err != nil {
				return err
			}
			if len(ids) == 0 {
				return ErrNotFound
			}
		}
		return tx.Create(grant).Error
	})
}

// UpdateSecretGrant saves changes to an existing grant in place, keeping its ID
// so anything that references the grant stays valid.
func (s *Store) UpdateSecretGrant(ctx context.Context, grant *model.SecretGrant) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Save(grant).Error
}

func (s *Store) GetSecretGrant(ctx context.Context, projectID, grantID string) (*model.SecretGrant, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.SecretGrant](read.Where("project_id = ?", projectID), "id", grantID)
}

// ListSecretGrants returns a project's grants, optionally filtered to a single
// secret, or to those one approval rule minted (ADR 26-10-12-330 §4): a rule's
// grants are independent of it once made, and this is how they are found to
// be revoked deliberately.
func (s *Store) ListSecretGrants(ctx context.Context, projectID, secretID, ruleID string) ([]model.SecretGrant, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.Where("project_id = ?", projectID)
	if secretID != "" {
		query = query.Where("secret_id = ?", secretID)
	}
	if ruleID != "" {
		query = query.Where("rule_id = ?", ruleID)
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

// deleteSecretGrantsByScopeTx removes every grant scoped to one sandbox or
// harness config. Called when that sandbox or config is deleted: its ID is
// never reused, so a grant naming it can never cover anything again, and left
// behind it would only be listed — read as a standing authorization nobody has.
func deleteSecretGrantsByScopeTx(tx *gorm.DB, scope, scopeKey string) error {
	return tx.Where("scope = ? AND scope_key = ?", scope, scopeKey).Delete(&model.SecretGrant{}).Error
}

// DeleteSecretGrant revokes a grant. The lookup and the delete share a
// transaction because grantID may be an ID prefix (firstByID).
func (s *Store) DeleteSecretGrant(ctx context.Context, projectID, grantID string) error {
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		grant, err := firstByID[model.SecretGrant](tx.Where("project_id = ?", projectID), "id", grantID)
		if err != nil {
			return err
		}
		return tx.Delete(grant).Error
	})
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
	// The host is matched in Go rather than in SQL: a grant covers each of its
	// hosts and everything beneath them (hostscope.CoversAny), which is a relation
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
		if !hostscope.CoversAny(c.Hosts, host) {
			continue
		}
		// A grant its holder may only delegate authorizes nothing it sends.
		if !c.MayUse() {
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

// ListLiveDelegationGrants returns the live delegation grants a discobox holds
// (purpose delegate, scoped to it), newest first: what bounds the grants it may
// hand on by approving a request (ADR 26-09-30-782 §3). Every other live-grant
// query leaves these out, since a delegation grant authorizes nothing its
// holder sends.
func (s *Store) ListLiveDelegationGrants(ctx context.Context, projectID, sandboxID string) ([]model.SecretGrant, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.SecretGrant
	err = read.
		Where("project_id = ? AND purpose = ? AND scope = ? AND scope_key = ?",
			projectID, model.SecretGrantPurposeDelegate, model.SecretGrantScopeSandbox, sandboxID).
		Where("expires_at IS NULL OR expires_at > ?", time.Now().UTC()).
		Order("granted_at DESC").
		Find(&out).Error
	return out, err
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
		if len(candidates[i].Uses) == 0 || !candidates[i].MayUse() {
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
	return hostscope.SpecificityAny(candidate.Hosts, host) < hostscope.SpecificityAny(current.Hosts, host)
}
