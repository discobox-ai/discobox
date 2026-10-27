package store

import (
	"context"
	"slices"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// withBoundSecrets counts each harness config's use bindings into
// HarnessConfig.BoundSecrets. Every read that returns a harness config applies
// it — including the one a sandbox preloads — because the field is not a
// column, and a read that skipped it would report a harness with credentials
// as having none.
func withBoundSecrets(db *gorm.DB) *gorm.DB {
	return db.Select("harness_configs.*, " +
		"(SELECT COUNT(*) FROM secret_bindings b WHERE b.target_kind = 'harnessConfig' AND b.target_id = harness_configs.id AND b.use) AS bound_secrets")
}

// ListHarnessConfigBundles is the bundle every harness config of every
// project is pinned to, as each config's ID, Bundle and BundleDigest alone:
// what the asset store keeps blobs for (ADR 26-10-16-423 §13).
func (s *Store) ListHarnessConfigBundles(ctx context.Context) ([]model.HarnessConfig, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var configs []model.HarnessConfig
	err = read.
		Select("id", "bundle", "bundle_digest").
		Where("bundle <> '' AND bundle_digest <> ''").
		Find(&configs).Error
	return configs, err
}

// ListSandboxBundles is the bundles the sandboxes of a project, or of every
// project when projectID is empty, are pinned to, as each sandbox's ID and its
// bundle columns alone, either form (model.SandboxManifest.BundlePins) and its
// agent's: what a pool of the project is answered and what the asset store
// keeps blobs for (ADR 26-10-16-423 §§7, 13, ADR 26-10-24-953 §2).
func (s *Store) ListSandboxBundles(ctx context.Context, projectID string) ([]model.Sandbox, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.Model(&model.Sandbox{}).
		Select("id", "bundle", "bundle_digest", "platform_bundle", "platform_bundle_digest", "bundles", "agent_bundle", "agent_bundle_digest").
		Where("bundle_digest <> '' OR platform_bundle_digest <> '' OR agent_bundle_digest <> '' OR NOT " + noBundleList)
	if projectID != "" {
		query = query.Where("project_id = ?", projectID)
	}
	var sandboxes []model.Sandbox
	err = query.Find(&sandboxes).Error
	return sandboxes, err
}

func (s *Store) ListHarnessConfigs(ctx context.Context, projectID string) ([]model.HarnessConfig, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var configs []model.HarnessConfig
	err = read.
		Scopes(withBoundSecrets).
		Where("project_id = ?", projectID).
		Order("created_at ASC").
		Find(&configs).Error
	return configs, err
}

func (s *Store) CreateHarnessConfig(ctx context.Context, config *model.HarnessConfig) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(config).Error
}

func (s *Store) GetHarnessConfig(ctx context.Context, projectID, configID string) (*model.HarnessConfig, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.HarnessConfig](read.Scopes(withBoundSecrets).Where("project_id = ?", projectID), "id", configID)
}

// GetHarnessConfigByID looks a harness config up by ID alone, for reconcilers
// that are handed a resource ID without its project scope.
func (s *Store) GetHarnessConfigByID(ctx context.Context, configID string) (*model.HarnessConfig, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.HarnessConfig](read.Scopes(withBoundSecrets), "id", configID)
}

func (s *Store) GetHarnessConfigByName(ctx context.Context, projectID, name string) (*model.HarnessConfig, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var config model.HarnessConfig
	if err := read.Scopes(withBoundSecrets).Where("project_id = ? AND name = ?", projectID, name).First(&config).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &config, nil
}

func (s *Store) GetHarnessConfigBySlug(ctx context.Context, projectID, slug string) (*model.HarnessConfig, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var config model.HarnessConfig
	if err := read.Scopes(withBoundSecrets).Where("project_id = ? AND slug = ?", projectID, slug).First(&config).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &config, nil
}

// UpdateHarnessConfig saves the config's row, all but the egress level a
// person set, which only SetHarnessConfigEgress writes (see egressColumn).
func (s *Store) UpdateHarnessConfig(ctx context.Context, config *model.HarnessConfig) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	previous := config.Revision
	config.Revision++
	result := write.Model(&model.HarnessConfig{}).Where("id = ? AND project_id = ? AND revision = ?", config.ID, config.ProjectID, previous).Select("*").Omit(egressColumn, "Project", "Sandboxes").Updates(config)
	if result.Error != nil {
		config.Revision = previous
		return result.Error
	}
	if result.RowsAffected != 1 {
		config.Revision = previous
		return ErrGenerationConflict
	}
	return nil
}

// DeleteHarnessConfig deletes a harness config no live sandbox uses, with the
// bindings on it. onBindingsDeleted, when set, is handed in the delete's
// transaction the
// bindings it took with it: the config's own, and those other discoboxes were
// handed on under its delegate bindings, whose holders it marks.
func (s *Store) DeleteHarnessConfig(ctx context.Context, projectID, configID string, onBindingsDeleted BindingsDeletedFunc) error {
	return s.Transaction(ctx, func(txStore *Store, tx *gorm.DB) error {
		// Locked before its bindings are deleted, as DeleteSandbox does.
		config, err := firstByID[model.HarnessConfig](tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ?", projectID), "id", configID)
		if err != nil {
			return err
		}
		if err := tx.Model(&model.Project{}).
			Where("id = ? AND default_harness_config_id = ?", projectID, configID).
			Update("default_harness_config_id", "").Error; err != nil {
			return err
		}
		// A live sandbox still using the config blocks deletion with a clear error.
		var liveRefs int64
		if err := tx.Model(&model.Sandbox{}).Where("harness_config_id = ? OR pending_harness_config_id = ?", configID, configID).Count(&liveRefs).Error; err != nil {
			return err
		}
		if liveRefs > 0 {
			return ErrInUse
		}
		deleted, err := DeleteSecretBindingsForTargetTx(tx, projectID, model.SecretBindingTargetHarnessConfig, config.ID)
		if err != nil {
			return err
		}
		if onBindingsDeleted != nil {
			if err := onBindingsDeleted(txStore, tx, deleted); err != nil {
				return err
			}
		}
		// An attachment ends with its harness config (ADR 26-10-27-172 §1):
		// the sandboxes it was attached to, which the callback marked through
		// the deleted bindings, no longer name it.
		if err := detachHarnessConfigTx(tx, projectID, config.ID); err != nil {
			return err
		}
		return tx.Delete(config).Error
	})
}

// detachHarnessConfigTx removes a harness config from every sandbox it was
// attached to.
func detachHarnessConfigTx(tx *gorm.DB, projectID, configID string) error {
	var sandboxes []model.Sandbox
	if err := tx.Select("id", "project_id", sandboxAttachedHarnessColumn).
		Where("project_id = ? AND "+sandboxAttachedHarnessColumn+" LIKE ?", projectID, `%"`+configID+`"%`).
		Find(&sandboxes).Error; err != nil {
		return err
	}
	for i := range sandboxes {
		kept := slices.DeleteFunc(slices.Clone(sandboxes[i].AttachedHarnessConfigIDs), func(id string) bool { return id == configID })
		if err := tx.Model(&sandboxes[i]).Select(sandboxAttachedHarnessColumn).
			Updates(&model.Sandbox{AttachedHarnessConfigIDs: kept}).Error; err != nil {
			return err
		}
	}
	return nil
}
