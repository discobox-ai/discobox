package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/obot-platform/discobox/server/internal/model"
)

func TestAgentConfigResourceEvents(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)

	config := &model.AgentConfig{
		ProjectID:      "project-1",
		Name:           "Codex",
		InstallCommand: []string{"npm", "install", "-g", "@openai/codex"},
		RunCommand:     []string{"codex", "exec"},
	}
	if err := s.CreateAgentConfig(ctx, config); err != nil {
		t.Fatalf("create agent config: %v", err)
	}
	config.Name = "Codex Updated"
	if err := s.UpdateAgentConfig(ctx, config); err != nil {
		t.Fatalf("update agent config: %v", err)
	}
	if err := s.DeleteAgentConfig(ctx, config.ProjectID, config.ID); err != nil {
		t.Fatalf("delete agent config: %v", err)
	}

	var events []model.ProjectEvent
	if err := db.Read.WithContext(ctx).
		Where("project_id = ? AND resource_type = ? AND resource_id = ?", config.ProjectID, "agentConfig", config.ID).
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
		var payload model.AgentConfig
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

func TestAgentConfigFilesRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)

	config := &model.AgentConfig{
		ProjectID:  "project-1",
		Name:       "Claude Code",
		RunCommand: []string{"claude"},
		Files: []model.AgentConfigFile{
			{Path: ".claude/settings.json", Content: `{"theme":"dark"}`},
		},
	}
	if err := s.CreateAgentConfig(ctx, config); err != nil {
		t.Fatalf("create agent config: %v", err)
	}

	got, err := s.GetAgentConfig(ctx, config.ProjectID, config.ID)
	if err != nil {
		t.Fatalf("get agent config: %v", err)
	}
	if len(got.Files) != 1 || got.Files[0].Path != ".claude/settings.json" || got.Files[0].Content != `{"theme":"dark"}` {
		t.Fatalf("files = %#v, want round-tripped files", got.Files)
	}
}

func TestDeleteAgentConfigClearsProjectDefault(t *testing.T) {
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

	defaultConfig := &model.AgentConfig{
		ProjectID:  project.ID,
		Name:       "Default",
		RunCommand: []string{"default-agent"},
	}
	if err := s.CreateAgentConfig(ctx, defaultConfig); err != nil {
		t.Fatalf("create default agent config: %v", err)
	}
	otherConfig := &model.AgentConfig{
		ProjectID:  project.ID,
		Name:       "Other",
		RunCommand: []string{"other-agent"},
	}
	if err := s.CreateAgentConfig(ctx, otherConfig); err != nil {
		t.Fatalf("create other agent config: %v", err)
	}

	project.DefaultAgentConfigID = defaultConfig.ID
	if err := s.UpsertProject(ctx, project); err != nil {
		t.Fatalf("set project default agent config: %v", err)
	}

	if err := s.DeleteAgentConfig(ctx, project.ID, otherConfig.ID); err != nil {
		t.Fatalf("delete non-default agent config: %v", err)
	}
	got, err := s.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("get project after non-default delete: %v", err)
	}
	if got.DefaultAgentConfigID != defaultConfig.ID {
		t.Fatalf("default after non-default delete = %q, want %q", got.DefaultAgentConfigID, defaultConfig.ID)
	}

	if err := s.DeleteAgentConfig(ctx, project.ID, defaultConfig.ID); err != nil {
		t.Fatalf("delete default agent config: %v", err)
	}
	got, err = s.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("get project after default delete: %v", err)
	}
	if got.DefaultAgentConfigID != "" {
		t.Fatalf("default after default delete = %q, want empty", got.DefaultAgentConfigID)
	}
}

func TestDeleteAgentConfigHardDeletes(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)

	config := &model.AgentConfig{
		ProjectID:  "project-1",
		Name:       "Codex",
		RunCommand: []string{"codex", "exec"},
	}
	if err := s.CreateAgentConfig(ctx, config); err != nil {
		t.Fatalf("create agent config: %v", err)
	}
	if err := s.DeleteAgentConfig(ctx, config.ProjectID, config.ID); err != nil {
		t.Fatalf("delete agent config: %v", err)
	}

	var count int64
	if err := db.Read.WithContext(ctx).Model(&model.AgentConfig{}).Where("id = ?", config.ID).Count(&count).Error; err != nil {
		t.Fatalf("count deleted agent config: %v", err)
	}
	if count != 0 {
		t.Fatalf("deleted agent config row count = %d, want 0", count)
	}

	recreated := &model.AgentConfig{
		ProjectID:  config.ProjectID,
		Name:       config.Name,
		RunCommand: config.RunCommand,
	}
	if err := s.CreateAgentConfig(ctx, recreated); err != nil {
		t.Fatalf("recreate agent config with same name: %v", err)
	}
}
