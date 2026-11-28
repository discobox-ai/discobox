package sandboxes

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandboxexport"
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
	read, err := services.SandboxToAPI(context.Background(), sb, services.SandboxUpgradeBasis{})
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
	all, err := st.ListSandboxes(ctx, "project-1", "", nil, "")
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
	manifest := model.SandboxManifest{Image: "img"}
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

// An archive is the caller's file: skills the sandbox would refuse at its first
// launch are refused at import, before the tree is read, and good ones come
// through whole (ADR 26-10-09-395 §1).
func TestImportHoldsSkillsToTheCreatesLine(t *testing.T) {
	ctx, svc, st, provider := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	bad := exportArchive(t, func(m *sandboxexport.Manifest) {
		m.Sandbox.Manifest.Skills = sandboxconfig.Skills{"evil": {Skill: "x", Files: []sandboxconfig.SkillFile{{Path: "../../.bashrc"}}}}
	}, map[string]string{"data/big": "pretend this is a workspace"})

	_, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(bad), services.SandboxImportOptions{})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("err = %v, want a 400", err)
	}
	if provider.imported != nil {
		t.Fatal("the tree was uploaded before the refusal")
	}

	good := exportArchive(t, func(m *sandboxexport.Manifest) {
		m.Sandbox.Manifest.Skills = sandboxconfig.Skills{"foo": {Skill: "# foo"}}
	}, nil)
	result, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(good), services.SandboxImportOptions{})
	if err != nil {
		t.Fatalf("import with good skills: %v", err)
	}
	sb, err := st.GetSandbox(ctx, "project-1", result.Sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sb.Skills, sandboxconfig.Skills{"foo": {Skill: "# foo"}}) {
		t.Fatalf("imported skills = %#v", sb.Skills)
	}
}

// A create naming a source directory or a home the pool would not place — it
// would put the source at the working root, or create the account with no
// home, instead — is refused with a 400 naming the field, before anything is
// written.
func TestCreateRefusesADirectoryThePoolWouldNotPlace(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	placed := serverapi.SandboxCreateConfig{Name: "placed"}
	placed.SetSource(serverapi.NewOptGitSource(serverapi.GitSource{
		Kind:        serverapi.GitSourceKindGit,
		Destination: serverapi.NewOptGitSourceDestination(serverapi.GitSourceDestination{Directory: serverapi.NewOptString("/work\tspace")}),
	}))
	homed := serverapi.SandboxCreateConfig{Name: "homed"}
	homed.SetUser(serverapi.NewOptSandboxUser(serverapi.SandboxUser{
		Name:          serverapi.NewOptString("discobox"),
		UID:           serverapi.NewOptInt64(10000),
		Gid:           serverapi.NewOptInt64(10000),
		HomeDirectory: serverapi.NewOptString("/home/dis\tcobox"),
	}))
	for _, tc := range []struct {
		config serverapi.SandboxCreateConfig
		field  string
	}{
		{placed, "source directory"},
		{homed, "home directory"},
	} {
		_, err := svc.CreateSandbox(ctx, "project-1", services.CreateSandboxBody{
			HarnessName: serverapi.NewOptString("codex"),
			Config:      tc.config,
		})
		var status interface{ StatusCode() int }
		if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
			t.Fatalf("%s: err = %v, want a 400", tc.config.Name, err)
		}
		if !strings.Contains(err.Error(), tc.field) {
			t.Fatalf("%s: err %q does not name %q", tc.config.Name, err, tc.field)
		}
	}
	all, err := st.ListSandboxes(ctx, "project-1", "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("sandboxes = %d, want none: a refused create left one behind", len(all))
	}
}
