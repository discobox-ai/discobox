package sandboxcreate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A repository declaring foo means the ../foo the caller already has: the
// checkout is used, at its own path, exactly as -i ../foo would.
func TestBuildPromptSandboxBodyPrefersACheckoutOfADeclaredSource(t *testing.T) {
	workspace := testWorkspace(t)
	primary := newRunSourceTestRepoIn(t, workspace, "app")
	reference := newRunSourceTestRepoIn(t, workspace, "foo")
	declareSources(t, primary, map[string]string{"foo": "https://github.com/acme/foo"})

	var reported []DeclaredSource
	body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Source:               primary,
		IncludeDirty:         IncludeDirtyNever,
		ReportDeclaredSource: func(source DeclaredSource) { reported = append(reported, source) },
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}
	defer local.Close()

	references, _ := body.Config.SourceCodeReferences.Get()
	source, ok := references[wantSandboxPath(t, reference)]
	if !ok {
		t.Fatalf("references are keyed %v, want the local checkout %s", references, reference)
	}
	if source.Slug.Or("") != "foo" || source.LocalDirectory.Or("") != reference {
		t.Fatalf("source = %v, want the local checkout under the declared name", source)
	}
	if _, ok := source.URL.Get(); ok {
		t.Fatalf("source = %v, want it taken from disk rather than cloned", source)
	}
	if len(reported) != 1 || !reported[0].Local || reported[0].Checkout != reference {
		t.Fatalf("reported = %+v, want the local checkout announced", reported)
	}
}

// With no checkout beside the source, the declared URL is cloned — and it lands
// at the path the checkout would have occupied, so ../foo means the same thing
// inside the sandbox either way. That is the whole promise of declaring it.
func TestBuildPromptSandboxBodyClonesADeclaredSourceToTheSamePath(t *testing.T) {
	workspace := testWorkspace(t)
	primary := newRunSourceTestRepoIn(t, workspace, "app")
	// A file:// URL is a remote as far as source resolution is concerned — it
	// is cloned by the sandbox rather than taken from disk here — without a
	// test needing the network.
	remote := "file://" + filepath.ToSlash(newRunSourceTestRepoIn(t, t.TempDir(), "foo"))
	declareSources(t, primary, map[string]string{"foo": remote})

	var reported []DeclaredSource
	body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Source:               primary,
		IncludeDirty:         IncludeDirtyNever,
		ReportDeclaredSource: func(source DeclaredSource) { reported = append(reported, source) },
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}
	defer local.Close()

	want := filepath.Join(workspace, "foo")
	references, _ := body.Config.SourceCodeReferences.Get()
	source, ok := references[wantSandboxPath(t, want)]
	if !ok {
		t.Fatalf("references are keyed %v, want the sibling path %s a checkout would have used", references, want)
	}
	url, ok := source.URL.Get()
	if !ok || url.String() != remote {
		t.Fatalf("source = %v, want it cloned from the declared URL %s", source, remote)
	}
	if checkout, ok := source.Checkout.Get(); !ok || checkout.Commit.Or("") == "" {
		t.Fatalf("checkout = %v, want the remote's commit resolved at create", checkout)
	}
	if len(reported) != 1 || reported[0].Local || reported[0].Checkout != want {
		t.Fatalf("reported = %+v, want the missing checkout named", reported)
	}
}

// A fork checked out next door is used, because it is what the caller has — but
// the disagreement is reported, since a directory that merely shares the name
// looks identical from here.
func TestBuildPromptSandboxBodyReportsACheckoutThatDisagreesWithTheDeclaredURL(t *testing.T) {
	workspace := testWorkspace(t)
	primary := newRunSourceTestRepoIn(t, workspace, "app")
	reference := newRunSourceTestRepoIn(t, workspace, "foo")
	runSourceTestGit(t, reference)("remote", "add", "origin", "https://github.com/darren/foo.git")
	declareSources(t, primary, map[string]string{"foo": "https://github.com/acme/foo"})

	var reported []DeclaredSource
	_, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Source:               primary,
		IncludeDirty:         IncludeDirtyNever,
		ReportDeclaredSource: func(source DeclaredSource) { reported = append(reported, source) },
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}
	defer local.Close()

	if len(reported) != 1 || !reported[0].Local {
		t.Fatalf("reported = %+v, want the checkout used anyway", reported)
	}
	if reported[0].Origin != "https://github.com/darren/foo.git" {
		t.Fatalf("reported origin = %q, want the checkout's own remote", reported[0].Origin)
	}
}

// The same repository over ssh, over https, with or without .git is the same
// repository, and must not be reported as a disagreement.
func TestDeclaredSourceOriginMatchesAcrossURLForms(t *testing.T) {
	same := [][2]string{
		{"https://github.com/acme/foo.git", "https://github.com/acme/foo"},
		{"git@github.com:acme/foo.git", "https://github.com/acme/foo"},
		{"ssh://git@github.com/acme/foo", "https://github.com/acme/foo/"},
		{"https://GitHub.com/Acme/Foo", "https://github.com/acme/foo"},
	}
	for _, pair := range same {
		if !sameGitRemote(pair[0], pair[1]) {
			t.Errorf("%q and %q were read as different repositories", pair[0], pair[1])
		}
	}
	if sameGitRemote("https://github.com/acme/foo", "https://github.com/darren/foo") {
		t.Error("two different repositories were read as the same one")
	}
}

// An explicit -i is the caller's own instruction and outranks the repository's
// declaration of the same source, rather than colliding with it.
func TestBuildPromptSandboxBodyLetsIncludeOverrideADeclaredSource(t *testing.T) {
	workspace := testWorkspace(t)
	primary := newRunSourceTestRepoIn(t, workspace, "app")
	reference := newRunSourceTestRepoIn(t, workspace, "foo")
	declareSources(t, primary, map[string]string{"foo": "https://github.com/acme/foo"})

	body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Source:       primary,
		Include:      []string{reference},
		IncludeDirty: IncludeDirtyNever,
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}
	defer local.Close()

	references, _ := body.Config.SourceCodeReferences.Get()
	if len(references) != 1 {
		t.Fatalf("references = %v, want the one source, not it and its declaration", references)
	}
	if slug := references[wantSandboxPath(t, reference)].Slug.Or(""); slug != "foo" {
		t.Fatalf("slug = %q, want foo taken once", slug)
	}
}

// Declared sources are what the repository asks for, so opting out has to be
// possible for a caller who wants only what they named.
func TestBuildPromptSandboxBodySkipsDeclaredSourcesWhenAsked(t *testing.T) {
	workspace := testWorkspace(t)
	primary := newRunSourceTestRepoIn(t, workspace, "app")
	newRunSourceTestRepoIn(t, workspace, "foo")
	declareSources(t, primary, map[string]string{"foo": "https://github.com/acme/foo"})

	body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Source:              primary,
		IncludeDirty:        IncludeDirtyNever,
		SkipDeclaredSources: true,
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}
	defer local.Close()

	if references, ok := body.Config.SourceCodeReferences.Get(); ok && len(references) > 0 {
		t.Fatalf("references = %v, want none once declared sources are skipped", references)
	}
}

// The file states what the sandbox must contain. A file that cannot be read
// means that statement is unknown, and creating a sandbox missing the sources
// it names is worse than not creating one.
func TestBuildPromptSandboxBodyRefusesAMalformedDeclaration(t *testing.T) {
	workspace := testWorkspace(t)
	primary := newRunSourceTestRepoIn(t, workspace, "app")
	if err := os.MkdirAll(filepath.Join(primary, ".discobox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(primary, ".discobox", "sources.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Source:       primary,
		IncludeDirty: IncludeDirtyNever,
	})
	defer local.Close()
	if err == nil {
		t.Fatal("a malformed sources.json: got nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "sources.json") {
		t.Fatalf("error = %v, want it to name the file", err)
	}
}

// Where a declared source is looked for locally is not the file's to say: the
// checkout beside the primary source is found by name. A path here would be
// resolved against whatever directory the caller ran from, so it is refused
// rather than quietly bringing in some other repository.
func TestBuildPromptSandboxBodyRefusesAPathInsteadOfAURL(t *testing.T) {
	for _, value := range []string{"..", "../foo", "/srv/foo", "./foo"} {
		t.Run(value, func(t *testing.T) {
			workspace := testWorkspace(t)
			primary := newRunSourceTestRepoIn(t, workspace, "app")
			newRunSourceTestRepoIn(t, workspace, "foo")
			declareSources(t, primary, map[string]string{"foo": value})

			_, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
				Source:       primary,
				IncludeDirty: IncludeDirtyNever,
			})
			defer local.Close()
			if err == nil {
				t.Fatalf("declared source %q: got nil error, want a refusal", value)
			}
			if !strings.Contains(err.Error(), "not a Git URL") {
				t.Fatalf("error = %v, want it to say the value is not a Git URL", err)
			}
		})
	}
}

func declareSources(t *testing.T, root string, declared map[string]string) {
	t.Helper()
	dir := filepath.Join(root, ".discobox")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(declared)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sources.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRunSourceTestRepoIn is a repository at a named path inside a workspace, so
// a test can put two of them side by side the way a caller's checkouts are.
// testWorkspace is a temporary directory whose name matches what git reports
// for it. Windows hands out an 8.3 short name (RUNNER~1) and macOS hands out
// /var where git says /private/var, and these tests compare a path they built
// against one that came back through source resolution.
func testWorkspace(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func newRunSourceTestRepoIn(t *testing.T, workspace, name string) string {
	t.Helper()
	repo := filepath.Join(workspace, name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := runSourceTestGit(t, repo)
	git("init", "-b", "main")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt")
	git("commit", "-m", "base")
	return repo
}
