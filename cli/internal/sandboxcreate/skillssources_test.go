package sandboxcreate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/platform"
)

// A --skills value is a skills source when what precedes its //PATH is a Git
// URL, and a directory otherwise; the path starts at the first // after the
// scheme, so it may hold @ and :.
func TestSplitSkillsReadsEachValueByItsForm(t *testing.T) {
	cases := []struct {
		value     string
		repo      bool
		source    string
		directory string
	}{
		{value: "./team-skills"},
		{value: "/home/me/skills"},
		{value: "https://github.com/acme/tools", repo: true, source: "https://github.com/acme/tools", directory: "."},
		{value: "https://github.com/acme/tools@main//agents/skills", repo: true, source: "https://github.com/acme/tools@main", directory: "agents/skills"},
		{value: "git@github.com:acme/tools//skills", repo: true, source: "git@github.com:acme/tools", directory: "skills"},
		{value: "git@github.com:acme/tools@v1//odd@name:dir", repo: true, source: "git@github.com:acme/tools@v1", directory: "odd@name:dir"},
		{value: "ssh://git@example.com:2222/acme/tools.git//skills", repo: true, source: "ssh://git@example.com:2222/acme/tools.git", directory: "skills"},
		// GitHub shorthand, when no such directory is here (ADR 26-10-15-664).
		{value: "acme/tools@main//skills", repo: true, source: "https://github.com/acme/tools.git@main", directory: "skills"},
		{value: "github.com/acme/tools", repo: true, source: "https://github.com/acme/tools.git", directory: "."},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			got, err := SplitSkills([]string{tc.value})
			if err != nil {
				t.Fatalf("split skills: %v", err)
			}
			if !tc.repo {
				if !reflect.DeepEqual(got.Directories, []string{tc.value}) || len(got.Repositories) != 0 {
					t.Fatalf("split = %+v, want the one directory", got)
				}
				return
			}
			if len(got.Directories) != 0 || len(got.Repositories) != 1 {
				t.Fatalf("split = %+v, want one repository", got)
			}
			repo := got.Repositories[0]
			if repo.Arg != tc.value || repo.Source.String() != tc.source || repo.Directory != tc.directory {
				t.Fatalf("split = %+v, want %s in %s", repo, tc.source, tc.directory)
			}
		})
	}
}

// A path that leaves the repository, or names nothing, is refused.
func TestSplitSkillsRefusesAPathOutsideTheRepository(t *testing.T) {
	for _, value := range []string{
		"https://github.com/acme/tools//../etc",
		"https://github.com/acme/tools//",
		"https://github.com/acme/tools///abs",
		"https://github.com/acme/tools//a/./b",
	} {
		if _, err := SplitSkills([]string{value}); err == nil {
			t.Fatalf("split %s: nil, want a refusal", value)
		}
	}
}

// A directory's skills are read as they are, so a ref or a path on one is
// refused rather than ignored; shorthand that is a directory here is the
// directory.
func TestSplitSkillsReadsADirectoryAsItIs(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join("acme", "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := SplitSkills([]string{"acme/tools"})
	if err != nil || !reflect.DeepEqual(got.Directories, []string{"acme/tools"}) {
		t.Fatalf("split = %+v, %v; want the directory that is there", got, err)
	}
	for _, value := range []string{"acme/tools@main", "./skills//sub"} {
		if _, err := SplitSkills([]string{value}); err == nil || !strings.Contains(err.Error(), "directory") {
			t.Fatalf("split %s = %v, want a refusal of a ref or path on a directory", value, err)
		}
	}
}

// Each skills repository is one reference marked for its skills, checked out
// below home, and the directories are filed in the order given, one
// repository's sharing its reference however they interleave with another's.
func TestBuildPromptSandboxBodyFilesSkillsRepositoriesInOrder(t *testing.T) {
	tools := "file://" + filepath.ToSlash(newRunSourceTestRepoIn(t, t.TempDir(), "tools"))
	shared := "file://" + filepath.ToSlash(newRunSourceTestRepoIn(t, t.TempDir(), "shared"))

	body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Platform:     linuxSandbox,
		NoSource:     true,
		IncludeDirty: IncludeDirtyNever,
		SkillDirs:    []string{tools + "//agents/skills", shared, tools + "//extra"},
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}
	defer local.Close()

	references, _ := body.Config.SourceCodeReferences.Get()
	toolsKey, sharedKey := "~/.discobox/skill-sources/tools", "~/.discobox/skill-sources/shared"
	if len(references) != 2 {
		t.Fatalf("references = %v, want one per repository", references)
	}
	for _, key := range []string{toolsKey, sharedKey} {
		reference, ok := references[key]
		if !ok {
			t.Fatalf("references are keyed %v, want %s", references, key)
		}
		if purpose := reference.Purpose.Or(""); purpose != apiclientgen.GitSourcePurposeSkills {
			t.Fatalf("%s: purpose = %q, want skills", key, purpose)
		}
		if checkout, ok := reference.Checkout.Get(); !ok || checkout.Commit.Or("") == "" {
			t.Fatalf("%s: checkout = %v, want the commit resolved at create", key, checkout)
		}
	}
	want := []apimodel.SandboxSkillsSource{
		{Source: toolsKey, Directory: "agents/skills"},
		{Source: sharedKey, Directory: "."},
		{Source: toolsKey, Directory: "extra"},
	}
	if !reflect.DeepEqual(body.Config.SkillsSources, want) {
		t.Fatalf("skills sources = %+v, want %+v", body.Config.SkillsSources, want)
	}
	if _, ok := body.Config.Skills.Get(); ok {
		t.Fatal("a skills repository was read as content")
	}
}

// A skills source is keyed below home with slashes on every platform: the
// sandbox expands it by its own rules (ADR 26-11-01-692 §1), so a Windows
// sandbox's key is no different from a Linux one's, and it is never under the
// working root a sandbox with no source works in.
func TestBuildPromptSandboxBodyKeysSkillsBelowHomeOnEveryPlatform(t *testing.T) {
	tools := "file://" + filepath.ToSlash(newRunSourceTestRepoIn(t, t.TempDir(), "tools"))
	for _, sandbox := range []platform.Platform{linuxSandbox, {OS: "windows", Arch: "amd64"}, {OS: "darwin", Arch: "arm64"}} {
		body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
			Platform:     sandbox,
			NoSource:     true,
			IncludeDirty: IncludeDirtyNever,
			SkillDirs:    []string{tools},
		})
		if err != nil {
			t.Fatalf("%s: build prompt sandbox body: %v", sandbox.OS, err)
		}
		local.Close()
		references, _ := body.Config.SourceCodeReferences.Get()
		reference, ok := references["~/.discobox/skill-sources/tools"]
		if !ok || len(references) != 1 {
			t.Fatalf("%s: references = %v, want one keyed ~/.discobox/skill-sources/tools", sandbox.OS, references)
		}
		destination, _ := reference.Destination.Get()
		if got := destination.Directory.Or(""); got != "~/.discobox/skill-sources/tools" {
			t.Fatalf("%s: destination directory = %q, want the key", sandbox.OS, got)
		}
	}
}

// One repository at two refs would need two checkouts of one name.
func TestBuildPromptSandboxBodyRefusesOneSkillsRepositoryAtTwoRefs(t *testing.T) {
	tools := "file://" + filepath.ToSlash(newRunSourceTestRepoIn(t, t.TempDir(), "tools"))
	_, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Platform:     linuxSandbox,
		NoSource:     true,
		IncludeDirty: IncludeDirtyNever,
		SkillDirs:    []string{tools + "@main//a", tools + "//b"},
	})
	if local != nil {
		local.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "two refs") {
		t.Fatalf("build prompt sandbox body = %v, want a refusal naming the two refs", err)
	}
}

// `sandbox create` files a skills repository beside the references it was
// given outright, and refuses one that lands on one of them.
func TestAddSkillsSourcesFilesBesideTheGivenReferences(t *testing.T) {
	tools := "file://" + filepath.ToSlash(newRunSourceTestRepoIn(t, t.TempDir(), "tools"))
	given := apiclientgen.GitSource{Kind: apiclientgen.GitSourceKindGit}
	given.SetSlug(apiclientgen.NewOptString("tools"))

	config := apimodel.SandboxCreateConfig{Name: "box"}
	config.SetSourceCodeReferences(apiclientgen.NewOptSandboxCreateConfigSourceCodeReferences(
		apiclientgen.SandboxCreateConfigSourceCodeReferences{"/srv/given": given}))
	args, err := SplitSkills([]string{tools + "//skills"})
	if err != nil {
		t.Fatal(err)
	}
	if err := AddSkillsSources(context.Background(), &config, args.Repositories); err != nil {
		t.Fatalf("add skills sources: %v", err)
	}
	references, _ := config.SourceCodeReferences.Get()
	key := "~/.discobox/skill-sources/tools"
	reference, ok := references[key]
	if !ok || len(references) != 2 {
		t.Fatalf("references = %v, want the given one and %s", references, key)
	}
	if slug := reference.Slug.Or(""); slug == "tools" {
		t.Fatalf("skills source took the slug %q a given reference holds", slug)
	}
	if want := []apimodel.SandboxSkillsSource{{Source: key, Directory: "skills"}}; !reflect.DeepEqual(config.SkillsSources, want) {
		t.Fatalf("skills sources = %+v, want %+v", config.SkillsSources, want)
	}

	config = apimodel.SandboxCreateConfig{Name: "box"}
	config.SetSourceCodeReferences(apiclientgen.NewOptSandboxCreateConfigSourceCodeReferences(
		apiclientgen.SandboxCreateConfigSourceCodeReferences{key: given}))
	if err := AddSkillsSources(context.Background(), &config, args.Repositories); err == nil {
		t.Fatal("add skills sources onto an occupied directory: nil, want a refusal")
	}
}

// The project's skills sources go ahead of the create's own --skills, so a
// person's own directory is the later one and wins on a skill's name (ADR
// 26-10-14-894 §2).
func TestBuildPromptSandboxBodyFilesProjectSkillsFirst(t *testing.T) {
	tools := "file://" + filepath.ToSlash(newRunSourceTestRepoIn(t, t.TempDir(), "tools"))
	shared := "file://" + filepath.ToSlash(newRunSourceTestRepoIn(t, t.TempDir(), "shared"))

	body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Platform:      linuxSandbox,
		NoSource:      true,
		IncludeDirty:  IncludeDirtyNever,
		ProjectSkills: []SkillsRepository{projectEntry(shared, "", "skills")},
		SkillDirs:     []string{tools},
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}
	defer local.Close()
	want := []apimodel.SandboxSkillsSource{
		{Source: "~/.discobox/skill-sources/shared", Directory: "skills"},
		{Source: "~/.discobox/skill-sources/tools", Directory: "."},
	}
	if !reflect.DeepEqual(body.Config.SkillsSources, want) {
		t.Fatalf("skills sources = %+v, want %+v", body.Config.SkillsSources, want)
	}
}

// A --skills naming a repository the project names at another ref is refused
// as two --skills are, and the refusal says how to leave the project's out.
func TestBuildPromptSandboxBodyRefusesAProjectRepositoryAtAnotherRef(t *testing.T) {
	tools := "file://" + filepath.ToSlash(newRunSourceTestRepoIn(t, t.TempDir(), "tools"))
	_, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Platform:      linuxSandbox,
		NoSource:      true,
		IncludeDirty:  IncludeDirtyNever,
		ProjectSkills: []SkillsRepository{projectEntry(tools, "main", ".")},
		SkillDirs:     []string{tools + "//b"},
	})
	if local != nil {
		local.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "the project's skills source "+tools+"@main") || !strings.Contains(err.Error(), "--no-project-skills") {
		t.Fatalf("build prompt sandbox body = %v, want a refusal naming the project's and --no-project-skills", err)
	}
}

// A project entry is the --skills value it stands for.
func TestProjectSkillsRepositoryIsItsSkillsValue(t *testing.T) {
	for _, tc := range []struct {
		url, ref, dir string
		arg           string
	}{
		{url: "https://github.com/acme/tools", dir: ".", arg: "https://github.com/acme/tools"},
		{url: "https://github.com/acme/tools", ref: "v2", dir: "agents/skills", arg: "https://github.com/acme/tools@v2//agents/skills"},
		{url: "git@github.com:acme/tools", dir: "", arg: "git@github.com:acme/tools"},
	} {
		repo, err := ProjectSkillsRepository(tc.url, tc.ref, tc.dir)
		if err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		split, err := SplitSkills([]string{repo.Arg})
		if err != nil || repo.Arg != tc.arg || len(split.Repositories) != 1 {
			t.Fatalf("%+v: arg %q (%v), want %q", tc, repo.Arg, err, tc.arg)
		}
		if got := split.Repositories[0]; got.Key() != repo.Key() {
			t.Fatalf("%q reads back as %+v, want %+v", repo.Arg, got, repo)
		}
		if repo.Source.Location != tc.url || repo.Source.Ref != tc.ref {
			t.Fatalf("%q: url, ref = %q, %q", repo.Arg, repo.Source.Location, repo.Source.Ref)
		}
	}
}

// projectEntry is a project entry for a test repository, which is a file URL
// the project itself would refuse: the order and refusals it is used for are
// the create's, past the check ProjectSkillsRepository makes.
func projectEntry(url, ref, dir string) SkillsRepository {
	repo, _ := ProjectSkillsRepository(url, ref, dir)
	return repo
}

// An entry is checked again on the client: it is what the server answered,
// and another member wrote it.
//
//nolint:gosec // G101: fixtures of a credential in a URL, which is what is refused.
func TestProjectSkillsRepositoryRefusesWhatTheServerShouldHave(t *testing.T) {
	for _, tc := range []struct{ url, ref, dir string }{
		{url: "--upload-pack=touch x#@x:y", ref: "."},
		{url: "https://x-access-token:ghp_x@github.com/acme/tools"},
		{url: "file:///srv/tools"},
		{url: "https://github.com/acme/tools", dir: "../etc"},
	} {
		if _, err := ProjectSkillsRepository(tc.url, tc.ref, tc.dir); err == nil || !strings.Contains(err.Error(), "the project's skills source") {
			t.Errorf("ProjectSkillsRepository(%q, %q, %q) = %v, want a refusal naming the project's entry", tc.url, tc.ref, tc.dir, err)
		}
	}
}
