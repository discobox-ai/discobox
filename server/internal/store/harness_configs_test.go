package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/obot-platform/discobox/server/internal/model"
)

func TestGetHarnessConfigBySlug(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)

	config := &model.HarnessConfig{
		ProjectID: "project-1",
		Slug:      "codex",
		BuiltIn:   true,
		Name:      "Codex",
	}
	if err := s.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	got, err := s.GetHarnessConfigBySlug(ctx, "project-1", "codex")
	if err != nil {
		t.Fatalf("get by slug: %v", err)
	}
	if got.ID != config.ID || !got.BuiltIn {
		t.Fatalf("get by slug = %#v", got)
	}
	if _, err := s.GetHarnessConfigBySlug(ctx, "project-1", "missing"); err == nil {
		t.Fatalf("expected not-found for missing slug")
	}
	// Slug lookup is project-scoped.
	if _, err := s.GetHarnessConfigBySlug(ctx, "project-2", "codex"); err == nil {
		t.Fatalf("expected not-found for slug in another project")
	}
}

func TestHarnessConfigResourceEvents(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)

	config := &model.HarnessConfig{
		ProjectID:  "project-1",
		Name:       "Codex",
		RunCommand: []string{"codex", "exec"},
	}
	if err := s.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	config.Name = "Codex Updated"
	if err := s.UpdateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("update harness config: %v", err)
	}
	if err := s.DeleteHarnessConfig(ctx, config.ProjectID, config.ID); err != nil {
		t.Fatalf("delete harness config: %v", err)
	}

	var events []model.ProjectEvent
	if err := db.Read.WithContext(ctx).
		Where("project_id = ? AND resource_type = ? AND resource_id = ?", config.ProjectID, "harnessConfig", config.ID).
		Order("seq ASC").
		Find(&events).Error; err != nil {
		t.Fatalf("list project events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("event count = %d, want 3", len(events))
	}
	wantActions := []string{model.EventActionCreated, model.EventActionUpdated, model.EventActionDeleted}
	for i, event := range events {
		if event.Type != model.EventTypeResourceChanged {
			t.Fatalf("event[%d].type = %q, want %q", i, event.Type, model.EventTypeResourceChanged)
		}
		if event.Action != wantActions[i] {
			t.Fatalf("event[%d].action = %q, want %q", i, event.Action, wantActions[i])
		}
		var payload model.HarnessConfig
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatalf("decode event[%d] payload: %v", i, err)
		}
		if payload.ID != config.ID || payload.ProjectID != config.ProjectID {
			t.Fatalf("event[%d] payload = %#v, want config identity", i, payload)
		}
	}
	if events[1].Seq <= events[0].Seq || events[2].Seq <= events[1].Seq {
		t.Fatalf("event seqs are not increasing: %d, %d, %d", events[0].Seq, events[1].Seq, events[2].Seq)
	}
}

func TestHarnessConfigFilesRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)

	config := &model.HarnessConfig{
		ProjectID:  "project-1",
		Name:       "Claude Code",
		RunCommand: []string{"claude"},
		Files: []model.HarnessConfigFile{
			{Path: ".claude/settings.json", Content: `{"theme":"dark"}`},
		},
	}
	if err := s.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}

	got, err := s.GetHarnessConfig(ctx, config.ProjectID, config.ID)
	if err != nil {
		t.Fatalf("get harness config: %v", err)
	}
	if len(got.Files) != 1 || got.Files[0].Path != ".claude/settings.json" || got.Files[0].Content != `{"theme":"dark"}` {
		t.Fatalf("files = %#v, want round-tripped files", got.Files)
	}
}

func TestDeleteHarnessConfigClearsProjectDefault(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)

	project := &model.Project{
		ID:          "project-1",
		OwnerUserID: "user-1",
		Name:        "Project",
		Slug:        "project",
	}
	if err := s.UpsertProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	defaultConfig := &model.HarnessConfig{
		ProjectID:  project.ID,
		Name:       "Default",
		RunCommand: []string{"default-harness"},
	}
	if err := s.CreateHarnessConfig(ctx, defaultConfig); err != nil {
		t.Fatalf("create default harness config: %v", err)
	}
	otherConfig := &model.HarnessConfig{
		ProjectID:  project.ID,
		Name:       "Other",
		RunCommand: []string{"other-harness"},
	}
	if err := s.CreateHarnessConfig(ctx, otherConfig); err != nil {
		t.Fatalf("create other harness config: %v", err)
	}

	project.DefaultHarnessConfigID = defaultConfig.ID
	if err := s.UpsertProject(ctx, project); err != nil {
		t.Fatalf("set project default harness config: %v", err)
	}

	if err := s.DeleteHarnessConfig(ctx, project.ID, otherConfig.ID); err != nil {
		t.Fatalf("delete non-default harness config: %v", err)
	}
	got, err := s.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("get project after non-default delete: %v", err)
	}
	if got.DefaultHarnessConfigID != defaultConfig.ID {
		t.Fatalf("default after non-default delete = %q, want %q", got.DefaultHarnessConfigID, defaultConfig.ID)
	}

	if err := s.DeleteHarnessConfig(ctx, project.ID, defaultConfig.ID); err != nil {
		t.Fatalf("delete default harness config: %v", err)
	}
	got, err = s.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("get project after default delete: %v", err)
	}
	if got.DefaultHarnessConfigID != "" {
		t.Fatalf("default after default delete = %q, want empty", got.DefaultHarnessConfigID)
	}
}

func TestDeleteHarnessConfigHardDeletes(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)

	config := &model.HarnessConfig{
		ProjectID:  "project-1",
		Name:       "Codex",
		RunCommand: []string{"codex", "exec"},
	}
	if err := s.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	if err := s.DeleteHarnessConfig(ctx, config.ProjectID, config.ID); err != nil {
		t.Fatalf("delete harness config: %v", err)
	}

	var count int64
	if err := db.Read.WithContext(ctx).Model(&model.HarnessConfig{}).Where("id = ?", config.ID).Count(&count).Error; err != nil {
		t.Fatalf("count deleted harness config: %v", err)
	}
	if count != 0 {
		t.Fatalf("deleted harness config row count = %d, want 0", count)
	}

	recreated := &model.HarnessConfig{
		ProjectID:  config.ProjectID,
		Name:       config.Name,
		RunCommand: config.RunCommand,
	}
	if err := s.CreateHarnessConfig(ctx, recreated); err != nil {
		t.Fatalf("recreate harness config with same name: %v", err)
	}
}
