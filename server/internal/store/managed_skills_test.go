package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func TestMutateSandboxSkillsRecordsIntentAndAuditOnce(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	inventoryTestSandbox(t, s, "sandbox-1")

	change := sandboxconfig.SkillMutation{
		Operation: "set", Origin: sandboxconfig.SkillOrigin{SourceID: "manager"},
		Skills: sandboxconfig.Skills{"one": {Skill: "consider this instruction"}},
	}
	allowed := func(*store.Store) error { return nil }
	first, err := s.MutateSandboxSkills(ctx, "project-1", "sandbox-1", "lead", change, allowed)
	if err != nil || first.Desired.Revision != 1 {
		t.Fatalf("first update: revision %d, %v", first.Desired.Revision, err)
	}
	retry, err := s.MutateSandboxSkills(ctx, "project-1", "sandbox-1", "lead", change, allowed)
	if err != nil || retry.Desired.Revision != first.Desired.Revision {
		t.Fatalf("retry: revision %d, %v", retry.Desired.Revision, err)
	}
	refused := errors.New("released")
	change.Skills["one"] = sandboxconfig.Skill{Skill: "refused change"}
	if _, err := s.MutateSandboxSkills(ctx, "project-1", "sandbox-1", "lead", change, func(*store.Store) error {
		return refused
	}); !errors.Is(err, refused) {
		t.Fatalf("authorization failure: %v", err)
	}

	events, err := s.ListSandboxEventsAfter(ctx, "project-1", 0, 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("got %d events, %v", len(events), err)
	}
	var data model.SandboxEventData
	if err := json.Unmarshal(events[1].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.SkillChange == nil || data.SkillChange.Actor != "lead" ||
		data.SkillChange.Revision != 1 || data.SkillChange.Skills["one"].After == nil {
		t.Fatalf("incorrect audit metadata: %+v", data.SkillChange)
	}
	if bytes.Contains(events[1].Data, []byte("consider this instruction")) {
		t.Fatal("audit contains the skill body")
	}
}
