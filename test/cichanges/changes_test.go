// Package cichanges_test checks how CI decides a pull request changes only
// docs/: scripts/ci-changes.sh's answer, and that every job of the CI
// workflow waits on it; and each workflow's concurrency group and
// cancellation.
package cichanges_test

import (
	"fmt"
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
		On struct {
			MergeGroup struct {
				Types []string `yaml:"types"`
			} `yaml:"merge_group"`
		} `yaml:"on"`
		Jobs map[string]struct {
			Needs any    `yaml:"needs"`
			If    string `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if types := workflow.On.MergeGroup.Types; len(types) != 1 || types[0] != "checks_requested" {
		t.Errorf("CI merge_group types = %v, want [checks_requested]", types)
	}
	changes, ok := workflow.Jobs["changes"]
	if !ok {
		t.Fatal("ci.yml has no changes job")
	}
	// Main pushes and merge groups skip change detection and run the code checks.
	// Unrelated label events skip change detection and all dependent jobs.
	if want := "github.event_name == 'pull_request' && (github.event.action != 'labeled' || github.event.label.name == 'macos')"; changes.If != want {
		t.Errorf("job changes runs if %q, want %q", changes.If, want)
	}
	for name, job := range workflow.Jobs {
		// A failed changes job leaves its output empty, so the job runs.
		// A skipped changes job also allows pushes and merge groups through.
		want := "${{ !cancelled() && needs.changes.outputs.code != 'false' && (github.event_name != 'pull_request' || needs.changes.result != 'skipped') }}" //nolint:misspell // GitHub's expression function is spelled cancelled().
		switch name {
		case "changes":
			continue
		case "docs":
			want = "needs.changes.outputs.code == 'false'"
		case "darwin", "darwin-test":
			// macOS bills at 20x: a pull request runs these only with the
			// `macos` label. Main pushes and merge groups always run them.
			want = "${{ !cancelled() && needs.changes.outputs.code != 'false' && (github.event_name != 'pull_request' || (needs.changes.result != 'skipped' && contains(github.event.pull_request.labels.*.name, 'macos'))) }}" //nolint:misspell // GitHub's expression function is spelled cancelled().
		}
		if job.Needs != "changes" {
			t.Errorf("job %s needs %v, want changes", name, job.Needs)
		}
		if job.If != want {
			t.Errorf("job %s runs if %q, want %q", name, job.If, want)
		}
	}
}

// Every workflow's concurrency keeps two rules. A pull_request event on a
// merged PR (a label added after the merge) has the base branch as
// github.ref, so a PR run keyed on the ref joined main's group and canceled
// main's push run (#1271): a PR's runs are keyed on its number. And a tag's
// run publishes, so nothing may cancel it, in progress or pending.
func TestWorkflowConcurrency(t *testing.T) {
	const prKey = "github.event.pull_request.number || github.ref"
	const tagSafe = "${{ !startsWith(github.ref, 'refs/tags/') }}"
	paths, err := filepath.Glob("../../.github/workflows/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no workflows found")
	}
	for _, path := range paths {
		name := filepath.Base(path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// `on` as a map, so a bare `pull_request:` (a null value) still
		// counts as present.
		var workflow struct {
			On          map[string]yaml.Node `yaml:"on"`
			Concurrency struct {
				Group            string `yaml:"group"`
				CancelInProgress any    `yaml:"cancel-in-progress"`
			} `yaml:"concurrency"`
		}
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var push struct {
			Tags []string `yaml:"tags"`
		}
		if node, ok := workflow.On["push"]; ok {
			if err := node.Decode(&push); err != nil {
				t.Fatalf("%s: on.push: %v", name, err)
			}
		}
		group := workflow.Concurrency.Group
		cancel := fmt.Sprint(workflow.Concurrency.CancelInProgress)
		if _, ok := workflow.On["pull_request"]; ok && strings.Contains(group, "github.ref") && !strings.Contains(group, prKey) {
			t.Errorf("%s: concurrency group %q keys a pull request's runs on github.ref; key them on %q", name, group, prKey)
		}
		if len(push.Tags) > 0 {
			if cancel != "false" && cancel != tagSafe {
				t.Errorf("%s: a tag runs it, but cancel-in-progress is %q; want false or %q", name, cancel, tagSafe)
			}
			// A newer run replaces a pending one whatever
			// cancel-in-progress says, so each tag needs a group of its own.
			if !strings.Contains(group, "github.ref") {
				t.Errorf("%s: a tag runs it, but its concurrency group %q is not per ref", name, group)
			}
		}
	}
}

// CI cancels every superseded run, main's included: only main's newest
// commit needs checking, and a run waiting on a macOS runner must not hold
// main's group while the pushes behind it are replaced unrun (#1272). That is
// safe only because no tag runs it.
func TestCICancelsSupersededRuns(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On struct {
			Push struct {
				Tags []string `yaml:"tags"`
			} `yaml:"push"`
		} `yaml:"on"`
		Concurrency struct {
			Group            string `yaml:"group"`
			CancelInProgress any    `yaml:"cancel-in-progress"`
		} `yaml:"concurrency"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	// An ignored label run must leave both active and pending CI alone.
	if want := "ci-${{ (github.event.action == 'labeled' && github.event.label.name != 'macos') && format('ignored-{0}', github.run_id) || github.event.pull_request.number || github.ref }}"; workflow.Concurrency.Group != want {
		t.Errorf("concurrency group %q, want %q", workflow.Concurrency.Group, want)
	}
	if got := fmt.Sprint(workflow.Concurrency.CancelInProgress); got != "true" {
		t.Errorf("cancel-in-progress %q, want true", got)
	}
	if len(workflow.On.Push.Tags) > 0 {
		t.Errorf("ci.yml runs on tags %v, and cancels in progress", workflow.On.Push.Tags)
	}
}

// A main build publishes, so it is never canceled in progress, and its runs
// share one group so a newer green commit replaces a pending build rather than
// queueing behind it. Only a run that builds joins that group: CI's own
// canceled runs on main complete too, and in the shared group each would
// replace a pending build and then skip it.
func TestMainBuildCoalescesOnlyBuildingRuns(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/main-build.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On struct {
			WorkflowRun struct {
				Workflows []string `yaml:"workflows"`
				Branches  []string `yaml:"branches"`
			} `yaml:"workflow_run"`
		} `yaml:"on"`
		Concurrency struct {
			Group            string `yaml:"group"`
			CancelInProgress any    `yaml:"cancel-in-progress"`
		} `yaml:"concurrency"`
		Jobs map[string]struct {
			If string `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(workflow.Concurrency.CancelInProgress); got != "false" {
		t.Errorf("cancel-in-progress %q, want false", got)
	}
	group := workflow.Concurrency.Group
	const gate = "github.event.workflow_run.conclusion == 'success' && github.event.workflow_run.event == 'push'"
	// A dispatch from another branch builds nothing, so it must not join
	// the group either.
	const dispatch = "github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main'"
	for _, want := range []string{gate, dispatch, "'main-build'", "github.run_id"} {
		if !strings.Contains(group, want) {
			t.Errorf("concurrency group %q lacks %q", group, want)
		}
	}
	for _, want := range []string{gate, dispatch} {
		if !strings.Contains(workflow.Jobs["version"].If, want) {
			t.Errorf("the version job's if %q does not gate on %q", workflow.Jobs["version"].If, want)
		}
	}
	if got := workflow.On.WorkflowRun; strings.Join(got.Workflows, ",") != "CI" || strings.Join(got.Branches, ",") != "main" {
		t.Errorf("runs after %v on %v, want CI on main", got.Workflows, got.Branches)
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
