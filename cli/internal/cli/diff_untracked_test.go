package cli

import (
	"strings"
	"testing"
)

func TestParseMaxUntracked(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"512MiB", 512 << 20},
		{"2g", 2 << 30},
		{"0", 0},
		{"none", 0},
		{"", 0},
		{"  1MiB  ", 1 << 20},
	} {
		got, err := parseMaxUntracked(tc.in)
		if err != nil {
			t.Fatalf("parseMaxUntracked(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("parseMaxUntracked(%q): got %d, want %d", tc.in, got, tc.want)
		}
	}
	if _, err := parseMaxUntracked("enormous"); err == nil {
		t.Fatal("a value that is not a size must be rejected")
	}
}

// TestUntrackedPayloadCommand covers the guard against the case it exists for:
// a package store nothing ignores, which the diff would otherwise hash into the
// sandbox's object database in full.
func TestUntrackedPayloadCommand(t *testing.T) {
	repo := newGitRepo(t)
	repo.commit("tracked.txt", "one\n", "init")
	repo.write("src/app.go", "package app\n")
	repo.write(".pnpm-store/a", strings.Repeat("x", 4096))
	repo.write(".pnpm-store/b", strings.Repeat("y", 4096))

	over, err := repo.run(untrackedPayloadCommand(nil, 1024))
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	for _, want := range []string{"untracked files come to", "over the 1.0 KiB limit", ".pnpm-store:", "--max-untracked"} {
		if !strings.Contains(over, want) {
			t.Fatalf("report missing %q:\n%s", want, over)
		}
	}

	// Under the limit the guard says nothing at all, which is what lets the
	// caller treat any output as the reason to stop.
	under, err := repo.run(untrackedPayloadCommand(nil, 1<<30))
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if strings.TrimSpace(under) != "" {
		t.Fatalf("a payload under the limit must report nothing:\n%s", under)
	}

	// A pathspec is the documented way past the guard, so it has to narrow what
	// the guard measures too.
	narrowed, err := repo.run(untrackedPayloadCommand([]string{"src"}, 1024))
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if strings.TrimSpace(narrowed) != "" {
		t.Fatalf("a pathspec that excludes the payload must clear the guard:\n%s", narrowed)
	}

	// Ignored files are not hashed either, so they must not count against it.
	repo.write(".gitignore", ".pnpm-store/\n")
	ignored, err := repo.run(untrackedPayloadCommand(nil, 1024))
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if strings.Contains(ignored, ".pnpm-store") {
		t.Fatalf("an ignored directory is never hashed and must not trip the guard:\n%s", ignored)
	}
}

// TestUntrackedPayloadCommandWithNothingUntracked covers the empty list, where
// xargs would otherwise run stat with no arguments and report an error.
func TestUntrackedPayloadCommandWithNothingUntracked(t *testing.T) {
	repo := newGitRepo(t)
	repo.commit("tracked.txt", "one\n", "init")

	out, err := repo.run(untrackedPayloadCommand(nil, 1))
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("nothing untracked is nothing to measure:\n%s", out)
	}
}
