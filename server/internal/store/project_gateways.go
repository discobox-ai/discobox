package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// ErrGatewayConflict is a gateway whose match another gateway of the project
// already has: a request would then match two (ADR 26-10-16-470 §2).
var ErrGatewayConflict = errors.New("gateway conflicts with the project's gateways")

// ListProjectGateways returns a project's gateways by name.
func (s *Store) ListProjectGateways(ctx context.Context, projectID string) ([]model.ProjectGateway, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.ProjectGateway
	err = read.Where("project_id = ?", projectID).Order("name ASC").Find(&out).Error
	return out, err
}

// GetProjectGateway returns a project's gateway by name.
func (s *Store) GetProjectGateway(ctx context.Context, projectID, name string) (*model.ProjectGateway, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out model.ProjectGateway
	err = read.Where("project_id = ? AND name = ?", projectID, name).Take(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// PutProjectGateway creates the gateway, or replaces the one of its name in
// place, keeping its ID, creator, and creation time. It holds the project's
// row for update first, so two puts at once take turns: on a database with
// more than one writer the second sees the first's match, and the unique
// index alone would answer it with an index violation rather than the
// conflict.
func (s *Store) PutProjectGateway(ctx context.Context, gw *model.ProjectGateway) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Transaction(func(tx *gorm.DB) error {
		var project model.Project
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&project, "id = ?", gw.ProjectID).Error; err != nil {
			return err
		}
		var other model.ProjectGateway
		err := tx.Where("project_id = ? AND match_url = ? AND name <> ?", gw.ProjectID, gw.Match, gw.Name).Take(&other).Error
		switch {
		case err == nil:
			return fmt.Errorf("%w: gateway %s already matches %s", ErrGatewayConflict, other.Name, gw.Match)
		case !errors.Is(err, gorm.ErrRecordNotFound):
			return err
		}
		var existing model.ProjectGateway
		err = tx.Where("project_id = ? AND name = ?", gw.ProjectID, gw.Name).Take(&existing).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			return tx.Create(gw).Error
		case err != nil:
			return err
		}
		gw.ID, gw.CreatedBy, gw.CreatedAt = existing.ID, existing.CreatedBy, existing.CreatedAt
		return tx.Select("match_url", "target_url", "request_headers", "updated_at").Save(gw).Error
	})
}

// DeleteProjectGateway removes a project's gateway by name.
func (s *Store) DeleteProjectGateway(ctx context.Context, projectID, name string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	result := write.Where("project_id = ? AND name = ?", projectID, name).Delete(&model.ProjectGateway{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
