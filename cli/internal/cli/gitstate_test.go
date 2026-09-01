package cli

import (
	"fmt"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// gitStateSandbox builds a sandbox whose primary source is "code", spawned at
// spawnCommit, with an optional agent report and applied-commit record — the
// three inputs the derivation reads.
func gitStateSandbox(t *testing.T, spawnCommit, agentSources string, applied []apimodel.AppliedSourceCommit) apimodel.Sandbox {
	t.Helper()
	source := apimodel.GitSource{
		Slug: apiclientgen.NewOptString("code"),
		Checkout: apiclientgen.NewOptGitSourceCheckout(apimodel.GitSourceCheckout{
			RefName: apiclientgen.NewOptString("main"),
			Commit:  apiclientgen.NewOptString(spawnCommit),
		}),
	}
	sb := apimodel.Sandbox{}
	sb.Config.Source = apiclientgen.NewOptGitSource(source)
	if agentSources != "" {
		status := apiclientgen.SandboxRuntimeAgentStatus{"sources": []byte(agentSources)}
		sb.Runtime.AgentStatus = apiclientgen.NewOptNilSandboxRuntimeAgentStatus(status)
	}
	if applied != nil {
		sb.Runtime.AppliedCommits = apiclientgen.NewOptNilAppliedSourceCommitArray(applied)
	}
	return sb
}

const (
	spawnSHA  = "1111111111111111111111111111111111111111"
	headSHA   = "2222222222222222222222222222222222222222"
	otherSHA  = "3333333333333333333333333333333333333333"
	hostSHA   = "4444444444444444444444444444444444444444"
	sourceFmt = `[{"slug":"code","target":"/workspace","clean":%s,"branch":"main","headCommit":"%s","observedAt":"2026-08-12T00:00:00Z"}]`
)

func TestSandboxGitStatusChanges(t *testing.T) {
	appliedHead := []apimodel.AppliedSourceCommit{{
		Slug: "code", Commit: headSHA, HostCommit: hostSHA, AppliedAt: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
	}}
	cases := []struct {
		name    string
		sandbox apimodel.Sandbox
		want    string
	}{
		{
			name:    "no report yet",
			sandbox: gitStateSandbox(t, spawnSHA, "", nil),
			want:    "-",
		},
		{
			name:    "dirty tree",
			sandbox: gitStateSandbox(t, spawnSHA, sprintfSources("false", headSHA), nil),
			want:    "dirty",
		},
		{
			name:    "clean on the spawn commit",
			sandbox: gitStateSandbox(t, spawnSHA, sprintfSources("true", spawnSHA), nil),
			want:    "clean",
		},
		{
			name:    "ready: committed but never applied",
			sandbox: gitStateSandbox(t, spawnSHA, sprintfSources("true", headSHA), nil),
			want:    "ready",
		},
		{
			name:    "clean on the applied commit",
			sandbox: gitStateSandbox(t, spawnSHA, sprintfSources("true", headSHA), appliedHead),
			want:    "applied",
		},
		{
			// Dirt on top of an applied commit is still content only the
			// sandbox holds; the tree wins over the history.
			name:    "dirty on the applied commit",
			sandbox: gitStateSandbox(t, spawnSHA, sprintfSources("false", headSHA), appliedHead),
			want:    "dirty",
		},
		{
			// Commits after the last apply are unapplied work again.
			name: "ready again past the applied commit",
			sandbox: gitStateSandbox(t, spawnSHA, sprintfSources("true", otherSHA), []apimodel.AppliedSourceCommit{{
				Slug: "code", Commit: headSHA, AppliedAt: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
			}}),
			want: "ready",
		},
		{
			// An apply recorded for a different source says nothing about this
			// one.
			name: "applied commit for another source",
			sandbox: gitStateSandbox(t, spawnSHA, sprintfSources("true", headSHA), []apimodel.AppliedSourceCommit{{
				Slug: "docs", Commit: headSHA, AppliedAt: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
			}}),
			want: "ready",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sandboxGitStatus(tc.sandbox).changes(sandboxSpawnCommit(tc.sandbox))
			if got != tc.want {
				t.Fatalf("changes = %q, want %q", got, tc.want)
			}
		})
	}
}

func sprintfSources(clean, head string) string {
	return fmt.Sprintf(sourceFmt, clean, head)
}

// A source the agent could not observe carries no state; the row falls back to
// the spawn position rather than showing a half-report.
func TestSandboxGitStatusIgnoresErroredSource(t *testing.T) {
	sb := gitStateSandbox(t, spawnSHA,
		`[{"slug":"code","target":"/workspace","clean":true,"error":"not a git repository","observedAt":"2026-08-12T00:00:00Z"}]`, nil)
	if state := sandboxGitStatus(sb); state.Known {
		t.Fatalf("state = %+v, want unknown", state)
	}
	if got := sandboxGitColumn(sb); got != "main@1111111" {
		t.Fatalf("git column = %q, want the spawn position", got)
	}
}

// The diff stat travels with its base: absent base, no stat; present base
// with zero counts is "no changes", which is an answer.
func TestSandboxGitStatusDiffStat(t *testing.T) {
	withStat := gitStateSandbox(t, spawnSHA,
		`[{"slug":"code","target":"/workspace","clean":false,"branch":"main","headCommit":"`+headSHA+`","diffBase":"`+spawnSHA+`","diffFiles":3,"diffAdded":61,"diffDeleted":12,"observedAt":"2026-08-12T00:00:00Z"}]`, nil)
	state := sandboxGitStatus(withStat)
	if !state.DiffKnown || state.DiffFiles != 3 || state.DiffAdded != 61 || state.DiffDeleted != 12 {
		t.Fatalf("diff stat = %+v, want 3 files +61 -12", state)
	}
	if got := state.diffColumn(); got != "+61 -12" {
		t.Fatalf("diff column = %q, want +61 -12", got)
	}

	noStat := gitStateSandbox(t, spawnSHA, sprintfSources("true", spawnSHA), nil)
	state = sandboxGitStatus(noStat)
	if state.DiffKnown {
		t.Fatalf("diff stat = %+v, want unknown without a base", state)
	}
	if got := state.diffColumn(); got != "-" {
		t.Fatalf("diff column = %q, want -", got)
	}

	zeroStat := gitStateSandbox(t, spawnSHA,
		`[{"slug":"code","target":"/workspace","clean":true,"branch":"main","headCommit":"`+spawnSHA+`","diffBase":"`+spawnSHA+`","observedAt":"2026-08-12T00:00:00Z"}]`, nil)
	state = sandboxGitStatus(zeroStat)
	if !state.DiffKnown || state.DiffFiles != 0 {
		t.Fatalf("diff stat = %+v, want known and empty", state)
	}
	if got := state.diffColumn(); got != "" {
		t.Fatalf("diff column = %q, want empty for no changes", got)
	}
}

func TestSandboxGitColumn(t *testing.T) {
	reported := gitStateSandbox(t, spawnSHA, sprintfSources("false", headSHA), nil)
	if got := sandboxGitColumn(reported); got != "main@2222222*" {
		t.Fatalf("git column = %q, want the reported head, starred", got)
	}
	unreported := gitStateSandbox(t, spawnSHA, "", nil)
	if got := sandboxGitColumn(unreported); got != "main@1111111" {
		t.Fatalf("git column = %q, want the spawn position", got)
	}
	// An applied sandbox shows the host-side commit its apply produced — the
	// SHA findable in the local repository — not its own head.
	applied := gitStateSandbox(t, spawnSHA, sprintfSources("true", headSHA), []apimodel.AppliedSourceCommit{{
		Slug: "code", Commit: headSHA, HostCommit: hostSHA, AppliedAt: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
	}})
	if got := sandboxGitColumn(applied); got != "main@4444444" {
		t.Fatalf("git column = %q, want the applied host commit", got)
	}
	// Commits past the last apply put the head back on the row.
	aheadAgain := gitStateSandbox(t, spawnSHA, sprintfSources("true", otherSHA), []apimodel.AppliedSourceCommit{{
		Slug: "code", Commit: headSHA, HostCommit: hostSHA, AppliedAt: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
	}})
	if got := sandboxGitColumn(aheadAgain); got != "main@3333333" {
		t.Fatalf("git column = %q, want the reported head", got)
	}
}
