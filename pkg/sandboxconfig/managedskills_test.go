package sandboxconfig

import (
	"errors"
	"strings"
	"testing"
)

func TestManagedSkillSyncReplacesItsSourceAtExpectedRevision(t *testing.T) {
	origin := SkillOrigin{SourceID: "manager", Commit: strings.Repeat("a", 40)}
	start := ManagedSkillSet{}
	followed, err := start.Mutate(SkillMutation{
		Operation: "follow", Origin: origin, ExpectedRevision: &start.Revision,
		Skills: Skills{"keep": {Skill: "old"}, "obsolete": {Skill: "old"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	added, err := followed.Mutate(SkillMutation{
		Operation: "add", Origin: SkillOrigin{SourceID: "local"},
		Skills: Skills{"other": {Skill: "unchanged"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	origin.Commit = strings.Repeat("b", 40)
	sync := SkillMutation{
		Operation: "sync", Origin: origin, ExpectedRevision: &followed.Revision,
		Skills: Skills{"keep": {Skill: "new"}},
	}
	if _, err := added.Mutate(sync); !errors.Is(err, ErrSkillRevisionChanged) {
		t.Fatalf("stale publication error = %v", err)
	}
	sync.ExpectedRevision = &added.Revision
	synced, err := added.Mutate(sync)
	if err != nil {
		t.Fatal(err)
	}
	if synced.Revision != added.Revision+1 || len(synced.Skills) != 2 ||
		synced.Skills["other"].Skill != "unchanged" || synced.Skills["keep"].Skill != "new" ||
		synced.Versions["keep"].Commit != origin.Commit || synced.Sources[origin.SourceID] != origin {
		t.Fatalf("unexpected synchronized set: %#v", synced)
	}
	if len(added.Skills) != 3 || added.Skills["keep"].Skill != "old" {
		t.Fatal("sync changed the original set")
	}
	sync.ExpectedRevision = &synced.Revision
	retried, err := synced.Mutate(sync)
	if err != nil || retried.Revision != synced.Revision {
		t.Fatalf("identical publication changed the revision: %d, %v", retried.Revision, err)
	}
}

func TestManagedSkillMutationChecksCombinedLimit(t *testing.T) {
	origin := SkillOrigin{SourceID: "local"}
	input := SkillMutation{Operation: "set", Origin: origin,
		Skills: Skills{"first": {Skill: strings.Repeat("a", MaxSkillsBytes)}},
	}
	current, err := (ManagedSkillSet{}).Mutate(input)
	if err != nil {
		t.Fatal(err)
	}
	next, err := current.Mutate(SkillMutation{Operation: "add", Origin: origin,
		Skills: Skills{"second": {Skill: "extra"}},
	})
	if err == nil || next.Revision != current.Revision || len(next.Skills) != 1 {
		t.Fatalf("oversized mutation changed the set: revision %d, %d skills, %v", next.Revision, len(next.Skills), err)
	}
}

func TestSkillDigestIgnoresFileOrder(t *testing.T) {
	a := SkillFile{Path: "a", Content: []byte("payload")}
	b := SkillFile{Path: "b", Executable: true}
	skill := Skill{Skill: "text", Files: []SkillFile{b, a}}
	first, err := skill.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if skill.Files[0].Path != "b" {
		t.Fatal("digest reordered the original files")
	}
	skill.Files[0], skill.Files[1] = skill.Files[1], skill.Files[0]
	second, err := skill.Digest()
	if err != nil || first != second {
		t.Fatalf("reordered files changed digest: %q, %v", second, err)
	}
	skill.Files[1].Executable = false
	third, err := skill.Digest()
	if err != nil || third == second {
		t.Fatalf("executable bit did not change digest: %q, %v", third, err)
	}
}
