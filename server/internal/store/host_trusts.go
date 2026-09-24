package store

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// Host trusts (ADR 0149): an agent's asks to trust a host, and the pins a
// person approved from them. Both belong to one sandbox and are deleted with
// it (deleteSandboxHostTrustsTx).

func (s *Store) CreateHostTrustRequest(ctx context.Context, req *model.HostTrustRequest) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(req).Error
}

// FindPendingHostTrustRequest returns the sandbox's open ask for host, or
// ErrNotFound, so an agent that asks again reuses its request instead of
// adding another line to the inbox.
func (s *Store) FindPendingHostTrustRequest(ctx context.Context, projectID, sandboxID, host string) (*model.HostTrustRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.HostTrustRequest
	err = read.Where("project_id = ? AND sandbox_id = ? AND host = ? AND status = ?",
		projectID, sandboxID, host, model.HostTrustRequestStatusPending).
		Order("created_at DESC").Limit(1).Find(&out).Error
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return &out[0], nil
}

// UpdateHostTrustRequestIfPending saves a still-pending request, refusing with
// ErrGenerationConflict once it has been answered.
func (s *Store) UpdateHostTrustRequestIfPending(ctx context.Context, req *model.HostTrustRequest) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	result := write.Model(&model.HostTrustRequest{}).
		Where("project_id = ? AND id = ? AND status = ?", req.ProjectID, req.ID, model.HostTrustRequestStatusPending).
		Select("*").
		Updates(req)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrGenerationConflict
	}
	return nil
}

func (s *Store) GetHostTrustRequest(ctx context.Context, projectID, requestID string) (*model.HostTrustRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.HostTrustRequest](read.Where("project_id = ?", projectID), "id", requestID)
}

// ListHostTrustRequests returns a project's trust requests, newest first,
// optionally narrowed to one status.
func (s *Store) ListHostTrustRequests(ctx context.Context, projectID, status string) ([]model.HostTrustRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.Where("project_id = ?", projectID)
	if status != "" {
		query = query.Where("status = ?", status)
	}
	out := []model.HostTrustRequest{}
	err = query.Order("created_at DESC").Find(&out).Error
	return out, err
}

// ApproveHostTrustRequest mints trust and marks req approved by it, in one
// transaction, and only while req is still pending: two people answering the
// same ask produce one trust, and the second is told it lost.
func (s *Store) ApproveHostTrustRequest(ctx context.Context, req *model.HostTrustRequest, trust *model.HostTrust) error {
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		if err := tx.Create(trust).Error; err != nil {
			return err
		}
		result := tx.Model(&model.HostTrustRequest{}).
			Where("project_id = ? AND id = ? AND status = ?", req.ProjectID, req.ID, model.HostTrustRequestStatusPending).
			Updates(map[string]any{"status": model.HostTrustRequestStatusApproved, "trust_id": trust.ID, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrGenerationConflict
		}
		req.Status = model.HostTrustRequestStatusApproved
		req.TrustID = trust.ID
		return nil
	})
}

// GetHostTrust returns a trust by ID within a project, live or not.
func (s *Store) GetHostTrust(ctx context.Context, projectID, trustID string) (*model.HostTrust, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.HostTrust](read.Where("project_id = ?", projectID), "id", trustID)
}

// ListLiveSandboxHostTrusts returns the trusts a sandbox holds at now.
func (s *Store) ListLiveSandboxHostTrusts(ctx context.Context, projectID, sandboxID string, now time.Time) ([]model.HostTrust, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	out := []model.HostTrust{}
	err = read.Where("project_id = ? AND sandbox_id = ? AND expires_at > ?", projectID, sandboxID, now.UTC()).
		Order("created_at ASC").Find(&out).Error
	return out, err
}

// ListLivePoolHostTrusts returns the live trusts of every sandbox placed on
// the pool: what that pool's proxy enforces.
func (s *Store) ListLivePoolHostTrusts(ctx context.Context, poolID string, now time.Time) ([]model.HostTrust, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	out := []model.HostTrust{}
	err = read.
		Where("expires_at > ? AND sandbox_id IN (?)", now.UTC(),
			read.Model(&model.Sandbox{}).Select("id").Where("pool_id = ?", poolID)).
		Order("created_at ASC").Find(&out).Error
	return out, err
}

// DeleteHostTrust revokes one of a sandbox's trusts. The lookup and the
// delete share a transaction because trustID may be an ID prefix (firstByID).
func (s *Store) DeleteHostTrust(ctx context.Context, projectID, sandboxID, trustID string) error {
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		trust, err := firstByID[model.HostTrust](tx.Where("project_id = ? AND sandbox_id = ?", projectID, sandboxID), "id", trustID)
		if err != nil {
			return err
		}
		return tx.Delete(trust).Error
	})
}

// deleteSandboxHostTrustsTx removes everything a sandbox's host trust asks
// left behind: a trust is a sandbox's alone and must not outlive it.
func deleteSandboxHostTrustsTx(tx *gorm.DB, sandboxID string) error {
	if err := tx.Where("sandbox_id = ?", sandboxID).Delete(&model.HostTrust{}).Error; err != nil {
		return err
	}
	return tx.Where("sandbox_id = ?", sandboxID).Delete(&model.HostTrustRequest{}).Error
}
