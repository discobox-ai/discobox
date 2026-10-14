package sandboxcreate

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
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
			want := []SkillsRepository{{Arg: tc.value, Source: tc.source, Directory: tc.directory}}
			if len(got.Directories) != 0 || !reflect.DeepEqual(got.Repositories, want) {
				t.Fatalf("split = %+v, want %+v", got, want)
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

// Each skills repository is one reference marked for its skills, checked out
// under skill-sources, and the directories are filed in the order given, one
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
	toolsKey, sharedKey := "/workspace/skill-sources/tools", "/workspace/skill-sources/shared"
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
	key := "/workspace/skill-sources/tools"
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
