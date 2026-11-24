package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/server/internal/model"
)

func (s *Store) CreateProject(ctx context.Context, project *model.Project) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	if err := write.Create(project).Error; err != nil {
		return err
	}
	return s.ensureProjectKey(ctx, write, project)
}

// ensureProjectKey gives a new project its data key now, outside the
// transactions its first sealed values may be written in (EncryptionKeys).
// A project whose key cannot be made (its provider unreachable) is deleted
// again, so no project is left half made, without its key or the seeding
// that follows its create.
func (s *Store) ensureProjectKey(ctx context.Context, write *gorm.DB, project *model.Project) error {
	err := s.keyring.Ensure(s.keyContext(ctx), project.ID)
	if err == nil {
		return nil
	}
	err = fmt.Errorf("make the data key of project %s: %w", project.ID, err)
	if delErr := write.Delete(project).Error; delErr != nil {
		return errors.Join(err, fmt.Errorf("delete the project again: %w", delErr))
	}
	return err
}

// UpsertProject saves the project's row, all but its egress level, which only
// SetProjectEgress writes (see egressColumn).
func (s *Store) UpsertProject(ctx context.Context, project *model.Project) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Omit(egressColumn).Save(project).Error
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
	if err := s.ensureProjectKey(ctx, write, project); err != nil {
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

// DeleteProject removes a project and its configuration in one transaction.
// Resources outside this cleanup must be removed through their owning services.
func (s *Store) DeleteProject(ctx context.Context, projectID string) error {
	return s.Transaction(ctx, func(txStore *Store, tx *gorm.DB) error {
		// Hold the project row on PostgreSQL so a concurrent foreign-key
		// reference cannot arrive between the checks and the delete.
		project, err := firstByID[model.Project](tx.Clauses(clause.Locking{Strength: "UPDATE"}), "id", projectID)
		if err != nil {
			return err
		}
		if project.Default {
			return fmt.Errorf("project is the default project; make another project the default before deleting it: %w", ErrInUse)
		}
		projectID = project.ID
		for _, blocker := range []struct {
			resource any
			name     string
		}{
			{&model.Sandbox{}, "sandboxes"},
			{&model.Pool{}, "pools"},
			{&model.HostTrustRequest{}, "host trust requests"},
			{&model.HostTrust{}, "host trusts"},
			{&model.SecretRejection{}, "secret rejections"},
		} {
			var count int64
			if err := tx.Model(blocker.resource).Where("project_id = ?", projectID).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				return fmt.Errorf("project has %s: %w", blocker.name, ErrInUse)
			}
		}
		// Secret ciphertext is nulled before the rows are deleted, matching
		// DeleteSecret.
		if err := tx.Model(&model.Secret{}).Where("project_id = ?", projectID).
			Update("encrypted_value", nil).Error; err != nil {
			return err
		}
		for _, scoped := range []any{
			&model.HarnessConfig{},
			&model.SecretBinding{},
			&model.SecretBindingEvent{},
			&model.SecretRequest{},
			&model.AdoptionRequest{},
			&model.APIAccessRequest{},
			&model.SecretRefreshRequest{},
			&model.Secret{},
			&model.SandboxProviderInstance{},
			&model.ProjectSkillsSource{},
			&model.ProjectGateway{},
			&model.ProjectMember{},
			&model.SSHKey{},
		} {
			if err := tx.Where("project_id = ?", projectID).Delete(scoped).Error; err != nil {
				return err
			}
		}
		// The project's data keys go with it, so the copies of its rows that
		// outlive this delete, in a replica, open nothing (ADR 26-11-02-902
		// §2). A backup taken before it still holds the wrapped keys.
		if err := txStore.keyring.Destroy(txStore.keyContext(ctx), projectID); err != nil {
			return err
		}
		if err := tx.Where("scope = ?", projectID).Delete(&model.EncryptionKey{}).Error; err != nil {
			return err
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
