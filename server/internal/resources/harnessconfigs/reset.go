package harnessconfigs

import (
	"context"
	"errors"
	"strings"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// ResetHarnessConfig returns the config to its bundle's defaults
// (ADR 26-11-02-152 §3): it removes the files discoboxes' saved harness
// changes wrote and the secrets those saves created — deleting a secret takes
// its bindings with it, and the sandboxes holding it are marked. The
// bundle-declared baseline, and secrets a person bound to the config by hand,
// are left as they are. Nothing about whether the harness can run changes:
// every registered harness can.
//
// A person's FileOverrides are theirs, not something a box saved, so a reset
// keeps them unless includeOverrides asks for a config with nothing but its
// bundle (ADR 26-11-04-028 §1). Cleared, the set is empty, never nil: nil
// means a config not yet split.
func (s *Service) ResetHarnessConfig(ctx context.Context, projectID, configID string, includeOverrides bool) (*model.HarnessConfig, error) {
	if _, err := s.store.GetHarnessConfig(ctx, projectID, configID); err != nil {
		return nil, apperrors.NotFound(err, "harness config not found")
	}
	if err := s.store.Transaction(ctx, func(tx *store.Store, db *gorm.DB) error {
		config, err := tx.GetHarnessConfig(ctx, projectID, configID)
		if err != nil {
			return err
		}
		// Claim the row first, so a save racing this one fails its revision.
		if err := tx.UpdateHarnessConfig(ctx, config); err != nil {
			return err
		}
		for _, secretID := range config.ConfiguredSecretIDs {
			secretID = strings.TrimSpace(secretID)
			if secretID == "" {
				continue
			}
			if s.sandboxes != nil {
				if err := s.sandboxes.MarkSecretHoldersDirtyTx(ctx, tx, db, projectID, secretID); err != nil {
					return err
				}
			}
			if err := tx.DeleteSecret(ctx, projectID, secretID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		config.ConfiguredFiles = nil
		config.ConfiguredSecretIDs = nil
		if includeOverrides && config.FileOverrides != nil {
			config.FileOverrides = []model.HarnessConfigFile{}
		}
		return tx.UpdateHarnessConfig(ctx, config)
	}); err != nil {
		return nil, err
	}
	return s.store.GetHarnessConfig(ctx, projectID, configID)
}
