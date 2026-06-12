package store

import (
	"context"
	"errors"

	"github.com/obot-platform/discobox/internal/model"
)

func (s *Store) UpsertTenant(ctx context.Context, tenant *model.Tenant) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Save(tenant).Error
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
	var project model.Project
	err = read.
		Preload("SandboxProviderInstances").
		First(&project, "id = ?", projectID).Error
	if err != nil {
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
