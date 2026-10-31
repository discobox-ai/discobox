package cli

import (
	"fmt"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// gitStateSandbox builds a sandbox whose primary source is "code", spawned at
// spawnCommit, with an optional agent report.
func gitStateSandbox(t *testing.T, spawnCommit, agentSources string) apimodel.Sandbox {
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
	return sb
}

const (
	spawnSHA  = "1111111111111111111111111111111111111111"
	headSHA   = "2222222222222222222222222222222222222222"
	sourceFmt = `[{"slug":"code","target":"/workspace","clean":%s,"branch":"main","headCommit":"%s","observedAt":"2026-08-12T00:00:00Z"}]`
)

func TestSandboxGitStatusChanges(t *testing.T) {
	cases := []struct {
		name    string
		sandbox apimodel.Sandbox
		want    string
	}{
		{
			name:    "no report yet",
			sandbox: gitStateSandbox(t, spawnSHA, ""),
			want:    "-",
		},
		{
			name:    "dirty tree",
			sandbox: gitStateSandbox(t, spawnSHA, sprintfSources("false", headSHA)),
			want:    "dirty",
		},
		{
			name:    "clean on the spawn commit",
			sandbox: gitStateSandbox(t, spawnSHA, sprintfSources("true", spawnSHA)),
			want:    "clean",
		},
		{
			name:    "ready: committed ahead of spawn",
			sandbox: gitStateSandbox(t, spawnSHA, sprintfSources("true", headSHA)),
			want:    "ready",
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
		`[{"slug":"code","target":"/workspace","clean":true,"error":"not a git repository","observedAt":"2026-08-12T00:00:00Z"}]`)
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
		`[{"slug":"code","target":"/workspace","clean":false,"branch":"main","headCommit":"`+headSHA+`","diffBase":"`+spawnSHA+`","diffFiles":3,"diffAdded":61,"diffDeleted":12,"observedAt":"2026-08-12T00:00:00Z"}]`)
	state := sandboxGitStatus(withStat)
	if !state.DiffKnown || state.DiffFiles != 3 || state.DiffAdded != 61 || state.DiffDeleted != 12 {
		t.Fatalf("diff stat = %+v, want 3 files +61 -12", state)
	}
	if got := state.diffColumn(); got != "+61 -12" {
		t.Fatalf("diff column = %q, want +61 -12", got)
	}

	noStat := gitStateSandbox(t, spawnSHA, sprintfSources("true", spawnSHA))
	state = sandboxGitStatus(noStat)
	if state.DiffKnown {
		t.Fatalf("diff stat = %+v, want unknown without a base", state)
	}
	if got := state.diffColumn(); got != "-" {
		t.Fatalf("diff column = %q, want -", got)
	}

	zeroStat := gitStateSandbox(t, spawnSHA,
		`[{"slug":"code","target":"/workspace","clean":true,"branch":"main","headCommit":"`+spawnSHA+`","diffBase":"`+spawnSHA+`","observedAt":"2026-08-12T00:00:00Z"}]`)
	state = sandboxGitStatus(zeroStat)
	if !state.DiffKnown || state.DiffFiles != 0 {
		t.Fatalf("diff stat = %+v, want known and empty", state)
	}
	if got := state.diffColumn(); got != "" {
		t.Fatalf("diff column = %q, want empty for no changes", got)
	}
}

func TestSandboxGitColumn(t *testing.T) {
	reported := gitStateSandbox(t, spawnSHA, sprintfSources("false", headSHA))
	if got := sandboxGitColumn(reported); got != "main@2222222*" {
		t.Fatalf("git column = %q, want the reported head, starred", got)
	}
	unreported := gitStateSandbox(t, spawnSHA, "")
	if got := sandboxGitColumn(unreported); got != "main@1111111" {
		t.Fatalf("git column = %q, want the spawn position", got)
	}
}
