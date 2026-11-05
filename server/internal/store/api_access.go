package store

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CreateAPIAccessRequest shares the project lock with creation, purge and parent changes.
// The decision and the setting commit together; requests survive sandbox purge.
func (s *Store) CreateAPIAccessRequest(ctx context.Context, req *model.APIAccessRequest) error {
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		if err := lockProject(tx, req.ProjectID); err != nil {
			return err
		}
		box, err := firstByID[model.Sandbox](tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ?", req.ProjectID), "id", req.RequesterSandboxID)
		if err != nil {
			return err
		}
		if box.DesiredState == model.DesiredStateDeleted || box.DesiredState == model.DesiredStateArchived {
			return apperrors.NewStatusError(http.StatusConflict, "an archived or deleting sandbox cannot request API access")
		}
		if box.DiscoboxAPI {
			return apperrors.NewStatusError(http.StatusConflict, "this sandbox already has API access")
		}
		var existing model.APIAccessRequest
		err = tx.Where("project_id = ? AND requester_sandbox_id = ? AND status = ?", req.ProjectID, req.RequesterSandboxID, model.SecretRequestStatusPending).First(&existing).Error
		if err == nil {
			*req = existing
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return tx.Create(req).Error
	})
}
func (s *Store) GetAPIAccessRequest(ctx context.Context, projectID, requestID string) (*model.APIAccessRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.APIAccessRequest](read.Where("project_id = ?", projectID), "id", requestID)
}
func (s *Store) ListAPIAccessRequests(ctx context.Context, projectID, status, requesterID string) ([]model.APIAccessRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.Where("project_id = ?", projectID)
	if status != "" {
		query = query.Where("status = ?", status)
	}
	if requesterID != "" {
		query = query.Where("requester_sandbox_id = ?", requesterID)
	}
	out := []model.APIAccessRequest{}
	err = query.Order("created_at DESC").Find(&out).Error
	return out, err
}
func (s *Store) DecideAPIAccessRequest(ctx context.Context, projectID, requestID, userID string, approve bool) (*model.APIAccessRequest, error) {
	var result *model.APIAccessRequest
	err := s.Transaction(ctx, func(txStore *Store, tx *gorm.DB) error {
		if err := lockProject(tx, projectID); err != nil {
			return err
		}
		req, err := firstByID[model.APIAccessRequest](tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ?", projectID), "id", requestID)
		if err != nil {
			return err
		}
		if req.Status != model.SecretRequestStatusPending {
			return apperrors.NewStatusError(http.StatusConflict, "API access request is already answered")
		}
		req.Status = model.SecretRequestStatusDenied
		if approve {
			box, err := firstByID[model.Sandbox](tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ?", projectID), "id", req.RequesterSandboxID)
			if err != nil {
				return err
			}
			if box.DesiredState == model.DesiredStateDeleted || box.DesiredState == model.DesiredStateArchived {
				return apperrors.NewStatusError(http.StatusConflict, "an archived or deleting sandbox cannot receive API access")
			}
			if err := txStore.SetSandboxDiscoboxAPI(ctx, projectID, box.ID, true); err != nil {
				return err
			}
			req.Status = model.SecretRequestStatusApproved
		}
		now := time.Now().UTC()
		req.DecidedAt = &now
		req.DecidedBy = userID
		if err := tx.Save(req).Error; err != nil {
			return err
		}
		result = req
		return nil
	})
	return result, err
}
