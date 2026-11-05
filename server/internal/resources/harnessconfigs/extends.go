package harnessconfigs

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/harnessdefs"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

func variantParent(ctx context.Context, tx *store.Store, projectID, childID, parentID string) (*model.HarnessConfig, error) {
	parent, err := tx.GetHarnessConfig(ctx, projectID, strings.TrimSpace(parentID))
	if err != nil {
		return nil, apperrors.NotFound(err, "parent harness config not found in this project")
	}
	if parent.ID == childID || parent.ExtendsID != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "a harness variant must extend a different standalone config")
	}
	if parent.Bundle == "" || parent.BundleDigest == "" || parent.ManifestDigest == "" {
		return nil, apperrors.NewStatusError(http.StatusConflict, "parent harness has no resolved bundle; register or refresh its bundle first")
	}
	if parent.FileOverrides == nil {
		return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("parent harness %s holds unsplit personal files; refresh that parent before extending it", parent.Slug))
	}
	configs, err := tx.ListHarnessConfigs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, cfg := range configs {
		if cfg.ExtendsID != nil && *cfg.ExtendsID == childID {
			return nil, apperrors.NewStatusError(http.StatusConflict, "a harness with dependent variants cannot itself become a variant")
		}
	}
	return parent, nil
}

func (s *Service) createVariant(ctx context.Context, projectID string, input services.CreateHarnessConfigBody) (*model.HarnessConfig, error) {
	name, slug := strings.TrimSpace(input.Name.Or("")), strings.TrimSpace(input.Slug.Or(""))
	if slug == "" {
		slug = harnessdefs.Slugify(name)
	}
	if name == "" {
		name = slug
	}
	if err := harnessdefs.ValidateSlug(slug); err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	overrides, err := fileOverridesFromAPI(input.FileOverrides.Or(nil))
	if err != nil {
		return nil, err
	}
	cfg := &model.HarnessConfig{ProjectID: projectID, Name: name, Slug: slug, FileOverrides: overrides}
	err = s.store.HarnessConfigsTransaction(ctx, projectID, func(tx *store.Store, _ *gorm.DB) error {
		parent, err := variantParent(ctx, tx, projectID, "", input.Extends.Value)
		if err != nil {
			return err
		}
		configs, err := tx.ListHarnessConfigs(ctx, projectID)
		if err != nil {
			return err
		}
		for _, existing := range configs {
			if existing.Slug == slug || existing.Name == name {
				return apperrors.NewStatusError(http.StatusConflict, "harness config name or slug already in use")
			}
		}
		cfg.ExtendsID = &parent.ID
		cfg.InheritBundle(parent)
		return tx.CreateHarnessConfig(ctx, cfg)
	})
	if err != nil {
		return nil, err
	}
	return s.store.GetHarnessConfig(ctx, projectID, cfg.ID)
}

func (s *Service) extendConfig(ctx context.Context, cfg *model.HarnessConfig, parentID string) (*model.HarnessConfig, error) {
	if cfg.BuiltIn {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "a built-in harness tracks the bundle its server ships")
	}
	// A legacy config's personal paths must be identified from its old bundle,
	// never from the new parent's unrelated file set.
	if cfg.FileOverrides == nil {
		metadata, err := s.reinspect(ctx, cfg, cfg.Bundle)
		if err != nil {
			return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("cannot preserve legacy harness files; restore access to its old bundle and refresh it first: %v", err))
		}
		splitFileOverrides(cfg, bundleFiles(metadata))
	}
	previous := configPins(cfg)
	err := s.store.HarnessConfigsTransaction(ctx, cfg.ProjectID, func(tx *store.Store, db *gorm.DB) error {
		parent, err := variantParent(ctx, tx, cfg.ProjectID, cfg.ID, parentID)
		if err != nil {
			return err
		}
		beforeHosts := cfg.ImageEgressHosts
		cfg.ExtendsID = &parent.ID
		cfg.InheritBundle(parent)
		return s.writeSnapshot(ctx, tx, db, cfg, beforeHosts)
	})
	if err != nil {
		return nil, err
	}
	s.applyResolvedPins(ctx, cfg.ProjectID, cfg.ID, previous, configPins(cfg))
	return s.store.GetHarnessConfig(ctx, cfg.ProjectID, cfg.ID)
}

func (s *Service) writeSnapshot(ctx context.Context, tx *store.Store, db *gorm.DB, cfg *model.HarnessConfig, previousHosts []string) error {
	if err := tx.UpdateHarnessConfig(ctx, cfg); err != nil {
		return err
	}
	if err := s.syncConfiguredSecretRoutes(ctx, tx, db, cfg); err != nil {
		return err
	}
	return s.markEgressChangedTx(ctx, tx, db, cfg, !slices.Equal(previousHosts, cfg.ImageEgressHosts))
}

// publishSnapshot commits the root and each changed materialized child together,
// under the same lock used by conversion, creation and deletion. Readers consume
// these snapshots without accessing the registry or resolving parents.
func (s *Service) publishSnapshot(ctx context.Context, cfg *model.HarnessConfig) error {
	type movedConfig struct {
		cfg    model.HarnessConfig
		before pins
	}
	var moved []movedConfig
	err := s.store.HarnessConfigsTransaction(ctx, cfg.ProjectID, func(tx *store.Store, db *gorm.DB) error {
		current, err := tx.GetHarnessConfig(ctx, cfg.ProjectID, cfg.ID)
		if err != nil {
			return err
		}
		if current.Revision != cfg.Revision {
			return store.ErrGenerationConflict
		}
		// A refresh that started before a conversion cannot overwrite its source.
		if !reflect.DeepEqual(*current, *cfg) {
			if err := s.writeSnapshot(ctx, tx, db, cfg, current.ImageEgressHosts); err != nil {
				return err
			}
			moved = append(moved, movedConfig{*cfg, configPins(current)})
		}
		configs, err := tx.ListHarnessConfigs(ctx, cfg.ProjectID)
		if err != nil {
			return err
		}
		for i := range configs {
			child := &configs[i]
			if child.ExtendsID == nil || *child.ExtendsID != cfg.ID {
				continue
			}
			before := *child
			child.InheritBundle(cfg)
			if reflect.DeepEqual(before, *child) {
				continue
			}
			if err := s.writeSnapshot(ctx, tx, db, child, before.ImageEgressHosts); err != nil {
				return err
			}
			moved = append(moved, movedConfig{*child, configPins(&before)})
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, change := range moved {
		s.applyResolvedPins(ctx, change.cfg.ProjectID, change.cfg.ID, change.before, configPins(&change.cfg))
	}
	return nil
}
