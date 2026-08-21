package sandboxcreate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An extra source is the sandbox's to reach at the same path it has here, under
// the name the directory already has. That is the whole of what `-i ../foo`
// promises: the sandbox has foo, at foo's path.
func TestBuildPromptSandboxBodyPlacesAnIncludedDirectoryAtItsOwnPath(t *testing.T) {
	primary := newRunSourceTestRepo(t)
	reference := newNamedRunSourceTestRepo(t, "foo")

	body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Source:       primary,
		Include:      []string{reference},
		IncludeDirty: IncludeDirtyNever,
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}
	defer local.Close()

	references, ok := body.Config.SourceCodeReferences.Get()
	if !ok || len(references) != 1 {
		t.Fatalf("source code references = %v, want the one included source", references)
	}
	sandboxPath := wantSandboxPath(t, reference)
	source, ok := references[sandboxPath]
	if !ok {
		t.Fatalf("references are keyed %v, want the source's own directory %s", references, sandboxPath)
	}
	if slug := source.Slug.Or(""); slug != "foo" {
		t.Fatalf("slug = %q, want the directory's own name", slug)
	}
	if source.LocalDirectory.Or("") != reference {
		t.Fatalf("local directory = %q, want %s", source.LocalDirectory.Or(""), reference)
	}
	destination, _ := source.Destination.Get()
	if destination.Directory.Or("") != sandboxPath {
		t.Fatalf("destination = %q, want the same path the source has here", destination.Directory.Or(""))
	}
	// Only the primary source decides where the harness starts.
	if working, ok := destination.WorkingDirectory.Get(); ok {
		t.Fatalf("reference set a working directory %q; that is the primary source's to name", working)
	}
	if checkout, ok := source.Checkout.Get(); !ok || checkout.Commit.Or("") == "" {
		t.Fatalf("checkout = %v, want the commit resolved at create", checkout)
	}
	// The primary source is still the server's to name.
	primarySource, _ := body.Config.Source.Get()
	if slug, ok := primarySource.Slug.Get(); ok {
		t.Fatalf("primary source claimed the slug %q; the server names it", slug)
	}
}

// Two directories can share a name. The client settles that itself so the slug
// it will address a push with is the slug the server records.
func TestBuildPromptSandboxBodySeparatesIncludedSourcesWithTheSameName(t *testing.T) {
	body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Source:       newRunSourceTestRepo(t),
		Include:      []string{newNamedRunSourceTestRepo(t, "foo"), newNamedRunSourceTestRepo(t, "foo")},
		IncludeDirty: IncludeDirtyNever,
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}
	defer local.Close()

	references, _ := body.Config.SourceCodeReferences.Get()
	slugs := map[string]struct{}{}
	for _, source := range references {
		slugs[source.Slug.Or("")] = struct{}{}
	}
	if len(references) != 2 || len(slugs) != 2 {
		t.Fatalf("references = %v, want two sources under two slugs", references)
	}
	if _, ok := slugs["foo"]; !ok {
		t.Fatalf("slugs = %v, want the first one to keep the name foo", slugs)
	}
	if _, ok := slugs["foo-2"]; !ok {
		t.Fatalf("slugs = %v, want the second one numbered off foo", slugs)
	}
}

// Including the same directory twice is a mistake, not a request for two copies
// of it: they would land on top of each other in the sandbox.
func TestBuildPromptSandboxBodyRefusesTheSameIncludedDirectoryTwice(t *testing.T) {
	reference := newNamedRunSourceTestRepo(t, "foo")

	_, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Source:       newRunSourceTestRepo(t),
		Include:      []string{reference, filepath.Join(reference, ".")},
		IncludeDirty: IncludeDirtyNever,
	})
	defer local.Close()
	if err == nil {
		t.Fatal("including one directory twice: got nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "already included") {
		t.Fatalf("error = %v, want it to name the duplicate", err)
	}
}

// Including the source the sandbox already runs against would put two sources
// on one directory, so it is the same mistake as including one twice.
func TestBuildPromptSandboxBodyRefusesIncludingThePrimarySource(t *testing.T) {
	primary := newRunSourceTestRepo(t)

	_, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Source:       primary,
		Include:      []string{primary},
		IncludeDirty: IncludeDirtyNever,
	})
	defer local.Close()
	if err == nil {
		t.Fatal("including the primary source: got nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "already included") {
		t.Fatalf("error = %v, want it to name the duplicate", err)
	}
}

// A subdirectory brings in the repository that holds it, exactly as the primary
// source does, and is named after that repository rather than the subdirectory.
func TestBuildPromptSandboxBodyIncludesTheRepositoryHoldingASubdirectory(t *testing.T) {
	reference := newNamedRunSourceTestRepo(t, "foo")
	if err := os.MkdirAll(filepath.Join(reference, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Source:       newRunSourceTestRepo(t),
		Include:      []string{filepath.Join(reference, "sub")},
		IncludeDirty: IncludeDirtyNever,
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}
	defer local.Close()

	references, _ := body.Config.SourceCodeReferences.Get()
	source, ok := references[wantSandboxPath(t, reference)]
	if !ok {
		t.Fatalf("references are keyed %v, want the repository root %s", references, reference)
	}
	if slug := source.Slug.Or(""); slug != "foo" {
		t.Fatalf("slug = %q, want the repository's name", slug)
	}
}

// Each included source is its own working tree, so each is snapshotted on its
// own — and each has its own repository to push out of, which delivery finds by
// the key the source was filed under.
func TestBuildPromptSandboxBodySnapshotsEachIncludedSourceOnItsOwn(t *testing.T) {
	reference := newNamedRunSourceTestRepo(t, "foo")
	if err := os.WriteFile(filepath.Join(reference, "tracked.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Source:       newRunSourceTestRepo(t),
		Include:      []string{reference},
		IncludeDirty: IncludeDirtyAlways,
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}
	defer local.Close()

	references, _ := body.Config.SourceCodeReferences.Get()
	sandboxPath := wantSandboxPath(t, reference)
	workspace, _ := references[sandboxPath].Workspace.Get()
	if workspace.Mode.Or("") != "dirty" || workspace.SnapshotRef.Or("") == "" {
		t.Fatalf("workspace = %v, want the included source's own uncommitted work carried", workspace)
	}
	// The primary source is clean, and its own snapshot is unaffected by the
	// reference's.
	primary, _ := body.Config.Source.Get()
	primaryWorkspace, _ := primary.Workspace.Get()
	if primaryWorkspace.SnapshotRef.Or("") != "" {
		t.Fatalf("primary workspace = %v, want a clean checkout", primaryWorkspace)
	}

	root, err := local.pushRoot(sandboxPath)
	if err != nil || root != reference {
		t.Fatalf("pushRoot(%s) = %q, %v, want the included repository", sandboxPath, root, err)
	}
}

// A directory in no repository can be included too: it is delivered the same
// way the primary source would be, out of a throwaway repository the caller
// closes.
func TestBuildPromptSandboxBodyIncludesADirectoryWithNoRepository(t *testing.T) {
	reference := filepath.Join(testWorkspace(t), "foo")
	if err := os.MkdirAll(reference, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reference, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{
		Source:  newRunSourceTestRepo(t),
		Include: []string{reference},
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}

	references, _ := body.Config.SourceCodeReferences.Get()
	source := references[wantSandboxPath(t, reference)]
	if !source.NoLocalRepository.Or(false) || source.LocalDirectory.Or("") != reference {
		t.Fatalf("source = %v, want %s recorded as a directory with no repository", source, reference)
	}
	root, err := local.pushRoot(wantSandboxPath(t, reference))
	if err != nil {
		t.Fatalf("pushRoot: %v", err)
	}
	if root == reference {
		t.Fatalf("push root = %q, want a repository built outside the directory", root)
	}
	if _, err := os.Stat(filepath.Join(reference, ".git")); !os.IsNotExist(err) {
		t.Fatalf("stat %s/.git = %v, want the directory left as it was found", reference, err)
	}

	// Closing releases every throwaway repository, not just the primary one.
	local.Close()
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("stat %s = %v, want the throwaway repository deleted", root, err)
	}
}

// newNamedRunSourceTestRepo is a repository whose directory has a name worth
// asserting on, which t.TempDir's own generated name is not.
func newNamedRunSourceTestRepo(t *testing.T, name string) string {
	t.Helper()
	repo := filepath.Join(testWorkspace(t), name)
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
