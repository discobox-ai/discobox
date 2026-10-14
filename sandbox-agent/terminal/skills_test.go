package terminal

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandboxconfig"
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

// installDeclaredSkills is the pair installSkills runs over each tree: read
// what is declared, then copy it. Home is not resolved when there is nothing to
// copy, which is what keeps a launch working without one.
func installDeclaredSkills(source, home string) error {
	return installTree(filepath.Join(source, ProjectSkillsDir), home)
}

// installTree is the same pair over any of the trees the install reads: the
// image's built-in skills, or a repository's declared ones.
func installTree(from, home string) error {
	entries, err := declaredSkills(from)
	if err != nil {
		return err
	}
	return copySkills(from, entries, home, nil)
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

// The skills for what the image installs reach every sandbox, whatever it was
// made from. A repository does not declare `discobox-access`; it is in the
// sandbox regardless, and an agent that has never been told the credential
// protocol exists writes a token into a config file instead of asking for one.
func TestBuiltinSkillsAreInstalledForEverySandbox(t *testing.T) {
	image, home := t.TempDir(), t.TempDir()
	writeTreeFile(t, image, filepath.Join("discobox-access", "SKILL.md"), "ask a human\n", 0o644)

	if err := installTree(image, home); err != nil {
		t.Fatalf("install builtin skills: %v", err)
	}
	for _, dir := range skillDirectories {
		got := readFile(t, filepath.Join(home, dir, "discobox-access", "SKILL.md"))
		if got != "ask a human\n" {
			t.Fatalf("%s/discobox-access/SKILL.md = %q", dir, got)
		}
	}
}

// An image built before the built-in skills existed has no such directory.
// That is an older image rather than a misconfiguration, and failing the
// primary launch over it would take away the sandbox to protect the
// documentation.
func TestAnImageWithNoBuiltinSkillsInstallsNothing(t *testing.T) {
	home := t.TempDir()

	if err := installTree(filepath.Join(t.TempDir(), "absent"), home); err != nil {
		t.Fatalf("install with no builtin directory: %v", err)
	}
	for _, dir := range skillDirectories {
		if _, err := os.Stat(filepath.Join(home, dir)); !os.IsNotExist(err) {
			t.Fatalf("stat %s = %v, want no directory created", dir, err)
		}
	}
}

// The repository is copied second, so a repository that declares a skill by the
// same name replaces the image's: it is the more specific declaration. What it
// does not name is left alone, since neither owns the directory.
func TestARepositorySkillWinsOverTheImages(t *testing.T) {
	image, source, home := t.TempDir(), t.TempDir(), t.TempDir()
	writeTreeFile(t, image, filepath.Join("discobox-access", "SKILL.md"), "the image's\n", 0o644)
	writeTreeFile(t, image, filepath.Join("other", "SKILL.md"), "untouched\n", 0o644)
	writeSkill(t, source, filepath.Join("discobox-access", "SKILL.md"), "the repository's\n", 0o644)

	if err := installTree(image, home); err != nil {
		t.Fatalf("install builtin skills: %v", err)
	}
	if err := installDeclaredSkills(source, home); err != nil {
		t.Fatalf("install declared skills: %v", err)
	}
	for _, dir := range skillDirectories {
		root := filepath.Join(home, dir)
		if got := readFile(t, filepath.Join(root, "discobox-access", "SKILL.md")); got != "the repository's\n" {
			t.Fatalf("%s: discobox-access = %q, want the repository's", dir, got)
		}
		if got := readFile(t, filepath.Join(root, "other", "SKILL.md")); got != "untouched\n" {
			t.Fatalf("%s: other = %q, want the image's left alone", dir, got)
		}
	}
}

// writeTreeFile writes one file under a skills tree that is not a repository's
// — the image's, in these tests.
func writeTreeFile(t *testing.T, root, rel, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// The sandbox's own skills are written last, from content, so they win over the
// repository's on a name they share, and leave its other skills alone
// (ADR 26-10-09-395 §3).
func TestTheSandboxsSkillsWinOverTheRepositorys(t *testing.T) {
	source, home := t.TempDir(), t.TempDir()
	writeSkill(t, source, filepath.Join("review", "SKILL.md"), "the repository's\n", 0o644)
	writeSkill(t, source, filepath.Join("review", "old.md"), "kept\n", 0o644)
	writeSkill(t, source, filepath.Join("other", "SKILL.md"), "untouched\n", 0o644)
	skills := sandboxconfig.Skills{"review": {Skill: "the sandbox's\n", Files: []sandboxconfig.SkillFile{
		{Path: "bin/run.sh", Content: []byte("#!/bin/sh\n"), Executable: true},
		{Path: "logo.png", Content: []byte{0x89, 0}},
	}}}

	// The repository's, then the sandbox's, in installSkills' order.
	if err := installDeclaredSkills(source, home); err != nil {
		t.Fatalf("install declared skills: %v", err)
	}
	if err := writeSkills(skills, home, nil); err != nil {
		t.Fatalf("install the sandbox's skills: %v", err)
	}
	for _, dir := range skillDirectories {
		root := filepath.Join(home, dir)
		if got := readFile(t, filepath.Join(root, "review", "SKILL.md")); got != "the sandbox's\n" {
			t.Fatalf("%s: review = %q, want the sandbox's", dir, got)
		}
		if got := readFile(t, filepath.Join(root, "review", "old.md")); got != "kept\n" {
			t.Fatalf("%s: review/old.md = %q, want the repository's left in place", dir, got)
		}
		if got := readFile(t, filepath.Join(root, "other", "SKILL.md")); got != "untouched\n" {
			t.Fatalf("%s: other = %q, want the repository's left alone", dir, got)
		}
		if got := readFile(t, filepath.Join(root, "review", "logo.png")); got != "\x89\x00" {
			t.Fatalf("%s: logo.png = %q, want its bytes", dir, got)
		}
		if runtime.GOOS == "windows" {
			continue
		}
		info, err := os.Stat(filepath.Join(root, "review", "bin", "run.sh"))
		if err != nil {
			t.Fatalf("%s: stat run.sh: %v", dir, err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("%s: run.sh mode = %v, want 0755", dir, info.Mode().Perm())
		}
		info, err = os.Stat(filepath.Join(root, "review", "logo.png"))
		if err != nil {
			t.Fatalf("%s: stat logo.png: %v", dir, err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("%s: logo.png mode = %v, want 0644", dir, info.Mode().Perm())
		}
	}
}

// A sandbox whose only skills are its own still has them installed.
func TestTheSandboxsSkillsAreInstalledWithNothingElseToCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		// As TestPrimaryLaunchInstallsProjectSkillsOnce: the service resolves
		// the source as a guest path, which a C:\ temp directory is not.
		t.Skip("guest path resolution")
	}
	source, home := t.TempDir(), t.TempDir()
	svc := newSkillsTestService(t, source, home, nil)
	svc.skills = sandboxconfig.Skills{"mine": {Skill: "# mine\n"}}

	if err := svc.installSkills(); err != nil {
		t.Fatalf("install skills: %v", err)
	}
	for _, dir := range skillDirectories {
		if got := readFile(t, filepath.Join(home, dir, "mine", "SKILL.md")); got != "# mine\n" {
			t.Fatalf("%s: mine = %q", dir, got)
		}
	}
}

// A skill that would write outside its directory is refused before anything is
// written under home.
func TestASkillLeavingItsDirectoryIsRefusedAndWritesNothing(t *testing.T) {
	source, home := t.TempDir(), t.TempDir()
	writeSkill(t, source, filepath.Join("review", "SKILL.md"), "# review\n", 0o644)
	svc := newSkillsTestService(t, source, home, nil)
	svc.skills = sandboxconfig.Skills{"evil": {Skill: "x", Files: []sandboxconfig.SkillFile{{Path: "../../.bashrc", Content: []byte("x")}}}}

	if err := svc.installSkills(); err == nil {
		t.Fatal("install skills: nil, want a refusal")
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("home holds %v, want nothing written", entries)
	}
}

// A skills source's skills are links into its checkout, from the directory
// it names: an edit through one lands in the checkout, where it can be
// committed, and nothing but a directory holding a SKILL.md is linked.
func TestASkillsSourcesSkillsAreLinkedIntoItsCheckout(t *testing.T) {
	checkout, home := t.TempDir(), t.TempDir()
	writeTreeFile(t, checkout, "agents/skills/deploy/SKILL.md", "# deploy\n", 0o644)
	writeTreeFile(t, checkout, "agents/skills/notes/README.md", "not a skill\n", 0o644)
	writeTreeFile(t, checkout, "agents/skills/.hidden/SKILL.md", "hidden\n", 0o644)
	writeTreeFile(t, checkout, "deploy/SKILL.md", "outside the directory\n", 0o644)

	sources := []sandboxconfig.SkillsSource{{Target: checkout, Directory: "agents/skills"}}
	if err := linkSkillsSources(sources, home, nil); err != nil {
		t.Fatalf("link skills sources: %v", err)
	}
	for _, dir := range skillDirectories {
		root := filepath.Join(home, dir)
		if got := readFile(t, filepath.Join(root, "deploy", "SKILL.md")); got != "# deploy\n" {
			t.Fatalf("%s: deploy = %q", dir, got)
		}
		for _, name := range []string{"notes", ".hidden"} {
			if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
				t.Fatalf("%s: %s linked (%v), want it left out", dir, name, err)
			}
		}
	}
	edited := filepath.Join(home, skillDirectories[0], "deploy", "SKILL.md")
	if err := os.WriteFile(edited, []byte("# deploy, improved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(checkout, "agents", "skills", "deploy", "SKILL.md")); got != "# deploy, improved\n" {
		t.Fatalf("checkout's deploy = %q, want the edit made through the link", got)
	}
}

// A linked skill replaces a copied one of its name whole, and a later skills
// directory replaces an earlier one's link without touching that checkout.
func TestALaterSkillsSourceWinsAndALinkReplacesACopy(t *testing.T) {
	first, second, home := t.TempDir(), t.TempDir(), t.TempDir()
	writeTreeFile(t, first, "review/SKILL.md", "first\n", 0o644)
	writeTreeFile(t, first, "lint/SKILL.md", "first's lint\n", 0o644)
	writeTreeFile(t, second, "skills/review/SKILL.md", "second\n", 0o644)
	copied := sandboxconfig.Skills{"lint": {Skill: "copied\n", Files: []sandboxconfig.SkillFile{{Path: "extra.md", Content: []byte("x")}}}}

	if err := writeSkills(copied, home, nil); err != nil {
		t.Fatalf("write skills: %v", err)
	}
	sources := []sandboxconfig.SkillsSource{
		{Target: first, Directory: sandboxconfig.RootSkillsDirectory},
		{Target: second, Directory: "skills"},
	}
	if err := linkSkillsSources(sources, home, nil); err != nil {
		t.Fatalf("link skills sources: %v", err)
	}
	for _, dir := range skillDirectories {
		root := filepath.Join(home, dir)
		if got := readFile(t, filepath.Join(root, "review", "SKILL.md")); got != "second\n" {
			t.Fatalf("%s: review = %q, want the later source's", dir, got)
		}
		if got := readFile(t, filepath.Join(root, "lint", "SKILL.md")); got != "first's lint\n" {
			t.Fatalf("%s: lint = %q, want the link's", dir, got)
		}
		if _, err := os.Stat(filepath.Join(root, "lint", "extra.md")); !os.IsNotExist(err) {
			t.Fatalf("%s: the copy's extra.md survived (%v), want the copy replaced whole", dir, err)
		}
	}
	if got := readFile(t, filepath.Join(first, "review", "SKILL.md")); got != "first\n" {
		t.Fatalf("first checkout's review = %q, want it untouched by being replaced", got)
	}
}

// A directory the checkout does not hold fails the install, naming it.
func TestASkillsDirectoryTheCheckoutLacksFailsTheInstall(t *testing.T) {
	checkout, home := t.TempDir(), t.TempDir()
	sources := []sandboxconfig.SkillsSource{{Target: checkout, Directory: "agents/skills"}}
	err := linkSkillsSources(sources, home, nil)
	if err == nil || !strings.Contains(err.Error(), "agents/skills") {
		t.Fatalf("link skills sources = %v, want a failure naming the directory", err)
	}
}

// A sandbox whose only skills come from a skills source still has them
// installed on its first launch.
func TestASkillsSourceIsInstalledWithNothingElseToCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("guest path resolution")
	}
	source, checkout, home := t.TempDir(), t.TempDir(), t.TempDir()
	writeTreeFile(t, checkout, "mine/SKILL.md", "# mine\n", 0o644)
	svc := newSkillsTestService(t, source, home, nil)
	svc.skillsSources = []sandboxconfig.SkillsSource{{Target: checkout, Directory: sandboxconfig.RootSkillsDirectory}}

	if err := svc.installSkills(); err != nil {
		t.Fatalf("install skills: %v", err)
	}
	for _, dir := range skillDirectories {
		if got := readFile(t, filepath.Join(home, dir, "mine", "SKILL.md")); got != "# mine\n" {
			t.Fatalf("%s: mine = %q", dir, got)
		}
	}
}

// The install runs again when a first launch fails after it. The second run
// copies over the first run's links, and must not write the copy through them
// into the skills source's checkout.
func TestASecondInstallLeavesTheSkillsSourcesCheckoutAlone(t *testing.T) {
	source, checkout, home := t.TempDir(), t.TempDir(), t.TempDir()
	writeSkill(t, source, filepath.Join("review", "SKILL.md"), "the repository's\n", 0o644)
	writeTreeFile(t, checkout, "review/SKILL.md", "the skills source's\n", 0o644)
	skills := sandboxconfig.Skills{"review": {Skill: "the sandbox's\n"}}
	sources := []sandboxconfig.SkillsSource{{Target: checkout, Directory: sandboxconfig.RootSkillsDirectory}}

	for run := 0; run < 2; run++ {
		if err := installDeclaredSkills(source, home); err != nil {
			t.Fatalf("run %d: install declared skills: %v", run, err)
		}
		if err := writeSkills(skills, home, nil); err != nil {
			t.Fatalf("run %d: write skills: %v", run, err)
		}
		if err := linkSkillsSources(sources, home, nil); err != nil {
			t.Fatalf("run %d: link skills sources: %v", run, err)
		}
	}
	if got := readFile(t, filepath.Join(checkout, "review", "SKILL.md")); got != "the skills source's\n" {
		t.Fatalf("checkout's review = %q, want it untouched by the second run", got)
	}
	for _, dir := range skillDirectories {
		if got := readFile(t, filepath.Join(home, dir, "review", "SKILL.md")); got != "the skills source's\n" {
			t.Fatalf("%s: review = %q, want the link's", dir, got)
		}
	}
}
