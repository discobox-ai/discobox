package store

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/server/internal/model"
)

func (s *Store) CreateProject(ctx context.Context, project *model.Project) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(project).Error
}

func (s *Store) UpsertProject(ctx context.Context, project *model.Project) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Save(project).Error
}

func (s *Store) CreateProjectIfNotExists(ctx context.Context, project *model.Project) (*model.Project, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}
	var out model.Project
	if err := write.First(&out, "id = ?", project.ID).Error; err == nil {
		return &out, nil
	} else if !errors.Is(mapNotFound(err), ErrNotFound) {
		return nil, err
	}
	if err := write.Create(project).Error; err != nil {
		return nil, err
	}
	return project, nil
}

func (s *Store) CreateProjectMemberIfNotExists(ctx context.Context, member *model.ProjectMember) (*model.ProjectMember, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}
	var out model.ProjectMember
	if err := write.First(&out, "project_id = ? AND user_id = ?", member.ProjectID, member.UserID).Error; err == nil {
		return &out, nil
	} else if !errors.Is(mapNotFound(err), ErrNotFound) {
		return nil, err
	}
	if member.Role == "" {
		member.Role = "member"
	}
	if err := write.Create(member).Error; err != nil {
		return nil, err
	}
	return member, nil
}

func (s *Store) ListProjects(ctx context.Context) ([]model.Project, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var projects []model.Project
	err = read.
		Order("created_at ASC").
		Find(&projects).Error
	return projects, err
}

func (s *Store) ListProjectsForUser(ctx context.Context, userID string) ([]model.Project, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var projects []model.Project
	err = read.
		Joins("JOIN project_members ON project_members.project_id = projects.id").
		Where("project_members.user_id = ?", userID).
		Order("projects.default_project DESC, projects.created_at ASC").
		Find(&projects).Error
	return projects, err
}

func (s *Store) GetProject(ctx context.Context, projectID string) (*model.Project, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.Project](read.Preload("SandboxProviderInstances"), "id", projectID)
}

// GetProjectByOwnerAndName resolves a project by the pair that uniquely
// identifies it for a user. Name is the only human-facing handle a project
// has, so it is unique per owner and callers check it before create/rename to
// turn the index violation into a clear conflict.
func (s *Store) GetProjectByOwnerAndName(ctx context.Context, ownerUserID, name string) (*model.Project, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var project model.Project
	if err := read.Where("owner_user_id = ? AND name = ?", ownerUserID, name).First(&project).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &project, nil
}

func (s *Store) GetDefaultProjectForUser(ctx context.Context, userID string) (*model.Project, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var project model.Project
	err = read.
		Joins("JOIN project_members ON project_members.project_id = projects.id").
		Preload("SandboxProviderInstances").
		Where("project_members.user_id = ? AND projects.default_project = ?", userID, true).
		Order("projects.created_at ASC").
		First(&project).Error
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &project, nil
}

// SetDefaultProjectForUser moves the user's default-project flag to projectID.
// The flag lives on the project row, so moving it is a two-statement write that
// has to be atomic: a crash between them would leave the user with two default
// projects (or none), and /projects/default resolves by that flag alone.
func (s *Store) SetDefaultProjectForUser(ctx context.Context, userID, projectID string) (*model.Project, error) {
	var project *model.Project
	err := s.Transaction(ctx, func(txStore *Store, tx *gorm.DB) error {
		if err := tx.Model(&model.Project{}).
			Where("default_project = ? AND id IN (?)", true,
				tx.Model(&model.ProjectMember{}).Select("project_id").Where("user_id = ?", userID)).
			Update("default_project", false).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.Project{}).
			Where("id = ?", projectID).
			Update("default_project", true).Error; err != nil {
			return err
		}
		var err error
		project, err = txStore.GetProject(ctx, projectID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return project, nil
}

func (s *Store) CountSandboxesForProject(ctx context.Context, projectID string) (int64, error) {
	return s.countForProject(ctx, projectID, &model.Sandbox{})
}

func (s *Store) CountPoolsForProject(ctx context.Context, projectID string) (int64, error) {
	return s.countForProject(ctx, projectID, &model.Pool{})
}

func (s *Store) countForProject(ctx context.Context, projectID string, resource any) (int64, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return 0, err
	}
	var count int64
	err = read.Model(resource).Where("project_id = ?", projectID).Count(&count).Error
	return count, err
}

// DeleteProject removes a project and the configuration rows that only exist
// inside it. Runtime-bearing resources are not deleted here: the caller
// refuses the delete while the project still has sandboxes or pools, because
// those own hosts and containers that must be torn down through their own
// reconcilers first.
func (s *Store) DeleteProject(ctx context.Context, projectID string) error {
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		project, err := firstByID[model.Project](tx, "id", projectID)
		if err != nil {
			return err
		}
		// Secret ciphertext is nulled rather than left in soft-deleted rows,
		// matching DeleteSecret.
		if err := tx.Model(&model.Secret{}).Where("project_id = ?", projectID).
			Update("encrypted_value", nil).Error; err != nil {
			return err
		}
		for _, scoped := range []any{
			&model.HarnessConfigSecretBinding{},
			&model.HarnessConfig{},
			&model.SecretGrant{},
			&model.SecretRequest{},
			&model.Secret{},
			&model.SandboxProviderInstance{},
			&model.SandboxAccessIssuerKey{},
			&model.ProjectMember{},
			&model.ProjectEvent{},
		} {
			if err := tx.Where("project_id = ?", projectID).Delete(scoped).Error; err != nil {
				return err
			}
		}
		return tx.Delete(project).Error
	})
}

func (s *Store) IsProjectMember(ctx context.Context, projectID, userID string) (bool, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return false, err
	}
	var count int64
	if err := read.Model(&model.ProjectMember{}).
		Where("project_id = ? AND user_id = ?", projectID, userID).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}
