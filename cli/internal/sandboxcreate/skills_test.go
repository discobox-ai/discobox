package sandboxcreate

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

func writeSkillsTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// Every subdirectory holding a SKILL.md is one skill, its other files carried
// with it, slash-separated and with the executable bit; anything else is not a
// skill and .git is not part of one.
func TestReadSkillsReadsEverySkillInADirectory(t *testing.T) {
	dir := t.TempDir()
	writeSkillsTestFile(t, filepath.Join(dir, "foo", "SKILL.md"), "# foo", 0o644)
	writeSkillsTestFile(t, filepath.Join(dir, "foo", "scripts", "run.sh"), "#!/bin/sh\n", 0o755)
	writeSkillsTestFile(t, filepath.Join(dir, "foo", ".git", "HEAD"), "ref", 0o644)
	writeSkillsTestFile(t, filepath.Join(dir, "notaskill", "README.md"), "no", 0o644)
	writeSkillsTestFile(t, filepath.Join(dir, ".hidden", "SKILL.md"), "no", 0o644)
	writeSkillsTestFile(t, filepath.Join(dir, "loose.md"), "no", 0o644)

	skills, err := ReadSkills([]string{dir}, false)
	if err != nil {
		t.Fatalf("ReadSkills() = %v", err)
	}
	want := sandboxconfig.Skills{"foo": {Skill: "# foo", Files: []sandboxconfig.SkillFile{
		{Path: "scripts/run.sh", Content: []byte("#!/bin/sh\n"), Executable: runtime.GOOS != "windows"},
	}}}
	if !reflect.DeepEqual(skills, want) {
		t.Fatalf("ReadSkills() = %#v, want %#v", skills, want)
	}
}

// --user-skills reads ~/.claude/skills and then ~/.agents/skills, and every
// --skills after them in order; the last declaration of a name wins whole.
func TestReadSkillsOrdersHomeThenDirectoriesAndTheLastWins(t *testing.T) {
	home, first, second := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writeSkillsTestFile(t, filepath.Join(home, ".claude", "skills", "a", "SKILL.md"), "claude a", 0o644)
	writeSkillsTestFile(t, filepath.Join(home, ".claude", "skills", "a", "extra.md"), "claude extra", 0o644)
	writeSkillsTestFile(t, filepath.Join(home, ".claude", "skills", "b", "SKILL.md"), "claude b", 0o644)
	writeSkillsTestFile(t, filepath.Join(home, ".agents", "skills", "b", "SKILL.md"), "agents b", 0o644)
	writeSkillsTestFile(t, filepath.Join(home, ".agents", "skills", "c", "SKILL.md"), "agents c", 0o644)
	writeSkillsTestFile(t, filepath.Join(first, "a", "SKILL.md"), "first a", 0o644)
	writeSkillsTestFile(t, filepath.Join(first, "c", "SKILL.md"), "first c", 0o644)
	writeSkillsTestFile(t, filepath.Join(second, "c", "SKILL.md"), "second c", 0o644)

	skills, err := ReadSkills([]string{first, second}, true)
	if err != nil {
		t.Fatalf("ReadSkills() = %v", err)
	}
	got := map[string]string{}
	for name, skill := range skills {
		got[name] = skill.Skill
	}
	want := map[string]string{"a": "first a", "b": "agents b", "c": "second c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("skills = %v, want %v", got, want)
	}
	if len(skills["a"].Files) != 0 {
		t.Fatalf("a's files = %v, want the replaced skill's files gone with it", skills["a"].Files)
	}
}

// Without --user-skills nothing is read from home, and a home with neither
// directory is not an error with it.
func TestReadSkillsReadsHomeOnlyWhenAskedAndToleratesItsAbsence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writeSkillsTestFile(t, filepath.Join(home, ".claude", "skills", "a", "SKILL.md"), "a", 0o644)
	if skills, err := ReadSkills(nil, false); err != nil || skills != nil {
		t.Fatalf("ReadSkills(nil, false) = %v, %v; want nothing", skills, err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	if skills, err := ReadSkills(nil, true); err != nil || skills != nil {
		t.Fatalf("ReadSkills(nil, true) on an empty home = %v, %v; want nothing", skills, err)
	}
}

// A --skills directory that is not there was named by mistake, and an empty
// one names nothing.
func TestReadSkillsRefusesAMissingDirectory(t *testing.T) {
	if _, err := ReadSkills([]string{filepath.Join(t.TempDir(), "missing")}, false); err == nil {
		t.Fatal("ReadSkills() = nil, want an error for a missing --skills directory")
	}
	if _, err := ReadSkills([]string{""}, false); err == nil || err.Error() != "--skills needs a directory" {
		t.Fatalf("ReadSkills(\"\") = %v, want --skills needs a directory", err)
	}
}

// A skill in ~/.claude/skills is often a link into a checkout; it is read
// through the link, and so is a linked file in it. A linked directory inside a
// skill is not followed, so a link cannot loop.
func TestReadSkillsFollowsLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir, checkout := t.TempDir(), t.TempDir()
	writeSkillsTestFile(t, filepath.Join(checkout, "SKILL.md"), "# linked", 0o644)
	writeSkillsTestFile(t, filepath.Join(checkout, "real.md"), "real", 0o644)
	if err := os.Symlink("real.md", filepath.Join(checkout, "alias.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".", filepath.Join(checkout, "loop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(checkout, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}

	skills, err := ReadSkills([]string{dir}, false)
	if err != nil {
		t.Fatalf("ReadSkills() = %v", err)
	}
	want := sandboxconfig.Skills{"linked": {Skill: "# linked", Files: []sandboxconfig.SkillFile{
		{Path: "alias.md", Content: []byte("real")},
		{Path: "real.md", Content: []byte("real")},
	}}}
	if !reflect.DeepEqual(skills, want) {
		t.Fatalf("ReadSkills() = %#v, want %#v", skills, want)
	}
}

// Many skills each under the limit stop at the one that takes the whole past
// it, before the rest are read, and the refusal says what to leave out.
func TestReadSkillsNamesTheLargestWhenOverTheLimit(t *testing.T) {
	dir := t.TempDir()
	writeSkillsTestFile(t, filepath.Join(dir, "big", "SKILL.md"), "# big", 0o644)
	writeSkillsTestFile(t, filepath.Join(dir, "big", "blob.bin"), strings.Repeat("x", sandboxconfig.MaxSkillsBytes*3/5), 0o644)
	writeSkillsTestFile(t, filepath.Join(dir, "bigger", "SKILL.md"), "# bigger", 0o644)
	writeSkillsTestFile(t, filepath.Join(dir, "bigger", "blob.bin"), strings.Repeat("x", sandboxconfig.MaxSkillsBytes*4/5), 0o644)
	writeSkillsTestFile(t, filepath.Join(dir, "small", "SKILL.md"), "# small", 0o644)

	_, err := ReadSkills([]string{dir}, false)
	if err == nil || !strings.Contains(err.Error(), "skill bigger: it takes the skills past") || !strings.Contains(err.Error(), "the largest read before it are big (") {
		t.Fatalf("ReadSkills() = %v, want bigger refused, naming big as the largest read before it", err)
	}
}

// A SKILL.md past the limit is refused before it is read, and a skill
// directory that cannot be read is an error, not a skill silently left out.
func TestReadSkillsRefusesWhatItCannotOrShouldNotRead(t *testing.T) {
	dir := t.TempDir()
	writeSkillsTestFile(t, filepath.Join(dir, "huge", "SKILL.md"), strings.Repeat("x", sandboxconfig.MaxSkillsBytes+1), 0o644)
	if _, err := ReadSkills([]string{dir}, false); err == nil || !strings.Contains(err.Error(), "skill huge: its SKILL.md") {
		t.Fatalf("ReadSkills() = %v, want huge's SKILL.md refused", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	// An entry that cannot be stat'ed for a reason other than being absent: a
	// link to itself fails with a loop, where a dangling one would be skipped.
	dir = t.TempDir()
	if err := os.Symlink("looped", filepath.Join(dir, "looped")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSkills([]string{dir}, false); err == nil {
		t.Fatal("ReadSkills() = nil, want an entry that cannot be stat'ed to be an error")
	}
	if err := os.Remove(filepath.Join(dir, "looped")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(dir, "dangling")); err != nil {
		t.Fatal(err)
	}
	if skills, err := ReadSkills([]string{dir}, false); err != nil || skills != nil {
		t.Fatalf("ReadSkills() with a dangling link = %v, %v; want it skipped", skills, err)
	}
}

// One skill past the limit is refused at the file that takes it there, before
// the rest is read: a link to a whole checkout is one skill.
func TestReadSkillsStopsAtTheFileThatPassesTheLimit(t *testing.T) {
	dir := t.TempDir()
	writeSkillsTestFile(t, filepath.Join(dir, "huge", "SKILL.md"), "# huge", 0o644)
	writeSkillsTestFile(t, filepath.Join(dir, "huge", "a.bin"), strings.Repeat("x", sandboxconfig.MaxSkillsBytes), 0o644)
	writeSkillsTestFile(t, filepath.Join(dir, "huge", "b.bin"), "x", 0o644)

	_, err := ReadSkills([]string{dir}, false)
	if err == nil || !strings.Contains(err.Error(), "skill huge") || !strings.Contains(err.Error(), "a.bin") {
		t.Fatalf("ReadSkills() = %v, want huge refused at a.bin", err)
	}
}

func TestSetCreateSandboxSkillsCarriesContentAndTheExecutableBit(t *testing.T) {
	var config apiclientgen.SandboxCreateConfig
	SetCreateSandboxSkills(&config, nil)
	if config.Skills.Set {
		t.Fatal("skills set with none to carry")
	}
	SetCreateSandboxSkills(&config, sandboxconfig.Skills{"foo": {Skill: "# foo", Files: []sandboxconfig.SkillFile{
		{Path: "run.sh", Content: []byte("x"), Executable: true},
		{Path: "a.md", Content: []byte("y")},
	}}})
	got := config.Skills.Or(nil)["foo"]
	if got.Skill != "# foo" || len(got.Files) != 2 {
		t.Fatalf("skill = %#v", got)
	}
	if !got.Files[0].Executable.Or(false) || got.Files[1].Executable.Set {
		t.Fatalf("executable = %v, %v; want only run.sh's set", got.Files[0].Executable, got.Files[1].Executable)
	}
}

// The file limit holds across skills: once the skills read so far hold every
// file a discobox takes, the next is refused before its SKILL.md is read.
func TestReadSkillsStopsAtTheFileLimitAcrossSkills(t *testing.T) {
	dir := t.TempDir()
	// One skill of exactly the limit: its SKILL.md and MaxSkillFiles-1 files.
	for i := range sandboxconfig.MaxSkillFiles - 1 {
		writeSkillsTestFile(t, filepath.Join(dir, "a-full", fmt.Sprintf("f%d", i)), "", 0o644)
	}
	writeSkillsTestFile(t, filepath.Join(dir, "a-full", "SKILL.md"), "# full", 0o644)
	if skills, err := ReadSkills([]string{dir}, false); err != nil || len(skills["a-full"].Files) != sandboxconfig.MaxSkillFiles-1 {
		t.Fatalf("ReadSkills() at the limit = %v; want it taken", err)
	}
	// A directory that is not a skill is skipped at the limit as anywhere.
	writeSkillsTestFile(t, filepath.Join(dir, "b-notes", "README.md"), "notes", 0o644)
	if _, err := ReadSkills([]string{dir}, false); err != nil {
		t.Fatalf("ReadSkills() with a directory that is not a skill = %v; want it skipped", err)
	}
	writeSkillsTestFile(t, filepath.Join(dir, "c-more", "SKILL.md"), "# more", 0o644)
	_, err := ReadSkills([]string{dir}, false)
	if err == nil || !strings.Contains(err.Error(), "skill c-more: it takes the skills past the 1000 files") || !strings.Contains(err.Error(), "a-full (") {
		t.Fatalf("ReadSkills() = %v, want c-more refused naming a-full", err)
	}
}
