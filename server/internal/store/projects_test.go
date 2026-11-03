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
	s, db := newTestStoreWithDB(t, false)

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

// A project whose data key cannot be made is not left behind half made: its
// row goes again, and the create fails (ADR 26-11-02-902 §2).
func TestCreateProjectWithoutItsKeyLeavesNoProject(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, true)
	if err := db.Write.Exec("DROP TABLE encryption_keys").Error; err != nil {
		t.Fatal(err)
	}

	err := s.CreateProject(ctx, &model.Project{ID: "project-2", OwnerUserID: "user-1", Name: "Second"})
	if err == nil {
		t.Fatal("CreateProject succeeded with no place to store the data key")
	}
	if _, err := s.GetProject(ctx, "project-2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetProject after the failed create = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateProjectIfNotExists(ctx, &model.Project{ID: "project-3", OwnerUserID: "user-1", Name: "Third"}); err == nil {
		t.Fatal("CreateProjectIfNotExists succeeded with no place to store the data key")
	}
	if _, err := s.GetProject(ctx, "project-3"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetProject after the failed create-if-not-exists = %v, want ErrNotFound", err)
	}
}
