package terminal

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/x/shorttmp"
)

// newSkillsTestService builds a service whose default workdir is a repository
// checkout and whose run user's home is a directory the test can read, which is
// the whole of what the skills install depends on.
func newSkillsTestService(t *testing.T, source, home string, state PrimaryStateStore) *Service {
	t.Helper()
	dir := shorttmp.Dir(t)
	units := &shimUnits{}
	t.Cleanup(units.Close)
	env := map[string]string{"PATH": "/usr/bin"}
	execManager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot:    dir,
		DefaultWorkdir: source,
		RuntimeDir:     filepath.Join(dir, "rt"),
		Env:            env,
		Units:          units,
	})
	if err != nil {
		t.Fatalf("new exec manager: %v", err)
	}
	svc, err := NewService(ServiceConfig{
		Execs:        execManager,
		WorkingRoot:  dir,
		RuntimeDir:   filepath.Join(dir, "rt"),
		Env:          env,
		Harness:      config.Harness{ID: "codex", Command: []string{"codex"}},
		Units:        units,
		Installer:    &noopInstaller{},
		PrimaryState: state,
		ExecDefaults: config.ExecDefaults{HomeDirectory: home},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// writeSkill writes one file under the repository's declared skills directory.
func writeSkill(t *testing.T, source, rel, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(source, ProjectSkillsDir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// A repository's skills reach every skill directory a harness might read, with
// their tree and their executable bits intact.
func TestInstallSkillsCopiesTheTreeIntoEverySkillDirectory(t *testing.T) {
	source, home := t.TempDir(), t.TempDir()
	writeSkill(t, source, filepath.Join("review", "SKILL.md"), "# review\n", 0o644)
	writeSkill(t, source, filepath.Join("review", "bin", "run.sh"), "#!/bin/sh\n", 0o755)
	writeSkill(t, source, "loose.md", "loose\n", 0o644)

	if err := installDeclaredSkills(source, home); err != nil {
		t.Fatalf("install skills: %v", err)
	}

	for _, dir := range skillDirectories {
		root := filepath.Join(home, dir)
		if got := readFile(t, filepath.Join(root, "review", "SKILL.md")); got != "# review\n" {
			t.Fatalf("%s: SKILL.md = %q", dir, got)
		}
		if got := readFile(t, filepath.Join(root, "loose.md")); got != "loose\n" {
			t.Fatalf("%s: loose.md = %q", dir, got)
		}
		info, err := os.Stat(filepath.Join(root, "review", "bin", "run.sh"))
		if err != nil {
			t.Fatalf("%s: stat run.sh: %v", dir, err)
		}
		if runtime.GOOS == "windows" {
			// Windows carries no executable bit to carry over.
			continue
		}
		if info.Mode()&0o111 == 0 {
			t.Fatalf("%s: run.sh mode = %v, want the executable bit carried over", dir, info.Mode())
		}
	}
}

// The repository is the more specific declaration, so its skill wins over one
// of the same name already in the directory — but it does not own the
// directory, and everything else there survives.
func TestInstallSkillsOverwritesByNameAndLeavesTheRestAlone(t *testing.T) {
	source, home := t.TempDir(), t.TempDir()
	existing := filepath.Join(home, skillDirectories[0])
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(existing, "review.md"), []byte("image\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(existing, "other.md"), []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeSkill(t, source, "review.md", "repository\n", 0o644)

	if err := installDeclaredSkills(source, home); err != nil {
		t.Fatalf("install skills: %v", err)
	}

	if got := readFile(t, filepath.Join(existing, "review.md")); got != "repository\n" {
		t.Fatalf("review.md = %q, want the repository's copy", got)
	}
	if got := readFile(t, filepath.Join(existing, "other.md")); got != "keep\n" {
		t.Fatalf("other.md = %q, want the directory's other files untouched", got)
	}
}

// A link resolved from a home directory does not point where it did in the
// checkout, so it is not copied at all rather than copied as a dangling one.
func TestInstallSkillsSkipsSymlinks(t *testing.T) {
	source, home := t.TempDir(), t.TempDir()
	writeSkill(t, source, "real.md", "real\n", 0o644)
	link := filepath.Join(source, ProjectSkillsDir, "link.md")
	if err := os.Symlink("../../elsewhere.md", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := installDeclaredSkills(source, home); err != nil {
		t.Fatalf("install skills: %v", err)
	}

	root := filepath.Join(home, skillDirectories[0])
	if _, err := os.Lstat(filepath.Join(root, "link.md")); !os.IsNotExist(err) {
		t.Fatalf("lstat link.md = %v, want it not copied", err)
	}
	if got := readFile(t, filepath.Join(root, "real.md")); got != "real\n" {
		t.Fatalf("real.md = %q", got)
	}
}

// installDeclaredSkills is the pair installProjectSkills runs: read what the
// repository declares, then copy it. Home is not resolved when there is nothing
// to copy, which is what keeps a launch working without one.
func installDeclaredSkills(source, home string) error {
	declared := filepath.Join(source, ProjectSkillsDir)
	entries, err := declaredSkills(declared)
	if err != nil {
		return err
	}
	return installSkills(declared, entries, home, nil)
}

// Almost every repository declares no skills at all, and the ones that create
// the directory and leave it empty must not have a skills directory made for
// them in home either.
func TestInstallSkillsIsANoOpWithoutSkills(t *testing.T) {
	source, home := t.TempDir(), t.TempDir()

	if err := installDeclaredSkills(source, home); err != nil {
		t.Fatalf("install skills with no directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(source, ProjectSkillsDir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := installDeclaredSkills(source, home); err != nil {
		t.Fatalf("install skills with an empty directory: %v", err)
	}

	for _, dir := range skillDirectories {
		if _, err := os.Stat(filepath.Join(home, dir)); !os.IsNotExist(err) {
			t.Fatalf("stat %s = %v, want no directory created", dir, err)
		}
	}
}

// The install is the primary terminal's first launch and nothing else: it reads
// the primary source, which only the primary terminal's launch is sequenced
// behind (source delivery), and the copies belong to the harness afterwards.
func TestPrimaryLaunchInstallsProjectSkillsOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The exec manager resolves the workdir it starts execs in as a guest
		// path — Linux, whatever the host is — so it reads a C:\ source as a
		// relative one and joins it onto the working root. Only the install
		// itself is under test here, and it is reached through that resolution.
		t.Skip("guest path resolution")
	}
	source, home := t.TempDir(), t.TempDir()
	writeSkill(t, source, filepath.Join("review", "SKILL.md"), "# review\n", 0o644)
	state := &countingPrimaryState{}
	svc := newSkillsTestService(t, source, home, state)

	if err := svc.EnsurePrimary(t.Context(), nil); err != nil {
		t.Fatalf("ensure primary: %v", err)
	}
	installed := filepath.Join(home, skillDirectories[0], "review", "SKILL.md")
	if got := readFile(t, installed); got != "# review\n" {
		t.Fatalf("installed skill = %q", got)
	}

	// A sandbox that has launched before: the harness has had the files, and a
	// second launch must not put back what it removed.
	if err := os.RemoveAll(filepath.Join(home, skillDirectories[0])); err != nil {
		t.Fatalf("remove: %v", err)
	}
	relaunched := newSkillsTestService(t, source, home, state)
	if err := relaunched.EnsurePrimary(t.Context(), nil); err != nil {
		t.Fatalf("ensure primary again: %v", err)
	}
	if _, err := os.Stat(installed); !os.IsNotExist(err) {
		t.Fatalf("stat %s = %v, want no reinstall on a later launch", installed, err)
	}
}
