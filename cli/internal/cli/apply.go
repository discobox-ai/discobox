package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/gitapply"
	"github.com/obot-platform/discobox/cli/internal/sandboxapply"
	"github.com/obot-platform/discobox/internal/gitutil"
	"github.com/obot-platform/discobox/internal/hostid"
)

// newApplyCommand implements `disco apply` (ADR 0014): pulling a sandbox's
// committed source changes into the local repository they started from, via
// fetch + cherry-pick, never merge.
func (a *App) newApplyCommand() *cobra.Command {
	var sourceSlug string
	var dirOverrides []string
	cmd := &cobra.Command{
		Use:   "apply [SANDBOX_ID] [flags]",
		Short: "Apply a sandbox's committed source changes onto a local working tree",
		Long: `Fetch a sandbox's source commits and cherry-pick them onto the local working
tree they started from, per docs/adr/0014.

"local" is this machine — the repository you ran disco from; "sandbox" is the
copy of it the sandbox works in. Commits only ever move sandbox -> local.

Without SANDBOX_ID the sandbox is taken from the ones "disco ls" shows for the
current project directory: the only one when there is one, otherwise you are
asked to pick.

Every source on the sandbox is applied by default (the primary source plus any
secondary ones); --source narrows to one, named by its slug.

Each source is only applied into the local directory the sandbox knows it came
from, on this machine. A source with no known local directory here (a sandbox
created on a different machine, or a remote-cloned source) needs an explicit
--dir slug=path.

Uncommitted changes in the sandbox are never applied: only what has been
committed there. If cherry-picking a source's commits does not apply cleanly,
nothing about the local repository changes; the commands to reproduce and
resolve it manually are printed instead.

Every source reports both repositories, the base commit the range is taken
from and why, each commit being applied, and the local commit each one
became. Use -o json for the same report as a machine-readable object, and
--debug to additionally echo every git command as it runs.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			var sandboxArg string
			if len(args) > 0 {
				sandboxArg = args[0]
			}
			overrides, err := parseDirOverrides(dirOverrides)
			if err != nil {
				return err
			}
			return a.runApply(cmd, sandboxArg, sourceSlug, overrides)
		},
	}
	cmd.Flags().StringVar(&sourceSlug, "source", "", "Apply only the source with this slug, instead of every source on the sandbox")
	cmd.Flags().StringArrayVar(&dirOverrides, "dir", nil, "Local directory to apply a source into, as slug=path; required for a source with no known local directory on this machine")
	return cmd
}

func parseDirOverrides(values []string) (map[string]string, error) {
	out := make(map[string]string, len(values))
	for _, value := range values {
		slug, dir, ok := strings.Cut(value, "=")
		slug, dir = strings.TrimSpace(slug), strings.TrimSpace(dir)
		if !ok || slug == "" || dir == "" {
			return nil, fmt.Errorf("--dir must be in slug=path form, got %q", value)
		}
		out[slug] = dir
	}
	return out, nil
}

func (a *App) runApply(cmd *cobra.Command, sandboxArg, onlySlug string, dirOverrides map[string]string) error {
	ctx := cmd.Context()
	projectID, sandboxID, client, err := a.selectSandbox(cmd, sandboxArg)
	if err != nil {
		return err
	}
	sandboxRes, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return err
	}
	sandbox, err := expectResponse[apimodel.Sandbox](sandboxRes)
	if err != nil {
		return err
	}
	sources := applySources(sandbox)
	if onlySlug != "" {
		filtered := sources[:0]
		for _, s := range sources {
			if s.slug == onlySlug {
				filtered = append(filtered, s)
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("sandbox has no source with slug %q", onlySlug)
		}
		sources = filtered
	}
	if len(sources) == 0 {
		return fmt.Errorf("sandbox has no sources to apply")
	}

	host, err := hostid.Get()
	if err != nil {
		return err
	}

	gitServerURL, releaseGitServerURL, err := a.gitServerURL(ctx)
	if err != nil {
		return err
	}
	defer releaseGitServerURL()

	// --debug echoes the literal git commands, on stderr so it never
	// interleaves into a JSON report or a piped listing.
	if a.debug {
		stderr := cmd.ErrOrStderr()
		ctx = gitutil.WithTracer(ctx, func(dir string, args []string) {
			fmt.Fprintf(stderr, "+ git -C %s %s\n", dir, strings.Join(args, " "))
		})
	}

	out := cmd.OutOrStdout()
	printer := applyPrinter{out: out, on: a.output != "json"}
	report := applyReport{SandboxID: sandboxID, SandboxName: sandbox.Config.Name}
	printer.sandboxHeader(report, len(sources))
	for _, s := range sources {
		report.Sources = append(report.Sources, a.applyOneSource(ctx, printer, client, projectID, sandboxID, sandbox, host, gitServerURL, s, dirOverrides))
	}
	printer.summary(report)
	if a.output == "json" {
		if err := writeJSON(out, report); err != nil {
			return err
		}
	}

	// Per-source detail is already printed above, as each source is processed.
	// What is left for the error is the verdict: which sources ended badly and
	// how, in one line that lands last and is unmistakably a failure rather
	// than routine progress.
	var failures []string
	for _, source := range report.Sources {
		switch source.Status {
		case applyStatusApplied, applyStatusUpToDate:
		default:
			failures = append(failures, fmt.Sprintf("%s (%s)", source.Slug, source.Status))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d sources did not apply: %s", len(failures), len(sources), strings.Join(failures, ", "))
	}
	return nil
}

type applySourceEntry struct {
	slug   string
	source apimodel.GitSource
}

// applySources collects every source on the sandbox — the primary plus every
// secondary one — each already carrying a slug, since DefaultGitSourceSlugs
// assigns one to every source at create.
func applySources(sandbox *apimodel.Sandbox) []applySourceEntry {
	var out []applySourceEntry
	if source, ok := sandbox.Config.Source.Get(); ok {
		if slug, ok := source.Slug.Get(); ok && slug != "" {
			out = append(out, applySourceEntry{slug: slug, source: source})
		}
	}
	if refs, ok := sandbox.Config.SourceCodeReferences.Get(); ok {
		keys := make([]string, 0, len(refs))
		for key := range refs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			source := refs[key]
			if slug, ok := source.Slug.Get(); ok && slug != "" {
				out = append(out, applySourceEntry{slug: slug, source: source})
			}
		}
	}
	return out
}

// sourceWorkdir is the directory a source's working tree lives at inside the
// sandbox: its explicit working directory, or the directory it was placed in.
// Empty means the sandbox never told us where the source landed.
func sourceWorkdir(source apimodel.GitSource) string {
	dest, ok := source.Destination.Get()
	if !ok {
		return ""
	}
	if wd, ok := dest.WorkingDirectory.Get(); ok {
		return wd
	}
	if dir, ok := dest.Directory.Get(); ok {
		return dir
	}
	return ""
}

// lastAppliedCommit returns the sandbox-side commit named by the most recent
// AppliedSourceCommit for slug, narrowing what a repeat apply needs to send
// per ADR 0014 §2.
func lastAppliedCommit(sandbox *apimodel.Sandbox, slug string) string {
	entries, ok := sandbox.Runtime.AppliedCommits.Get()
	if !ok {
		return ""
	}
	var latest string
	var latestAt time.Time
	for _, entry := range entries {
		if entry.Slug != slug {
			continue
		}
		if latest == "" || entry.AppliedAt.After(latestAt) {
			latest, latestAt = entry.Commit, entry.AppliedAt
		}
	}
	return latest
}

// applyOneSource applies one source and accounts for everything it did along
// the way. It never returns an error: every outcome, failure included, is a
// status on the returned report, so the caller renders them all the same way
// and no failure loses the context the run had already established.
func (a *App) applyOneSource(ctx context.Context, printer applyPrinter, client *apiclientgen.Client, projectID, sandboxID string, sandbox *apimodel.Sandbox, hostID, gitServerURL string, entry applySourceEntry, dirOverrides map[string]string) applySourceReport {
	report := applySourceReport{Slug: entry.slug, Status: applyStatusError}
	fail := func(format string, args ...any) applySourceReport {
		report.Status = applyStatusError
		report.Error = fmt.Sprintf(format, args...)
		printer.step("ERROR: %s", report.Error)
		return report
	}

	hostDir, err := resolveApplyHostDir(sandbox, hostID, entry, dirOverrides)
	if err != nil {
		printer.bareSourceHeader(entry.slug)
		return fail("%v", err)
	}
	repoRoot, err := gitutil.Root(ctx, hostDir)
	if err != nil {
		printer.bareSourceHeader(entry.slug)
		return fail("%s is not a Git repository: %v", hostDir, err)
	}
	report.HostPath = repoRoot
	report.HostBranch, _ = gitutil.CurrentBranch(ctx, repoRoot)
	if head, err := gitutil.ResolveCommit(ctx, repoRoot, "HEAD"); err == nil {
		report.HostBase = head
	}
	report.SandboxDir = sourceWorkdir(entry.source)
	report.SandboxRef = sandboxapply.FetchRef(sandboxID, entry.slug)
	// The repositories on both ends are named before anything is done to
	// them, so a slow fetch or a failure that follows already has its context
	// on screen.
	printer.sourceHeader(report)

	if report.SandboxDir != "" {
		printer.step("checking the sandbox working tree (git status --porcelain in %s)", report.SandboxDir)
		dirty, status, err := a.sandboxSourceDirty(ctx, projectID, sandboxID, report.SandboxDir)
		if err != nil {
			return fail("check sandbox working tree: %v", err)
		}
		if dirty {
			report.Status = applyStatusBlocked
			report.UncommittedChanges = statusLines(status)
			report.NextSteps = []string{
				fmt.Sprintf("disco exec --sandbox-id %s -- git -C %s commit -a -m MESSAGE", sandboxID, report.SandboxDir),
				fmt.Sprintf("disco apply %s --source %s", sandboxID, entry.slug),
			}
			printer.step("BLOCKED: the sandbox has %d uncommitted %s; only committed work is applied",
				len(report.UncommittedChanges), pluralize("change", len(report.UncommittedChanges)))
			for _, line := range report.UncommittedChanges {
				printer.detail("%s", line)
			}
			printer.step("commit them in the sandbox, then apply again:")
			printer.nextSteps(report.NextSteps)
			return report
		}
		printer.detail("clean, nothing uncommitted")
	}

	printer.step("fetching the sandbox's commits into %s", report.SandboxRef)
	tip, err := sandboxapply.FetchSource(ctx, repoRoot, gitServerURL, projectID, sandboxID, a.token, entry.source)
	if err != nil {
		return fail("%v", err)
	}
	report.SandboxTip = tip
	printer.detail("sandbox tip %s", shortSHA(tip))

	report.Base, report.BaseOrigin = lastAppliedCommit(sandbox, entry.slug), baseOriginLastApplied
	if report.Base == "" {
		report.BaseOrigin = baseOriginMergeBase
		report.Base, err = gitapply.MergeBase(ctx, repoRoot, tip)
		if err != nil {
			return fail("%v", err)
		}
	}
	printer.step("base %s (%s)", shortSHA(report.Base), formatBaseOrigin(report.BaseOrigin))

	if report.Base == tip {
		report.Status = applyStatusUpToDate
		printer.step("UP TO DATE: the sandbox has no commits after %s, so local %s is unchanged", shortSHA(report.Base), applyTarget(report))
		return report
	}

	commits, err := gitutil.Log(ctx, repoRoot, report.Base+".."+tip)
	if err != nil {
		return fail("%v", err)
	}
	report.Commits = applyCommits(commits)
	printer.step("%d %s to apply (git log %s..%s):", len(report.Commits), pluralize("commit", len(report.Commits)), shortSHA(report.Base), report.SandboxRef)
	printer.commitList(report.Commits)

	printer.step("cherry-picking them in a scratch worktree, then fast-forwarding local %s", applyTarget(report))
	result, err := gitapply.Attempt(ctx, repoRoot, report.Base, tip)
	if err != nil {
		return fail("%v", err)
	}
	if result.HostBase != "" {
		report.HostBase = result.HostBase
	}
	if !result.Landed {
		report.Status = applyStatusConflict
		report.ConflictCommit = result.ConflictCommit
		// The fetch above already landed the ref locally in repoRoot, so no
		// further "git fetch" is needed here — only the cherry-pick that
		// actually reproduces the conflict.
		report.NextSteps = []string{
			fmt.Sprintf("git -C %s cherry-pick %s..%s", repoRoot, shortSHA(report.Base), report.SandboxRef),
		}
		printer.step("CONFLICT: %s%s did not apply cleanly", shortSHA(report.ConflictCommit), quoteSubject(commitSubject(report.Commits, report.ConflictCommit)))
		printer.step("nothing in %s changed; local %s is still at %s", repoRoot, applyTarget(report), shortSHA(report.HostBase))
		printer.step("reproduce and resolve it there directly:")
		printer.nextSteps(report.NextSteps)
		return report
	}
	report.HostTip = result.HostTip
	if hostCommits, err := gitutil.Log(ctx, repoRoot, result.HostBase+".."+result.HostTip); err == nil {
		report.Commits = pairHostCommits(report.Commits, hostCommits)
	}

	if _, err := client.CompleteSandboxApply(ctx, &apimodel.CompleteSandboxApplyBody{
		Slug:       entry.slug,
		Commit:     tip,
		HostCommit: result.HostTip,
		HostId:     hostID,
		HostPath:   repoRoot,
	}, apiclientgen.CompleteSandboxApplyParams{ProjectId: projectID, SandboxId: sandboxID}); err != nil {
		// The commits really are on the branch; only the server-side record of
		// them failed. Say both, so nobody reads this as "nothing happened"
		// and re-runs expecting a clean slate.
		return fail("commits landed in %s at %s, but recording the apply on the sandbox failed: %v", repoRoot, shortSHA(result.HostTip), err)
	}

	report.Status = applyStatusApplied
	printer.step("APPLIED %d %s to local %s: %s -> %s", len(report.Commits), pluralize("commit", len(report.Commits)), applyTarget(report), shortSHA(report.HostBase), shortSHA(report.HostTip))
	printer.appliedList(report.Commits)
	printer.step("recorded on sandbox %s as applied to %s", sandboxID, repoRoot)
	return report
}

// applyTarget names what the commits land on locally: the branch when there is
// one, the repository path when HEAD is detached.
func applyTarget(report applySourceReport) string {
	if report.HostBranch != "" {
		return report.HostBranch
	}
	return report.HostPath + " (detached HEAD)"
}

func commitSubject(commits []applyCommit, sha string) string {
	for _, commit := range commits {
		if commit.Commit == sha {
			return commit.Subject
		}
	}
	return ""
}

// statusLines splits `git status --porcelain` output into its entries, keeping
// the two-column status prefix that says what happened to each path.
func statusLines(status string) []string {
	var lines []string
	for _, line := range strings.Split(status, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimRight(line, "\r"))
		}
	}
	return lines
}

// resolveApplyHostDir picks the local directory a source applies into: an
// explicit --dir override, or the source's own LocalDirectory when this host
// is the one the sandbox was created from. Anything else has no local
// directory this command can trust.
func resolveApplyHostDir(sandbox *apimodel.Sandbox, hostID string, entry applySourceEntry, dirOverrides map[string]string) (string, error) {
	if dir, ok := dirOverrides[entry.slug]; ok {
		return dir, nil
	}
	origin, hasOrigin := sandbox.Origin.Get()
	local, hasLocal := entry.source.LocalDirectory.Get()
	if hasOrigin && hasLocal && origin.HostId == hostID && strings.TrimSpace(local) != "" {
		return local, nil
	}
	return "", fmt.Errorf("no known local directory on this machine; pass --dir %s=PATH", entry.slug)
}

// sandboxSourceDirty reports whether a source's working tree inside the
// sandbox has uncommitted changes, via `git status --porcelain` over the exec
// API. Fetching a source's commits only sees committed history (ADR 0014
// §3): this is the one thing that requires exec instead.
func (a *App) sandboxSourceDirty(ctx context.Context, projectID, sandboxID, workdir string) (bool, string, error) {
	body := &apimodel.CreateSandboxExecRequest{}
	body.SetCommand([]string{"git", "status", "--porcelain"})
	body.SetWorkdir(optString(workdir))
	exec, err := a.createSandboxExec(ctx, projectID, sandboxID, body)
	if err != nil {
		return false, "", err
	}
	if _, err := a.startSandboxExec(ctx, projectID, sandboxID, exec.ID); err != nil {
		return false, "", err
	}
	final, err := a.waitSandboxExecExit(ctx, projectID, sandboxID, exec.ID)
	if err != nil {
		return false, "", err
	}
	stdout, err := a.sandboxExecStdout(ctx, projectID, sandboxID, exec.ID)
	if err != nil {
		return false, "", err
	}
	if code, ok := final.ExitCode.Get(); !ok || code != 0 {
		return false, "", fmt.Errorf("git status: %s", strings.TrimSpace(stdout))
	}
	return strings.TrimSpace(stdout) != "", stdout, nil
}

// waitSandboxExecExitPollInterval paces polling an exec's status. git status
// on a source's working tree is near-instant, so a short interval keeps
// disco apply responsive without hammering the API.
const waitSandboxExecExitPollInterval = 150 * time.Millisecond

func (a *App) waitSandboxExecExit(ctx context.Context, projectID, sandboxID, execID string) (apimodel.SandboxExec, error) {
	for {
		exec, err := a.getSandboxExec(ctx, projectID, sandboxID, execID)
		if err != nil {
			return apimodel.SandboxExec{}, err
		}
		switch exec.Status {
		case apiclientgen.SandboxExecStatusExited, apiclientgen.SandboxExecStatusFailed, apiclientgen.SandboxExecStatusLost:
			return *exec, nil
		}
		select {
		case <-ctx.Done():
			return apimodel.SandboxExec{}, ctx.Err()
		case <-time.After(waitSandboxExecExitPollInterval):
		}
	}
}

func (a *App) sandboxExecStdout(ctx context.Context, projectID, sandboxID, execID string) (string, error) {
	client, err := a.apiClient()
	if err != nil {
		return "", err
	}
	res, err := client.ListSandboxExecLogs(ctx, apiclientgen.ListSandboxExecLogsParams{ProjectId: projectID, SandboxId: sandboxID, ExecId: execID})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apimodel.SandboxExecLogsResponse](res)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, entry := range body.GetEntries() {
		if entry.Stream == apiclientgen.SandboxExecLogEntryStreamInput {
			continue
		}
		out.Write(entry.Data)
	}
	return out.String(), nil
}

func shortSHA(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
