// Package cichanges_test checks how CI decides a pull request changes only
// docs/: scripts/ci-changes.sh's answer, and that every job of the CI
// workflow waits on it.
package cichanges_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Each case is a pull request: a base commit, then one commit of changes,
// merged the way GitHub's refs/pull/N/merge is, and the script run on that
// merge commit as the changes job runs it.
func TestChanges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	script, err := filepath.Abs("../../scripts/ci-changes.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		event string
		pr    func(t *testing.T, dir string)
		want  string
	}{
		{"docs only", "pull_request", func(t *testing.T, dir string) {
			write(t, dir, "docs/adr/new.md", "new\n")
			write(t, dir, "docs/public/cli.md", "changed\n")
		}, "code=false"},
		{"code beside docs", "pull_request", func(t *testing.T, dir string) {
			write(t, dir, "docs/adr/new.md", "new\n")
			write(t, dir, "cli/main.go", "package main // changed\n")
		}, "code=true"},
		{"top-level Markdown is code", "pull_request", func(t *testing.T, dir string) {
			write(t, dir, "README.md", "changed\n")
		}, "code=true"},
		{"a design doc beside code is code", "pull_request", func(t *testing.T, dir string) {
			write(t, dir, "cli/DESIGN.md", "changed\n")
		}, "code=true"},
		{"a path that only starts with docs is code", "pull_request", func(t *testing.T, dir string) {
			write(t, dir, "docsite/index.md", "new\n")
		}, "code=true"},
		{"a file moved into docs/ counts where it was", "pull_request", func(t *testing.T, dir string) {
			git(t, dir, "mv", "cli/main.go", "docs/main.go")
		}, "code=true"},
		{"a file deleted outside docs/", "pull_request", func(t *testing.T, dir string) {
			git(t, dir, "rm", "-q", "cli/main.go")
		}, "code=true"},
		{"a push is never filtered", "push", func(t *testing.T, dir string) {
			write(t, dir, "docs/adr/new.md", "new\n")
		}, "code=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			git(t, dir, "init", "-q", "-b", "main")
			write(t, dir, "cli/main.go", "package main\n")
			write(t, dir, "docs/adr/old.md", "old\n")
			commit(t, dir, "base")
			// The base branch moves on after the PR branches off, so HEAD^1
			// differs from the PR's own parent, as it does on GitHub.
			git(t, dir, "checkout", "-q", "-b", "pr")
			tc.pr(t, dir)
			commit(t, dir, "pr")
			git(t, dir, "checkout", "-q", "main")
			write(t, dir, "server/main.go", "package main\n")
			commit(t, dir, "main moves on")
			git(t, dir, "merge", "-q", "--no-ff", "-m", "merge", "pr")

			output := filepath.Join(t.TempDir(), "output")
			cmd := exec.CommandContext(t.Context(), script, tc.event) //nolint:gosec // G204: the repository's own script, run on a fixture event name.
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GITHUB_OUTPUT="+output)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s: %v", out, err)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Errorf("printed %q, want %q", got, tc.want)
			}
			written, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(written)); got != tc.want {
				t.Errorf("wrote %q to GITHUB_OUTPUT, want %q", got, tc.want)
			}
		})
	}
}

// A job added to CI later runs on a docs-only pull request unless it is gated
// like the others; this makes that a decision rather than an omission.
func TestEveryJobWaitsOnChanges(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Needs any    `yaml:"needs"`
			If    string `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if _, ok := workflow.Jobs["changes"]; !ok {
		t.Fatal("ci.yml has no changes job")
	}
	for name, job := range workflow.Jobs {
		// Fails closed: a failed or missing answer runs the job.
		want := "${{ !cancelled() && needs.changes.outputs.code != 'false' }}" //nolint:misspell // GitHub's expression function is spelled cancelled().
		switch name {
		case "changes":
			continue
		case "docs":
			want = "needs.changes.outputs.code == 'false'"
		}
		if job.Needs != "changes" {
			t.Errorf("job %s needs %v, want changes", name, job.Needs)
		}
		if job.If != want {
			t.Errorf("job %s runs if %q, want %q", name, job.If, want)
		}
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, dir, message string) {
	t.Helper()
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", message)
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
