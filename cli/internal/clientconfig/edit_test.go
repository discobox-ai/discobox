package clientconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddSkillEditsTheFileInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.yaml")
	t.Setenv(FileVar, path)
	original := `# my settings
new:
  # skills I always want
  skills:
    - ./team # relative to here
  userSkills: true
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(dir, "extra")
	if _, err := AddSkill(extra); err != nil {
		t.Fatalf("add: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"# my settings", "# skills I always want", "- ./team # relative to here", "- " + extra, "userSkills: true"} {
		if !strings.Contains(got, want) {
			t.Fatalf("edited file lacks %q:\n%s", want, got)
		}
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v; want 0600 kept", info.Mode(), err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.New.Skills) != 2 || cfg.New.Skills[0] != filepath.Join(dir, "team") || cfg.New.Skills[1] != extra {
		t.Fatalf("skills = %q", cfg.New.Skills)
	}

	// The relative entry is the same directory, as a create reads it.
	if _, err := AddSkill(filepath.Join(dir, "team")); err == nil {
		t.Fatal("adding a directory already named was not refused")
	}
	if _, err := RemoveSkill(filepath.Join(dir, "team")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := RemoveSkill(filepath.Join(dir, "team")); err == nil {
		t.Fatal("removing a directory not named was not refused")
	}
	if _, err := RemoveSkill(extra); err != nil {
		t.Fatalf("remove: %v", err)
	}
	cfg, err = Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.New.Skills) != 0 || !cfg.New.UserSkills {
		t.Fatalf("config = %+v", cfg.New)
	}
}

func TestAddSkillCreatesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discobox", "client.yaml")
	t.Setenv(FileVar, path)
	// Load refuses a named path that is not there; the edit is what makes it.
	if _, err := AddSkill("/srv/skills"); err != nil {
		t.Fatalf("add: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.New.Skills) != 1 || cfg.New.Skills[0] != filepath.Clean("/srv/skills") {
		t.Fatalf("skills = %q", cfg.New.Skills)
	}
}

func TestAddSkillRefusesWhatItCannotWrite(t *testing.T) {
	t.Setenv(FileVar, "")
	if _, err := AddSkill("/srv/skills"); !errors.Is(err, ErrNoFile) {
		t.Fatalf("err = %v, want ErrNoFile", err)
	}
	path := filepath.Join(t.TempDir(), "client.yaml")
	t.Setenv(FileVar, path)
	if err := os.WriteFile(path, []byte("new:\n  skils: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AddSkill("/srv/skills"); err == nil {
		t.Fatal("a file that does not parse was edited")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "new:\n  skils: []\n" {
		t.Fatalf("file changed: %q", data)
	}
}

// A file of comments alone, the reference copied into place, keeps them.
func TestAddSkillKeepsAFileOfCommentsAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	t.Setenv(FileVar, path)
	example, err := ExampleYAML()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, example, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AddSkill("/srv/skills"); err != nil {
		t.Fatalf("add: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), string(example)) {
		t.Fatalf("the reference's comments did not survive the edit:\n%s", data)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.New.Skills) != 1 || cfg.New.Skills[0] != filepath.Clean("/srv/skills") {
		t.Fatalf("skills = %q", cfg.New.Skills)
	}
}
