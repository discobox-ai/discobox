package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
)

// Bindings are the authorization (ADR 26-10-27-880 §1): a binding that exists
// and has not lapsed is the whole of the permission. Every change to one is
// written with an event in the same transaction (§6), so its history outlives
// the row.

// BindingExpiryResourceType is the reconcile resource that removes lapsed
// bindings (resources/secrets' BindingExpiryReconciler): one row,
// BindingExpiryID. Every binding written with an expiry marks it at that
// expiry, in the transaction that writes it (recordBindingEventTx), so no
// writer can leave a lapse unswept.
const (
	BindingExpiryResourceType = "secretBindingExpiry"
	BindingExpiryID           = "all"
)

// BindingsDeletedFunc is told, in the transaction of a target's delete, the
// bindings the delete took with it, so it can mark the discoboxes they
// covered.
type BindingsDeletedFunc func(txStore *Store, tx *gorm.DB, deleted []model.SecretBinding) error

// BindingChange is who made a change to a binding and why: the actor is a
// person's or a discobox's principal, or model.SecretBindingActorSystem for a
// cascade, an expiry or the upgrade; the cause is the request it answered or
// the parent whose removal took it.
type BindingChange struct {
	Actor string
	Cause string
}

// CreateSecretBinding stores a binding and its created event. A binding for a
// sandbox or harness config the project does not hold is ErrNotFound: deleting
// either takes its bindings with it (DeleteSecretBindingsForTargetTx), and the
// target is read FOR SHARE against that delete's FOR UPDATE so the two
// serialize, as a grant's did. A second use binding for one variable on one
// target, or a second delegate binding of one secret, is a constraint error.
func (s *Store) CreateSecretBinding(ctx context.Context, binding *model.SecretBinding, change BindingChange) error {
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		if err := lockBindingTarget(tx, binding.ProjectID, binding.TargetKind, binding.TargetID); err != nil {
			return err
		}
		if err := tx.Create(binding).Error; err != nil {
			return err
		}
		return recordBindingEventTx(tx, binding, model.SecretBindingEventCreated, change)
	})
}

// lockBindingTarget reads a binding's target FOR SHARE, refusing one the
// project does not hold.
func lockBindingTarget(tx *gorm.DB, projectID, kind, targetID string) error {
	var target any
	switch kind {
	case model.SecretBindingTargetSandbox:
		target = &model.Sandbox{}
	case model.SecretBindingTargetHarnessConfig:
		target = &model.HarnessConfig{}
	case model.SecretBindingTargetProject:
		if targetID != projectID {
			return ErrNotFound
		}
		return nil
	default:
		return ErrNotFound
	}
	var ids []string
	if err := tx.Model(target).Clauses(clause.Locking{Strength: "SHARE"}).
		Where("project_id = ? AND id = ?", projectID, targetID).
		Pluck("id", &ids).Error; err != nil {
		return err
	}
	if len(ids) == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateSecretBinding saves a binding's routes, expiry, variable and what it
// is for, and its updated event. A second use binding of the variable, or a
// second delegate binding of the secret, on its target is a constraint error.
func (s *Store) UpdateSecretBinding(ctx context.Context, binding *model.SecretBinding, change BindingChange) error {
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		binding.UpdatedAt = time.Now().UTC()
		result := tx.Model(&model.SecretBinding{}).
			Where("project_id = ? AND id = ?", binding.ProjectID, binding.ID).
			Select("routes", "expires_at", "env_name", "use", "delegate", "updated_at").
			Updates(binding)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return recordBindingEventTx(tx, binding, model.SecretBindingEventUpdated, change)
	})
}

// GetSecretBinding reads one binding of a project by ID or unique ID prefix.
func (s *Store) GetSecretBinding(ctx context.Context, projectID, bindingID string) (*model.SecretBinding, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.SecretBinding](read.Where("project_id = ?", projectID), "id", bindingID)
}

// SecretBindingByExactID reads a binding a row names, by its whole ID: what
// another row recorded is never matched as a prefix.
func (s *Store) SecretBindingByExactID(ctx context.Context, projectID, bindingID string) (*model.SecretBinding, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var rows []model.SecretBinding
	if err := read.Where("project_id = ? AND id = ?", projectID, bindingID).Limit(1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return &rows[0], nil
}

// SecretBindingFilter narrows ListSecretBindings; an empty field filters
// nothing.
type SecretBindingFilter struct {
	SecretID   string
	TargetKind string
	TargetID   string
	ParentID   string
}

// ListSecretBindings lists a project's bindings, oldest first, live or not.
func (s *Store) ListSecretBindings(ctx context.Context, projectID string, filter SecretBindingFilter) ([]model.SecretBinding, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.Where("project_id = ?", projectID)
	if filter.SecretID != "" {
		query = query.Where("secret_id = ?", filter.SecretID)
	}
	if filter.TargetKind != "" {
		query = query.Where("target_kind = ?", filter.TargetKind)
	}
	if filter.TargetID != "" {
		query = query.Where("target_id = ?", filter.TargetID)
	}
	if filter.ParentID != "" {
		query = query.Where("parent_id = ?", filter.ParentID)
	}
	var out []model.SecretBinding
	err = query.Order("created_at ASC, id ASC").Find(&out).Error
	return out, err
}

// BindingTarget is one level a sandbox is covered at.
type BindingTarget struct {
	Kind string
	ID   string
	// UseOnly is a level whose use bindings apply and whose delegate
	// bindings do not: a harness config attached to the sandbox (ADR
	// 26-10-27-172 §1).
	UseOnly bool
}

// Covers reports whether binding is on one of targets — for delegation, on
// one whose delegate bindings apply.
func Covers(targets []BindingTarget, binding *model.SecretBinding, forDelegation bool) bool {
	for _, target := range targets {
		if target.Kind == binding.TargetKind && target.ID == binding.TargetID && (!forDelegation || !target.UseOnly) {
			return true
		}
	}
	return false
}

// SandboxBindingTargets are the levels that cover a sandbox, narrowest first
// (ADR 26-10-27-880 §2, ADR 26-10-27-172 §1): the sandbox, its harness
// config, the harness configs attached to it to run beside its own (in the
// order attached, for use only), its project.
func SandboxBindingTargets(sandbox *model.Sandbox) []BindingTarget {
	targets := []BindingTarget{{Kind: model.SecretBindingTargetSandbox, ID: sandbox.ID}}
	own := ""
	if sandbox.HarnessConfigID != nil {
		own = strings.TrimSpace(*sandbox.HarnessConfigID)
	}
	if own != "" {
		targets = append(targets, BindingTarget{Kind: model.SecretBindingTargetHarnessConfig, ID: own})
	}
	for _, attached := range sandbox.AttachedHarnessConfigIDs {
		if attached = strings.TrimSpace(attached); attached != "" && attached != own {
			targets = append(targets, BindingTarget{Kind: model.SecretBindingTargetHarnessConfig, ID: attached, UseOnly: true})
		}
	}
	return append(targets, BindingTarget{Kind: model.SecretBindingTargetProject, ID: sandbox.ProjectID})
}

// ListBindingsOnTargets lists a project's bindings on any of targets, live or
// not, oldest first. Whoever reads them decides which applies.
func (s *Store) ListBindingsOnTargets(ctx context.Context, projectID string, targets []BindingTarget) ([]model.SecretBinding, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, nil
	}
	query := read.Where("project_id = ?", projectID)
	var clauses []string
	var args []any
	for _, target := range targets {
		clauses = append(clauses, "(target_kind = ? AND target_id = ?)")
		args = append(args, target.Kind, target.ID)
	}
	var out []model.SecretBinding
	err = query.Where(strings.Join(clauses, " OR "), args...).Order("created_at ASC, id ASC").Find(&out).Error
	return out, err
}

// ListBindingsByIDs reads bindings of a project by ID, the parents a set of
// bindings names. A missing ID is left out.
func (s *Store) ListBindingsByIDs(ctx context.Context, projectID string, ids []string) (map[string]model.SecretBinding, error) {
	out := map[string]model.SecretBinding{}
	if len(ids) == 0 {
		return out, nil
	}
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var rows []model.SecretBinding
	if err := read.Where("project_id = ? AND id IN ?", projectID, ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.ID] = row
	}
	return out, nil
}

// DeleteSecretBinding deletes a binding and every binding handed on under
// it, writing an event for each, and returns them, the one asked for first.
func (s *Store) DeleteSecretBinding(ctx context.Context, projectID, bindingID string, change BindingChange) ([]model.SecretBinding, error) {
	var deleted []model.SecretBinding
	err := s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		binding, err := firstByID[model.SecretBinding](tx.Where("project_id = ?", projectID), "id", bindingID)
		if err != nil {
			return err
		}
		deleted, err = deleteBindingTreeTx(tx, binding, model.SecretBindingEventDeleted, change)
		return err
	})
	return deleted, err
}

// deleteBindingTreeTx deletes binding and its children, an event each, and
// returns them. The children go as the system's, caused by their parent.
func deleteBindingTreeTx(tx *gorm.DB, binding *model.SecretBinding, event string, change BindingChange) ([]model.SecretBinding, error) {
	var children []model.SecretBinding
	if err := tx.Where("project_id = ? AND parent_id = ?", binding.ProjectID, binding.ID).Find(&children).Error; err != nil {
		return nil, err
	}
	if err := tx.Delete(&model.SecretBinding{}, "id = ?", binding.ID).Error; err != nil {
		return nil, err
	}
	if err := recordBindingEventTx(tx, binding, event, change); err != nil {
		return nil, err
	}
	deleted := []model.SecretBinding{*binding}
	for i := range children {
		gone, err := deleteBindingTreeTx(tx, &children[i], model.SecretBindingEventDeleted,
			BindingChange{Actor: model.SecretBindingActorSystem, Cause: binding.ID})
		if err != nil {
			return nil, err
		}
		deleted = append(deleted, gone...)
	}
	return deleted, nil
}

// DeleteChildBindings deletes every binding handed on under parentID, with
// theirs, as the system's caused by the parent: what turning its delegation
// off takes with it. It returns what it deleted.
func (s *Store) DeleteChildBindings(ctx context.Context, projectID, parentID string) ([]model.SecretBinding, error) {
	var deleted []model.SecretBinding
	err := s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		var children []model.SecretBinding
		if err := tx.Where("project_id = ? AND parent_id = ?", projectID, parentID).Find(&children).Error; err != nil {
			return err
		}
		var err error
		deleted, err = deleteBindingsTx(tx, children, BindingChange{Actor: model.SecretBindingActorSystem, Cause: parentID})
		return err
	})
	return deleted, err
}

// DeleteSecretBindingsForTargetTx deletes every binding on a target, with
// their children, as the system's: what the target's own delete takes with it.
// It returns every binding it deleted, so the caller can mark the discoboxes
// they covered.
func DeleteSecretBindingsForTargetTx(tx *gorm.DB, projectID, kind, targetID string) ([]model.SecretBinding, error) {
	var bindings []model.SecretBinding
	if err := tx.Where("project_id = ? AND target_kind = ? AND target_id = ?", projectID, kind, targetID).Find(&bindings).Error; err != nil {
		return nil, err
	}
	return deleteBindingsTx(tx, bindings, BindingChange{Actor: model.SecretBindingActorSystem, Cause: kind + ":" + targetID})
}

// deleteBindingsTx deletes bindings with their children, once each: a child
// that is also in bindings has gone with its parent by the time it is reached.
func deleteBindingsTx(tx *gorm.DB, bindings []model.SecretBinding, change BindingChange) ([]model.SecretBinding, error) {
	gone := map[string]bool{}
	var deleted []model.SecretBinding
	for i := range bindings {
		if gone[bindings[i].ID] {
			continue
		}
		removed, err := deleteBindingTreeTx(tx, &bindings[i], model.SecretBindingEventDeleted, change)
		if err != nil {
			return nil, err
		}
		for _, binding := range removed {
			gone[binding.ID] = true
		}
		deleted = append(deleted, removed...)
	}
	return deleted, nil
}

// deleteSecretBindingsBySecretTx deletes every binding of a secret, as the
// system's: what the secret's delete takes with it.
func deleteSecretBindingsBySecretTx(tx *gorm.DB, projectID, secretID string) ([]model.SecretBinding, error) {
	var bindings []model.SecretBinding
	if err := tx.Where("project_id = ? AND secret_id = ?", projectID, secretID).Find(&bindings).Error; err != nil {
		return nil, err
	}
	return deleteBindingsTx(tx, bindings, BindingChange{Actor: model.SecretBindingActorSystem, Cause: "secret:" + secretID})
}

// ListLapsedSecretBindings lists every binding whose expiry has passed, across
// projects, for the expiry sweep.
func (s *Store) ListLapsedSecretBindings(ctx context.Context, now time.Time) ([]model.SecretBinding, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.SecretBinding
	err = read.Where("expires_at IS NOT NULL AND expires_at <= ?", now.UTC()).Order("expires_at ASC").Find(&out).Error
	return out, err
}

// NextSecretBindingExpiry is when the next binding lapses, if any does.
func (s *Store) NextSecretBindingExpiry(ctx context.Context) (*time.Time, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var rows []model.SecretBinding
	if err := read.Where("expires_at IS NOT NULL").Order("expires_at ASC").Limit(1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0].ExpiresAt, nil
}

// ExpireSecretBindingTx deletes a lapsed binding and its children with expired
// and deleted events. A binding renewed since it was read is left alone.
func ExpireSecretBindingTx(tx *gorm.DB, binding *model.SecretBinding, now time.Time) ([]model.SecretBinding, error) {
	current, err := firstByID[model.SecretBinding](tx.Where("project_id = ?", binding.ProjectID), "id", binding.ID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if current.Live(now) {
		return nil, nil
	}
	return deleteBindingTreeTx(tx, current, model.SecretBindingEventExpired, BindingChange{Actor: model.SecretBindingActorSystem})
}

// recordBindingEventTx appends one event for a change to binding and, when
// the change leaves the binding with an expiry, arms the expiry sweep for
// that instant.
func recordBindingEventTx(tx *gorm.DB, binding *model.SecretBinding, event string, change BindingChange) error {
	if (event == model.SecretBindingEventCreated || event == model.SecretBindingEventUpdated) && binding.ExpiresAt != nil {
		ctx := tx.Statement.Context
		if ctx == nil {
			ctx = context.Background()
		}
		if err := reconcile.MarkAtTx(ctx, tx, BindingExpiryResourceType, BindingExpiryID, *binding.ExpiresAt); err != nil {
			return err
		}
	}
	snapshot := *binding
	snapshot.Project = nil
	// The history names the secret as it was called when the change was made.
	if snapshot.SecretName == "" {
		var names []string
		if err := tx.Model(&model.Secret{}).Where("project_id = ? AND id = ?", binding.ProjectID, binding.SecretID).Limit(1).Pluck("name", &names).Error; err != nil {
			return err
		}
		if len(names) == 1 {
			snapshot.SecretName = names[0]
		}
	}
	// Within a binding, a sequence orders its events where the clock does
	// not: a coarse clock (Windows) stamps two changes in one tick alike.
	// Every change to an existing binding writes or deletes its row in this
	// transaction, which serializes them, so two cannot read the same last.
	var last []model.SecretBindingEvent
	if err := tx.Select("seq").Where("project_id = ? AND binding_id = ?", binding.ProjectID, binding.ID).
		Order("seq DESC").Limit(1).Find(&last).Error; err != nil {
		return err
	}
	seq := int64(1)
	if len(last) == 1 {
		seq = last[0].Seq + 1
	}
	return tx.Create(&model.SecretBindingEvent{
		ProjectID: binding.ProjectID,
		BindingID: binding.ID,
		SecretID:  binding.SecretID,
		Event:     event,
		Actor:     change.Actor,
		Cause:     change.Cause,
		Binding:   snapshot,
		Seq:       seq,
		At:        time.Now().UTC(),
	}).Error
}

// SecretBindingEventFilter narrows ListSecretBindingEvents.
type SecretBindingEventFilter struct {
	BindingID string
	SecretID  string
	Since     time.Time
	Limit     int
}

// ListSecretBindingEvents reads a project's binding history, newest first.
func (s *Store) ListSecretBindingEvents(ctx context.Context, projectID string, filter SecretBindingEventFilter) ([]model.SecretBindingEvent, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.Where("project_id = ?", projectID)
	if filter.BindingID != "" {
		query = query.Where("binding_id = ?", filter.BindingID)
	}
	if filter.SecretID != "" {
		query = query.Where("secret_id = ?", filter.SecretID)
	}
	if !filter.Since.IsZero() {
		query = query.Where("at >= ?", filter.Since.UTC())
	}
	limit := filter.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	var out []model.SecretBindingEvent
	err = query.Order("at DESC, seq DESC, id DESC").Limit(limit).Find(&out).Error
	return out, err
}

// ListSandboxIDsCoveredBy lists the live sandboxes a binding target covers:
// the one sandbox, every sandbox on a harness config, or every sandbox in the
// project.
func (s *Store) ListSandboxIDsCoveredBy(ctx context.Context, projectID, kind, targetID string) ([]string, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.Model(&model.Sandbox{}).Where("project_id = ?", projectID).
		Where("state NOT IN ?", []string{model.SandboxStateArchived, model.SandboxStateDeleted})
	switch kind {
	case model.SecretBindingTargetSandbox:
		query = query.Where("id = ?", targetID)
	case model.SecretBindingTargetHarnessConfig:
		// The sandboxes running it, and those it was attached to.
		query = query.Where("harness_config_id = ? OR attached_harness_config_ids LIKE ?", targetID, `%"`+targetID+`"%`)
	case model.SecretBindingTargetProject:
	default:
		return nil, nil
	}
	var out []string
	err = query.Order("id ASC").Pluck("id", &out).Error
	return out, err
}

// ListTargetUseBindings lists the use bindings on one target, by variable.
func (s *Store) ListTargetUseBindings(ctx context.Context, projectID, kind, targetID string) ([]model.SecretBinding, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.SecretBinding
	err = read.Where("project_id = ? AND target_kind = ? AND target_id = ? AND use", projectID, kind, targetID).
		Order("env_name ASC").Find(&out).Error
	return out, err
}

// PutTargetUseBinding makes the use binding of one variable on a target the
// one given, which names the secret, routes and expiry: one already standing
// for the same secret is updated to them, one for another secret is replaced
// (removed, with what was handed on under it, and the new one created), each
// change with its event. It returns the binding standing and every one it
// removed.
func (s *Store) PutTargetUseBinding(ctx context.Context, binding *model.SecretBinding, change BindingChange) ([]model.SecretBinding, error) {
	var removed []model.SecretBinding
	err := s.Transaction(ctx, func(txStore *Store, tx *gorm.DB) error {
		var standing []model.SecretBinding
		if err := tx.Where("project_id = ? AND target_kind = ? AND target_id = ? AND use AND env_name = ?",
			binding.ProjectID, binding.TargetKind, binding.TargetID, binding.EnvName).Find(&standing).Error; err != nil {
			return err
		}
		for i := range standing {
			current := &standing[i]
			if current.SecretID == binding.SecretID {
				binding.ID, binding.CreatedAt = current.ID, current.CreatedAt
				binding.Delegate, binding.ParentID = current.Delegate, current.ParentID
				if model.SameRoutes(current.Routes, binding.Routes) &&
					sameExpiry(current.ExpiresAt, binding.ExpiresAt) {
					return nil
				}
				return txStore.UpdateSecretBinding(ctx, binding, change)
			}
			gone, err := deleteBindingTreeTx(tx, current, model.SecretBindingEventDeleted, change)
			if err != nil {
				return err
			}
			removed = append(removed, gone...)
		}
		return txStore.CreateSecretBinding(ctx, binding, change)
	})
	return removed, err
}

// DeleteTargetUseBinding removes the use binding of one variable on a target,
// with what was handed on under it, and returns what it removed.
func (s *Store) DeleteTargetUseBinding(ctx context.Context, projectID, kind, targetID, envName string, change BindingChange) ([]model.SecretBinding, error) {
	var removed []model.SecretBinding
	err := s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		var standing []model.SecretBinding
		if err := tx.Where("project_id = ? AND target_kind = ? AND target_id = ? AND use AND env_name = ?",
			projectID, kind, targetID, envName).Find(&standing).Error; err != nil {
			return err
		}
		var err error
		removed, err = deleteBindingsTx(tx, standing, change)
		return err
	})
	return removed, err
}

func sameExpiry(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
