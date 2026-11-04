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

// checkAdoptionTx reads under the project's lock. Every parent writer takes
// that lock, so the ancestor walk and a following write are indivisible.
func checkAdoptionTx(tx *gorm.DB, projectID, requesterID, childID string) (*model.Sandbox, error) {
	conflict := func(message string) error { return apperrors.NewStatusError(http.StatusConflict, message) }
	var targets []model.Sandbox
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ? AND id IN ?", projectID, []string{requesterID, childID}).Order("id ASC").Find(&targets).Error; err != nil {
		return nil, err
	}
	var requester, child *model.Sandbox
	for i := range targets {
		if targets[i].ID == requesterID {
			requester = &targets[i]
		}
		if targets[i].ID == childID {
			child = &targets[i]
		}
	}
	if requester == nil {
		return nil, conflict("requesting sandbox no longer exists in this project")
	}
	if !requester.DiscoboxAPI {
		return nil, conflict("requesting sandbox may not call the discobox API")
	}
	if child == nil {
		return nil, conflict("child sandbox does not exist in this project")
	}
	if child.ID == requester.ID {
		return nil, conflict("a sandbox cannot adopt itself")
	}
	if child.DesiredState == model.DesiredStateDeleted || child.DesiredState == model.DesiredStateArchived || child.State == model.SandboxStateArchived {
		return nil, conflict("an archived or deleting sandbox cannot be adopted")
	}
	if child.ParentSandboxID != nil && *child.ParentSandboxID == requester.ID {
		return nil, conflict("requester already leads this sandbox")
	}
	seen := map[string]bool{requester.ID: true}
	for parent := requester.ParentSandboxID; parent != nil; {
		if *parent == child.ID {
			return nil, conflict("adoption would create a cycle")
		}
		if seen[*parent] {
			return nil, conflict("existing parent chain contains a cycle")
		}
		seen[*parent] = true
		var ancestor model.Sandbox
		if err := tx.Where("project_id = ? AND id = ?", projectID, *parent).First(&ancestor).Error; err != nil {
			return nil, err
		}
		parent = ancestor.ParentSandboxID
	}
	return child, nil
}

func (s *Store) CreateAdoptionRequest(ctx context.Context, req *model.AdoptionRequest) error {
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		if err := lockProject(tx, req.ProjectID); err != nil {
			return err
		}
		if _, err := checkAdoptionTx(tx, req.ProjectID, req.RequesterSandboxID, req.ChildSandboxID); err != nil {
			return err
		}
		var existing model.AdoptionRequest
		err := tx.Where("project_id = ? AND requester_sandbox_id = ? AND child_sandbox_id = ? AND status = ?", req.ProjectID, req.RequesterSandboxID, req.ChildSandboxID, model.SecretRequestStatusPending).First(&existing).Error
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

func (s *Store) GetAdoptionRequest(ctx context.Context, projectID, requestID string) (*model.AdoptionRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.AdoptionRequest](read.Where("project_id = ?", projectID), "id", requestID)
}

func (s *Store) ListAdoptionRequests(ctx context.Context, projectID, status, requesterID string) ([]model.AdoptionRequest, error) {
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
	out := []model.AdoptionRequest{}
	err = query.Order("created_at DESC").Find(&out).Error
	return out, err
}

func (s *Store) DecideAdoptionRequest(ctx context.Context, projectID, requestID, userID string, approve bool) (*model.AdoptionRequest, error) {
	var result *model.AdoptionRequest
	err := s.Transaction(ctx, func(txStore *Store, tx *gorm.DB) error {
		if err := lockProject(tx, projectID); err != nil {
			return err
		}
		req, err := firstByID[model.AdoptionRequest](tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ?", projectID), "id", requestID)
		if err != nil {
			return err
		}
		if req.Status != model.SecretRequestStatusPending {
			return apperrors.NewStatusError(http.StatusConflict, "adoption request is already answered")
		}
		if approve {
			child, err := checkAdoptionTx(tx, projectID, req.RequesterSandboxID, req.ChildSandboxID)
			if err != nil {
				return err
			}
			req.ReplacedParentSandboxID = child.ParentSandboxID
			if err := tx.Model(&model.Sandbox{}).Where("project_id = ? AND id = ?", projectID, child.ID).Update("parent_sandbox_id", req.RequesterSandboxID).Error; err != nil {
				return err
			}
			txStore.sandboxChanged(child.ID)
			req.Status = model.SecretRequestStatusApproved
		} else {
			req.Status = model.SecretRequestStatusDenied
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

// ReleaseSandbox rechecks the caller's authority under the parent-write lock.
// Empty parentID is a person; a sandbox supplies its own ID.
func (s *Store) ReleaseSandbox(ctx context.Context, projectID, sandboxID, parentID string) error {
	return s.Transaction(ctx, func(txStore *Store, tx *gorm.DB) error {
		if err := lockProject(tx, projectID); err != nil {
			return err
		}
		child, err := firstByID[model.Sandbox](tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ?", projectID), "id", sandboxID)
		if err != nil {
			return err
		}
		if child.ParentSandboxID == nil {
			return apperrors.NewStatusError(http.StatusConflict, "sandbox has no parent")
		}
		if parentID != "" && *child.ParentSandboxID != parentID {
			return apperrors.NewStatusError(http.StatusForbidden, "sandbox is no longer your child")
		}
		if err := tx.Model(child).Update("parent_sandbox_id", nil).Error; err != nil {
			return err
		}
		txStore.sandboxChanged(child.ID)
		return nil
	})
}

func (s *Store) clearSandboxChildrenTx(tx *gorm.DB, projectID, parentID string) error {
	var children []model.Sandbox
	// Lock children and the purged sandbox together in the global row order.
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ? AND (parent_sandbox_id = ? OR id = ?)", projectID, parentID, parentID).Order("id ASC").Find(&children).Error; err != nil {
		return err
	}
	for _, child := range children {
		if child.ID == parentID {
			continue
		}
		if err := tx.Model(&model.Sandbox{}).Where("id = ?", child.ID).Update("parent_sandbox_id", nil).Error; err != nil {
			return err
		}
		s.sandboxChanged(child.ID)
	}
	return nil
}
