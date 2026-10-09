package sandboxes

import (
	"errors"
	"net/http"
	"reflect"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// A create's skills are kept with the sandbox's spec, content and all, and a
// read of it names them without their content (ADR 26-10-09-395 §2). Skills
// that would leave their directory are refused with a 400 before anything is
// written.
func TestCreateKeepsItsSkillsAndRefusesBadOnes(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	create := func(name string, skills serverapi.SandboxCreateConfigSkills) (*model.Sandbox, error) {
		config := serverapi.SandboxCreateConfig{Name: name}
		config.SetSkills(serverapi.NewOptSandboxCreateConfigSkills(skills))
		return svc.CreateSandbox(ctx, "project-1", services.CreateSandboxBody{
			HarnessName: serverapi.NewOptString("codex"),
			Config:      config,
		})
	}

	created, err := create("skilled", serverapi.SandboxCreateConfigSkills{
		"foo": {Skill: "# foo", Files: []serverapi.SandboxSkillFile{
			{Path: "bin/run.sh", Content: []byte("#!/bin/sh\n"), Executable: serverapi.NewOptBool(true)},
		}},
		"bar": {Skill: "# bar"},
	})
	if err != nil {
		t.Fatalf("create with skills: %v", err)
	}
	sb, err := st.GetSandbox(ctx, "project-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := sandboxconfig.Skills{
		"foo": {Skill: "# foo", Files: []sandboxconfig.SkillFile{{Path: "bin/run.sh", Content: []byte("#!/bin/sh\n"), Executable: true}}},
		"bar": {Skill: "# bar"},
	}
	if !reflect.DeepEqual(sb.Skills, want) {
		t.Fatalf("stored skills = %#v, want %#v", sb.Skills, want)
	}
	read, err := services.SandboxToAPI(sb, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := read.Config.SkillNames; !reflect.DeepEqual(got, []string{"bar", "foo"}) {
		t.Fatalf("skillNames = %v, want [bar foo]", got)
	}

	_, err = create("escaping", serverapi.SandboxCreateConfigSkills{
		"evil": {Skill: "x", Files: []serverapi.SandboxSkillFile{{Path: "../../.bashrc", Content: []byte("x")}}},
	})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("err = %v, want a 400", err)
	}
	all, err := st.ListSandboxes(ctx, "project-1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("sandboxes = %d, want only the skilled one: a refused create left one behind", len(all))
	}
}

// A sandbox created without skills keeps the fingerprint it had before skills
// existed, so no container is rebuilt for them (ADR 26-10-09-395 §2).
func TestSkillsLeaveTheFingerprintOfASandboxWithoutThem(t *testing.T) {
	manifest := model.SandboxManifest{HarnessMode: "run", Image: "img"}
	before := manifest.Fingerprint()
	manifest.Skills = sandboxconfig.Skills{}
	if manifest.Fingerprint() != before {
		t.Fatal("an empty skills map changed the fingerprint")
	}
	manifest.Skills = sandboxconfig.Skills{"foo": {Skill: "# foo"}}
	if manifest.Fingerprint() == before {
		t.Fatal("skills did not change the fingerprint")
	}
}
