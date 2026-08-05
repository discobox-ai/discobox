package dockercache

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func withIndex(t *testing.T, home string) {
	t.Helper()
	dir := CacheDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write index.json: %v", err)
	}
}

func joined(a Args) string { return strings.Join(a.Argv, " ") }

// `docker build` has no --cache-to, so it must be promoted to buildx.
func TestRewritePromotesLegacyBuild(t *testing.T) {
	home := t.TempDir()
	got := Rewrite([]string{"build", "-t", "x", "."}, home)

	if !got.Injected {
		t.Fatalf("expected cache injection, got %v", got.Argv)
	}
	want := []string{RealDocker, "buildx", "build", "-t", "x", "."}
	if !slices.Equal(got.Argv[:len(want)], want) {
		t.Fatalf("argv = %v, want prefix %v", got.Argv, want)
	}
	if !strings.Contains(joined(got), "--cache-to type=local,dest="+CacheDir(home)+",mode=max") {
		t.Fatalf("missing cache-to: %v", got.Argv)
	}
}

// Importing from a directory with no prior export makes BuildKit error, so
// --cache-from appears only once an index.json exists.
func TestRewriteImportsOnlyWhenCacheExists(t *testing.T) {
	home := t.TempDir()
	if strings.Contains(joined(Rewrite([]string{"build", "."}, home)), "--cache-from") {
		t.Fatal("cache-from must not be injected before any export exists")
	}
	withIndex(t, home)
	if !strings.Contains(joined(Rewrite([]string{"build", "."}, home)), "--cache-from type=local,src="+CacheDir(home)) {
		t.Fatal("cache-from must be injected once a cache exists")
	}
}

func TestRewriteBuildxBuildIsNotDoublePromoted(t *testing.T) {
	home := t.TempDir()
	got := Rewrite([]string{"buildx", "build", "."}, home)

	if !got.Injected {
		t.Fatalf("expected injection, got %v", got.Argv)
	}
	var n int
	for _, a := range got.Argv {
		if a == "build" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one build token, got %d: %v", n, got.Argv)
	}
	want := []string{RealDocker, "buildx", "build", "."}
	if !slices.Equal(got.Argv[:len(want)], want) {
		t.Fatalf("argv = %v, want prefix %v", got.Argv, want)
	}
}

// An explicit user choice always wins over the injected default.
func TestRewriteRespectsUserCacheFlags(t *testing.T) {
	home := t.TempDir()
	withIndex(t, home)
	for _, args := range [][]string{
		{"build", "--cache-from", "type=registry,ref=r/x", "."},
		{"build", "--cache-to=type=inline", "."},
		{"buildx", "build", "--cache-from", "type=gha", "."},
	} {
		got := Rewrite(args, home)
		if got.Injected {
			t.Fatalf("must not inject over user flags for %v: %v", args, got.Argv)
		}
		if !slices.Equal(got.Argv, append([]string{RealDocker}, args...)) {
			t.Fatalf("args mutated: %v", got.Argv)
		}
	}
}

// Only build commands are touched; everything else passes through byte-for-byte.
func TestRewritePassesThroughNonBuild(t *testing.T) {
	home := t.TempDir()
	for _, args := range [][]string{
		{"run", "--rm", "alpine", "true"},
		{"ps"},
		{"buildx", "ls"},
		{"builder", "prune", "-f"},
		{},
	} {
		got := Rewrite(args, home)
		if got.Injected {
			t.Fatalf("must not inject for %v", args)
		}
		if !slices.Equal(got.Argv, append([]string{RealDocker}, args...)) {
			t.Fatalf("args mutated for %v: %v", args, got.Argv)
		}
	}
}

// A global flag taking a separate value must not have its value mistaken for
// the subcommand -- `docker --context foo build .` is still a build.
func TestRewriteFindsBuildBehindGlobalFlags(t *testing.T) {
	home := t.TempDir()
	for _, args := range [][]string{
		{"--context", "foo", "build", "."},
		{"--log-level=debug", "build", "."},
		{"-H", "unix:///x.sock", "buildx", "build", "."},
	} {
		if got := Rewrite(args, home); !got.Injected {
			t.Fatalf("expected injection for %v, got %v", args, got.Argv)
		}
	}
	// ...and a value that merely looks like a subcommand is not one.
	if got := Rewrite([]string{"--context", "build", "ps"}, home); got.Injected {
		t.Fatalf("--context's value must not be read as the subcommand: %v", got.Argv)
	}
}

// A read-only or missing cache volume must degrade to an uncached build, never
// break the user's command.
func TestRewriteFallsBackWhenCacheDirUnusable(t *testing.T) {
	home := filepath.Join(t.TempDir(), "nope")
	if err := os.WriteFile(home, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	got := Rewrite([]string{"build", "."}, home)
	if got.Injected {
		t.Fatalf("expected pass-through when cache dir cannot be created: %v", got.Argv)
	}
	if !slices.Equal(got.Argv, []string{RealDocker, "build", "."}) {
		t.Fatalf("argv = %v", got.Argv)
	}
}
