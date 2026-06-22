package matcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	hooks "github.com/obot-platform/discobox/hooks"
	"github.com/obot-platform/discobox/hooks/watcher"
)

func TestMatchAppliesGlobalHookPatternsAndSortsChanges(t *testing.T) {
	repo := t.TempDir()
	hookDefs := []hooks.Hook{
		{ID: "first", Name: "First", Type: hooks.HookTypeFile, Pattern: "**/*.{go,mod}", Ignore: []string{"**/*_test.go", "vendor/**"}},
		{ID: "second", Name: "Second", Type: hooks.HookTypeFile, Pattern: "src/**/*.ts", Ignore: []string{"src/generated/**"}},
		{ID: "session", Name: "Session", Type: hooks.HookTypeSession},
	}
	changes := []watcher.Change{
		{Path: filepath.Join(repo, "vendor/lib.go"), Kind: watcher.Modified},
		{Path: "src/generated/client.ts", Kind: watcher.Created},
		{Path: "tmp/skip.go", Kind: watcher.Modified},
		{Path: "b.go", Kind: watcher.Deleted},
		{Path: "a_test.go", Kind: watcher.Modified},
		{Path: "src/app.ts", Kind: watcher.Created},
		{Path: "go.mod", Kind: watcher.Modified},
		{Path: "a.go", Kind: watcher.Created},
	}

	res, err := Match(repo, hookDefs, changes, []string{"tmp/**"}, Options{DisableGitIgnore: true})
	if err != nil {
		t.Fatalf("Match returned error: %v", err)
	}
	if got, want := hookIDs(res.Matches), []string{"first", "second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hook IDs = %v, want %v", got, want)
	}
	if got, want := changeKeys(res.Matches[0].Changes), []string{"a.go:created", "b.go:deleted", "go.mod:modified"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first changes = %v, want %v", got, want)
	}
	if got, want := changeKeys(res.Matches[1].Changes), []string{"src/app.ts:created"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second changes = %v, want %v", got, want)
	}
	if got, want := skippedReasons(res.Skipped), []string{"tmp/skip.go:modified:global_ignored"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skipped = %v, want %v", got, want)
	}
}

func TestMatchNormalizesRelativeSlashPathsAndRejectsOutsideRepo(t *testing.T) {
	repo := t.TempDir()
	entry := &watcher.Entry{Path: "dir\\file.go"}
	res, err := Match(repo, []hooks.Hook{{ID: "go", Type: hooks.HookTypeFile, Pattern: "dir/*.go"}}, []watcher.Change{
		{Path: "." + string(filepath.Separator) + "dir" + string(filepath.Separator) + "file.go", Kind: watcher.Modified, Entry: entry},
		{Path: filepath.Dir(repo), Kind: watcher.Modified},
		{Path: "", Kind: watcher.Created},
	}, nil, Options{DisableGitIgnore: true})
	if err != nil {
		t.Fatalf("Match returned error: %v", err)
	}
	if got, want := changeKeys(res.Matches[0].Changes), []string{"dir/file.go:modified"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	if got := res.Matches[0].Changes[0].Entry.Path; got != "dir/file.go" {
		t.Fatalf("entry path = %q, want dir/file.go", got)
	}
	if got, want := skippedReasons(res.Skipped), []string{filepath.ToSlash(filepath.Dir(repo)) + ":modified:outside_repo", ":created:empty_path"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skipped = %v, want %v", got, want)
	}
}

func TestMatchCanDisableGitIgnoreOutsideGitWorktree(t *testing.T) {
	repo := t.TempDir()
	_, err := Match(repo, []hooks.Hook{{ID: "go", Type: hooks.HookTypeFile, Pattern: "*.go"}}, []watcher.Change{{Path: "a.go", Kind: watcher.Modified}}, nil, Options{})
	if err == nil {
		t.Fatal("Match without DisableGitIgnore in non-git dir returned nil error")
	}
	res, err := Match(repo, []hooks.Hook{{ID: "go", Type: hooks.HookTypeFile, Pattern: "*.go"}}, []watcher.Change{{Path: "a.go", Kind: watcher.Modified}}, nil, Options{DisableGitIgnore: true})
	if err != nil {
		t.Fatalf("Match with DisableGitIgnore returned error: %v", err)
	}
	if got, want := hookIDs(res.Matches), []string{"go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hook IDs = %v, want %v", got, want)
	}
}

func TestMatchFiltersGitIgnoredPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("git setup helper uses Unix-like process assumptions")
	}
	repo := t.TempDir()
	run(t, repo, "git", "init")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.log\nbuild/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Match(repo, []hooks.Hook{{ID: "all", Type: hooks.HookTypeFile, Pattern: "**/*"}}, []watcher.Change{
		{Path: "ignored.log", Kind: watcher.Modified},
		{Path: "build/out.txt", Kind: watcher.Created},
		{Path: "keep.txt", Kind: watcher.Modified},
	}, nil, Options{})
	if err != nil {
		t.Fatalf("Match returned error: %v", err)
	}
	if got, want := changeKeys(res.Matches[0].Changes), []string{"keep.txt:modified"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	if got, want := skippedReasons(res.Skipped), []string{"ignored.log:modified:git_ignored", "build/out.txt:created:git_ignored"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skipped = %v, want %v", got, want)
	}
}

func TestMatchReportsInvalidGlobPattern(t *testing.T) {
	repo := t.TempDir()
	_, err := Match(repo, []hooks.Hook{{ID: "bad", Type: hooks.HookTypeFile, Pattern: "["}}, []watcher.Change{{Path: "a.go", Kind: watcher.Modified}}, nil, Options{DisableGitIgnore: true})
	if err == nil {
		t.Fatal("Match returned nil error for invalid glob")
	}
}

func hookIDs(matches []MatchedHook) []string {
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.HookID
	}
	return out
}

func changeKeys(changes []watcher.Change) []string {
	out := make([]string, len(changes))
	for i, ch := range changes {
		out[i] = ch.Path + ":" + string(ch.Kind)
	}
	return out
}

func skippedReasons(skipped []SkippedPath) []string {
	out := make([]string, len(skipped))
	for i, s := range skipped {
		out[i] = s.Path + ":" + string(s.Kind) + ":" + string(s.Reason)
	}
	return out
}

func run(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
}
