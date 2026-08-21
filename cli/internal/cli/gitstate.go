package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apimodel "github.com/obot-platform/discobox/api/model"
)

// sandboxGitState is the sandbox's current git position, as its own agent last
// reported it: which commit the primary source's working tree is sitting on,
// and whether anything there is uncommitted. It is derived, never fetched — the
// sandbox-agent pushes it through the pool agent (ADR 0030), so a listing can
// show it for every row without a git invocation anywhere.
type sandboxGitState struct {
	// Known separates "the agent has reported" from "nothing has reported
	// yet". A stopped or just-created sandbox has no state to show, which is
	// not the same as a clean one.
	Known bool

	Branch string
	Commit string // head, full SHA
	Clean  bool   // no uncommitted changes in the working tree

	// Applied: the head commit is the last commit `discobox apply` recorded for
	// this source, so everything committed here has landed on a host. Only a
	// clean tree can be applied — dirty content is by definition not.
	Applied bool

	// AppliedHostCommit is the host-side commit that apply produced, full SHA.
	// Cherry-picking onto a different parent always mints a new object, so this
	// — not the sandbox head — is the SHA findable in the local repository. Set
	// only when Applied.
	AppliedHostCommit string

	// The diff stat the agent measured: what the sandbox holds against the
	// spawn commit, forwarded to the merge base with upstream once the
	// sandbox has fetched so pulled commits do not count (ADR 0018's rule,
	// per ADR 0037) — committed and uncommitted tracked changes both, as
	// `git diff --shortstat` counts them. DiffKnown separates "nothing
	// changed" from a report that carried no stat (no base commit recorded,
	// or one the repository does not have).
	DiffKnown   bool
	DiffFiles   int
	DiffAdded   int
	DiffDeleted int

	ObservedAt time.Time
}

// changes is the one-word answer to "does this sandbox hold work I could
// lose": the working tree first, then the committed history against where it
// started and what has been applied.
//
//   - "dirty":     uncommitted changes in the working tree
//   - "applied":   clean, and the head commit was the last one applied to a host
//   - "ahead":     clean, with commits the sandbox made that no apply has landed
//   - "clean":     clean and still on the commit it was spawned from
//   - "-":         no agent has reported yet
//
// spawnCommit is the commit the sandbox was cut from, which is what separates
// "clean because untouched" from "clean because committed".
func (g sandboxGitState) changes(spawnCommit string) string {
	switch {
	case !g.Known:
		return "-"
	case !g.Clean:
		return "dirty"
	case g.Applied:
		return "applied"
	case g.Commit != "" && g.Commit != spawnCommit:
		return "ahead"
	default:
		return "clean"
	}
}

// position is the branch@commit the sandbox is sitting on, in the launcher's
// spelling, starred when the working tree holds uncommitted content. An
// applied sandbox shows the host-side commit its apply produced instead of its
// own head: everything here has landed, so the useful SHA is the one findable
// in the local repository.
func (g sandboxGitState) position() string {
	if !g.Known || (g.Branch == "" && g.Commit == "") {
		return ""
	}
	commit := g.Commit
	if g.Applied && g.AppliedHostCommit != "" {
		commit = g.AppliedHostCommit
	}
	out := g.Branch + "@" + shortCommit(commit)
	if !g.Clean {
		out += "*"
	}
	return out
}

// diffColumn is the diff stat as a table cell: what the sandbox holds against
// the commit it was spawned at, in the launcher's spelling. "-" when no stat
// was reported, empty when one was and nothing has changed — the CHANGES
// column already carries that answer as a word.
func (g sandboxGitState) diffColumn() string {
	if !g.DiffKnown {
		return "-"
	}
	if g.DiffFiles == 0 {
		return ""
	}
	return fmt.Sprintf("+%d -%d", g.DiffAdded, g.DiffDeleted)
}

// sandboxGitStatus reads the reported git state for the sandbox's primary
// source. The primary source is what a row is about — a sandbox with several
// is rare, and a row cannot show a column per source — which is the same call
// the launcher's diffstat already makes.
func sandboxGitStatus(sb apimodel.Sandbox) sandboxGitState {
	source, ok := sb.Config.Source.Get()
	if !ok {
		return sandboxGitState{}
	}
	slug := strings.TrimSpace(source.Slug.Or(""))
	status, ok := agentGitSourceStatus(sb, slug)
	if !ok {
		return sandboxGitState{}
	}
	state := sandboxGitState{
		Known:      true,
		Branch:     strings.TrimSpace(status.Branch.Or("")),
		Commit:     strings.TrimSpace(status.HeadCommit.Or("")),
		Clean:      status.Clean,
		ObservedAt: status.ObservedAt,
	}
	if strings.TrimSpace(status.DiffBase.Or("")) != "" {
		state.DiffKnown = true
		state.DiffFiles = int(status.DiffFiles.Or(0))
		state.DiffAdded = int(status.DiffAdded.Or(0))
		state.DiffDeleted = int(status.DiffDeleted.Or(0))
	}
	if applied, ok := lastApplied(&sb, slug); ok && state.Clean && state.Commit == applied.Commit {
		state.Applied = true
		state.AppliedHostCommit = strings.TrimSpace(applied.HostCommit)
	}
	return state
}

// agentGitSourceStatus picks slug's entry out of the agent-status payload. The
// payload is stored as the sandbox-agent sent it — an opaque map to the API —
// so the sources are decoded here into the schema type they were sent as. An
// entry that reports an observation error carries no state worth a row.
func agentGitSourceStatus(sb apimodel.Sandbox, slug string) (apimodel.SandboxAgentGitSourceStatus, bool) {
	agentStatus, ok := sb.Runtime.AgentStatus.Get()
	if !ok {
		return apimodel.SandboxAgentGitSourceStatus{}, false
	}
	raw, ok := agentStatus["sources"]
	if !ok {
		return apimodel.SandboxAgentGitSourceStatus{}, false
	}
	var sources []apimodel.SandboxAgentGitSourceStatus
	if err := json.Unmarshal(raw, &sources); err != nil {
		return apimodel.SandboxAgentGitSourceStatus{}, false
	}
	for _, source := range sources {
		if source.Slug != slug {
			continue
		}
		if strings.TrimSpace(source.Error.Or("")) != "" {
			return apimodel.SandboxAgentGitSourceStatus{}, false
		}
		return source, true
	}
	return apimodel.SandboxAgentGitSourceStatus{}, false
}

// sandboxSpawnCommit is the commit the primary source was cut from.
func sandboxSpawnCommit(sb apimodel.Sandbox) string {
	source, ok := sb.Config.Source.Get()
	if !ok {
		return ""
	}
	return sourceCheckoutCommit(source)
}

// sourceCheckoutCommit is the commit a source was cloned at, recorded on the
// sandbox at create. It is the sandbox's own starting point — including work
// carried in from a dirty local workspace, which arrives as uncommitted changes
// on top of exactly this commit.
func sourceCheckoutCommit(source apimodel.GitSource) string {
	checkout, ok := source.Checkout.Get()
	if !ok {
		return ""
	}
	return strings.TrimSpace(checkout.Commit.Or(""))
}

// sourceSnapshotRef is the ref holding the uncommitted work the sandbox was
// handed at create, for a source created from a dirty local workspace. Empty
// when the workspace was clean, which is when there is nothing to carry.
func sourceSnapshotRef(source apimodel.GitSource) string {
	workspace, ok := source.Workspace.Get()
	if !ok {
		return ""
	}
	if mode, ok := workspace.Mode.Get(); !ok || string(mode) != "dirty" {
		return ""
	}
	return strings.TrimSpace(workspace.SnapshotRef.Or(""))
}
