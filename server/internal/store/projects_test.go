package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func TestSoftDeletedProjectMemberExcludesProjectForUser(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)

	project := &model.Project{
		ID:          "project-1",
		OwnerUserID: "user-1",
		Name:        "Project",
		Default:     true,
	}
	if err := s.UpsertProject(ctx, project); err != nil {
		t.Fatalf("mark project default: %v", err)
	}
	member := &model.ProjectMember{ProjectID: project.ID, UserID: "user-1", Role: "owner"}
	if _, err := s.CreateProjectMemberIfNotExists(ctx, member); err != nil {
		t.Fatalf("create project member: %v", err)
	}

	projects, err := s.ListProjectsForUser(ctx, "user-1")
	if err != nil {
		t.Fatalf("list projects before delete: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != project.ID {
		t.Fatalf("projects before delete = %#v, want project", projects)
	}
	if got, err := s.GetDefaultProjectForUser(ctx, "user-1"); err != nil || got.ID != project.ID {
		t.Fatalf("default project before delete = %#v, %v; want project", got, err)
	}

	if err := db.Write.WithContext(ctx).Delete(member).Error; err != nil {
		t.Fatalf("soft delete project member: %v", err)
	}

	projects, err = s.ListProjectsForUser(ctx, "user-1")
	if err != nil {
		t.Fatalf("list projects after delete: %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("projects after delete = %#v, want none", projects)
	}
	if _, err := s.GetDefaultProjectForUser(ctx, "user-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("default project after delete error = %v, want ErrNotFound", err)
	}
}
