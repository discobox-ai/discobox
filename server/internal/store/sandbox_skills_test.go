package store_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// A listing answers with the names of a sandbox's skills and leaves their
// content unloaded, and saving a row a listing loaded puts no empty set back
// over them: skills are written by the insert alone (ADR 26-10-09-395 §2).
func TestListingsCarrySkillNamesAndSavesKeepTheSkills(t *testing.T) {
	ctx := context.Background()
	st, _ := newSandboxNameTestStore(t)
	skills := sandboxconfig.Skills{"b": {Skill: "# b"}, "a": {Skill: "# a", Files: []sandboxconfig.SkillFile{{Path: "x", Content: []byte("x")}}}}
	if err := st.CreateSandbox(ctx, &model.Sandbox{
		ID: "sb-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "skilled",
		SandboxManifest: model.SandboxManifest{Skills: skills},
	}); err != nil {
		t.Fatal(err)
	}

	listed, err := st.ListSandboxes(ctx, "project-1", "", nil)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListSandboxes() = %d, %v", len(listed), err)
	}
	if listed[0].Skills != nil {
		t.Fatalf("listed skills = %#v, want them unloaded", listed[0].Skills)
	}
	if !reflect.DeepEqual(listed[0].SkillNames, []string{"a", "b"}) {
		t.Fatalf("listed names = %v, want [a b]", listed[0].SkillNames)
	}

	listed[0].Name = "renamed"
	if err := st.UpdateSandbox(ctx, &listed[0]); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || !reflect.DeepEqual(got.Skills, skills) || !reflect.DeepEqual(got.SkillNames, []string{"a", "b"}) {
		t.Fatalf("after a save of the listed row: name %q, skills %#v, names %v; want the skills kept", got.Name, got.Skills, got.SkillNames)
	}
}
