package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/gitapply"
	"github.com/discobox-ai/discobox/cli/internal/sandboxapply"
	"github.com/discobox-ai/discobox/internal/gitutil"
	"github.com/discobox-ai/discobox/internal/hostid"
)

// newApplyCommand implements `discobox apply` (ADR 0014): pulling a sandbox's
// committed source changes into the local repository they started from, via
// fetch + cherry-pick, never merge.
func (a *App) newApplyCommand() *cobra.Command {
	var sourceSlug string
	var dirOverrides []string
	var allowDirty bool
	cmd := &cobra.Command{
		Use:   "apply [DISCOBOX_ID] [flags]",
		Short: "Apply a discobox's committed source changes onto a local working tree",
		Long: `Fetch a discobox's source commits and cherry-pick them onto the local working
tree they started from, per docs/adr/0014.

"local" is this machine — the repository you ran discobox from; "discobox" is the
copy of it the discobox works in. Commits only ever move discobox -> local.

Without DISCOBOX_ID the discobox is taken from the ones "discobox ls" shows for the
current project directory: the only one when there is one, otherwise you are
asked to pick.

Every source on the discobox is applied by default (the primary source plus any
secondary ones); --source narrows to one, named by its slug.

Each source is only applied into the local directory the discobox knows it came
from, on this machine. A source with no known local directory here (a discobox
created on a different machine, or a remote-cloned source) needs an explicit
--dir slug=path.

Uncommitted changes in the discobox are never applied: only what has been
committed there. A source whose discobox working tree is dirty is reported and
skipped, so nothing lands from a half-finished state by accident.
--allow-dirty applies that source's committed commits anyway and leaves the
uncommitted ones where they are.

If cherry-picking a source's commits does not apply cleanly, nothing about the
local repository changes; the commands to reproduce and resolve it manually are
printed instead.

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
			return a.runApply(cmd, sandboxArg, sourceSlug, overrides, allowDirty)
		},
	}
	cmd.Flags().StringVar(&sourceSlug, "source", "", "Apply only the source with this slug, instead of every source on the discobox")
	cmd.Flags().StringArrayVar(&dirOverrides, "dir", nil, "Local directory to apply a source into, as slug=path; required for a source with no known local directory on this machine")
	cmd.Flags().BoolVar(&allowDirty, "allow-dirty", false, "Apply a source's committed commits even when the discobox has uncommitted changes; they stay in the discobox either way")
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

func (a *App) runApply(cmd *cobra.Command, sandboxArg, onlySlug string, dirOverrides map[string]string, allowDirty bool) error {
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
	sources, err := selectSources(sandbox, onlySlug)
	if err != nil {
		return err
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
		report.Sources = append(report.Sources, a.applyOneSource(ctx, printer, client, projectID, sandboxID, sandbox, host, gitServerURL, s, dirOverrides, allowDirty))
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

// selectSources is the set of sources a command acts on: every source on the
// sandbox, or only the one named by slug. Shared by the commands that work
// per-source (apply, diff) so "--source" means the same thing in each.
func selectSources(sandbox *apimodel.Sandbox, onlySlug string) ([]applySourceEntry, error) {
	sources := applySources(sandbox)
	if onlySlug != "" {
		filtered := sources[:0]
		for _, s := range sources {
			if s.slug == onlySlug {
				filtered = append(filtered, s)
			}
		}
		if len(filtered) == 0 {
			return nil, fmt.Errorf("discobox has no source with slug %q", onlySlug)
		}
		sources = filtered
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("discobox has no sources")
	}
	return sources, nil
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

// lastApplied returns the most recent AppliedSourceCommit for slug: its
// sandbox-side commit narrows what a repeat apply needs to send per ADR 0014
// §2, and its host-side commit is the SHA a listing shows for an applied
// sandbox.
func lastApplied(sandbox *apimodel.Sandbox, slug string) (apimodel.AppliedSourceCommit, bool) {
	entries, ok := sandbox.Runtime.AppliedCommits.Get()
	if !ok {
		return apimodel.AppliedSourceCommit{}, false
	}
	var latest apimodel.AppliedSourceCommit
	found := false
	for _, entry := range entries {
		if entry.Slug != slug {
			continue
		}
		if !found || entry.AppliedAt.After(latest.AppliedAt) {
			latest, found = entry, true
		}
	}
	return latest, found
}

// applyOneSource applies one source and accounts for everything it did along
// the way. It never returns an error: every outcome, failure included, is a
// status on the returned report, so the caller renders them all the same way
// and no failure loses the context the run had already established.
func (a *App) applyOneSource(ctx context.Context, printer applyPrinter, client *apiclientgen.Client, projectID, sandboxID string, sandbox *apimodel.Sandbox, hostID, gitServerURL string, entry applySourceEntry, dirOverrides map[string]string, allowDirty bool) applySourceReport {
	report := applySourceReport{Slug: entry.slug, Status: applyStatusError}
	fail := func(format string, args ...any) applySourceReport {
		report.Status = applyStatusError
		report.Error = fmt.Sprintf(format, args...)
		printer.step("ERROR: %s", report.Error)
		return report
	}

	hostDir, dirOrigin, err := resolveApplyHostDir(sandbox, hostID, entry, dirOverrides)
	if err != nil {
		printer.bareSourceHeader(entry.slug)
		return fail("%v", err)
	}
	report.HostPathOrigin = dirOrigin
	repoRoot, err := gitutil.Root(ctx, hostDir)
	if errors.Is(err, gitutil.ErrNotARepository) {
		printer.bareSourceHeader(entry.slug)
		return fail("%s is not a Git repository", hostDir)
	}
	if err != nil {
		printer.bareSourceHeader(entry.slug)
		return fail("read the Git repository at %s: %v", hostDir, err)
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
		printer.step("checking the discobox working tree (git status --porcelain in %s)", report.SandboxDir)
		dirty, status, err := a.sandboxSourceDirty(ctx, projectID, sandboxID, report.SandboxDir)
		if err != nil {
			return fail("check discobox working tree: %v", err)
		}
		switch {
		case dirty && !allowDirty:
			report.Status = applyStatusBlocked
			report.UncommittedChanges = statusLines(status)
			report.NextSteps = dirtyNextSteps(sandboxID, entry.slug, report.SandboxDir, dirOverrides)
			printer.step("BLOCKED: the discobox has %d uncommitted %s; only committed work is applied",
				len(report.UncommittedChanges), pluralize("change", len(report.UncommittedChanges)))
			printer.detailLines(report.UncommittedChanges)
			printer.nextSteps(report.NextSteps)
			return report
		case dirty:
			// --allow-dirty. The uncommitted work is still listed: it stays in
			// the sandbox, and the whole point of the flag is that the user
			// chose to leave it there rather than not knowing about it.
			report.UncommittedChanges = statusLines(status)
			report.DirtyIgnored = true
			printer.step("--allow-dirty: applying anyway; %d uncommitted %s stay in the discobox and are not applied",
				len(report.UncommittedChanges), pluralize("change", len(report.UncommittedChanges)))
			printer.detailLines(report.UncommittedChanges)
		default:
			printer.detail("clean, nothing uncommitted")
		}
	}

	printer.step("fetching the discobox's commits into %s", report.SandboxRef)
	tip, err := sandboxapply.FetchSource(ctx, repoRoot, gitServerURL, projectID, sandboxID, a.token, entry.source)
	if err != nil {
		return fail("%v", err)
	}
	report.SandboxTip = tip
	printer.detail("discobox tip %s", shortSHA(tip))

	if last, ok := lastApplied(sandbox, entry.slug); ok {
		report.Base, report.BaseOrigin = last.Commit, baseOriginLastApplied
	}
	if report.Base == "" {
		report.BaseOrigin = baseOriginMergeBase
		report.Base, err = gitapply.MergeBase(ctx, repoRoot, tip)
		if err != nil {
			// The overwhelmingly likely cause is that repoRoot is not the
			// repository this source came from — unrelated histories share no
			// commit — which is worth saying outright, since --dir is how a
			// caller points at the wrong one in the first place.
			return fail("the discobox's history has no commit in common with %s, so there is nothing to apply onto; is that the repository source %q came from? (%v)", repoRoot, entry.slug, err)
		}
	}
	printer.step("base %s (%s)", shortSHA(report.Base), formatBaseOrigin(report.BaseOrigin))

	if report.Base == tip {
		report.Status = applyStatusUpToDate
		printer.step("UP TO DATE: the discobox has no commits after %s, so local %s is unchanged", shortSHA(report.Base), applyTarget(report))
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
		report.NextSteps = []applyNextStep{{
			Description: fmt.Sprintf("reproduce and resolve it in %s directly", repoRoot),
			Commands:    []string{fmt.Sprintf("git -C %s cherry-pick %s..%s", repoRoot, shortSHA(report.Base), report.SandboxRef)},
		}}
		printer.step("CONFLICT: %s%s did not apply cleanly", shortSHA(report.ConflictCommit), quoteSubject(commitSubject(report.Commits, report.ConflictCommit)))
		printer.step("nothing in %s changed; local %s is still at %s", repoRoot, applyTarget(report), shortSHA(report.HostBase))
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
		return fail("commits landed in %s at %s, but recording the apply on the discobox failed: %v", repoRoot, shortSHA(result.HostTip), err)
	}

	report.Status = applyStatusApplied
	printer.step("APPLIED %d %s to local %s: %s -> %s", len(report.Commits), pluralize("commit", len(report.Commits)), applyTarget(report), shortSHA(report.HostBase), shortSHA(report.HostTip))
	printer.appliedList(report.Commits)
	printer.step("recorded on discobox %s as applied to %s", sandboxID, repoRoot)
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

// dirtyNextSteps is the two ways out of a dirty sandbox working tree: commit
// the work there and apply it too, or apply only what is already committed and
// leave the rest. Both re-runs are spelled out for this exact source, --dir
// override included, so neither has to be reassembled by hand.
func dirtyNextSteps(sandboxID, slug, sandboxDir string, dirOverrides map[string]string) []applyNextStep {
	rerun := fmt.Sprintf("discobox apply %s --source %s", sandboxID, slug)
	if dir, ok := dirOverrides[slug]; ok {
		rerun += fmt.Sprintf(" --dir %s=%s", slug, dir)
	}
	return []applyNextStep{
		{
			Description: "commit them in the discobox, then apply again",
			Commands: []string{
				fmt.Sprintf("discobox shell %s -- git -C %s commit -a -m MESSAGE", sandboxID, sandboxDir),
				rerun,
			},
		},
		{
			Description: "or apply only what is already committed, leaving them in the discobox",
			Commands:    []string{rerun + " --allow-dirty"},
		},
	}
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
// explicit --dir override, or the source's own LocalDirectory when this
// machine is the one the sandbox was created from.
//
// Applying into a directory the sandbox did not come from would cherry-pick
// one repository's commits onto another, so the identity check is the gate:
// the sandbox's origin host must be this host, that source must have recorded
// where it came from, and that directory must still be there. Each of those
// fails for a different reason and says which, because "pass --dir" is only
// actionable once you know whether the sandbox is from another machine, was
// cloned from a remote, or simply moved.
func resolveApplyHostDir(sandbox *apimodel.Sandbox, hostID string, entry applySourceEntry, dirOverrides map[string]string) (string, hostDirOrigin, error) {
	if dir, ok := dirOverrides[entry.slug]; ok {
		return dir, hostDirFromOverride, nil
	}
	needDir := fmt.Sprintf("pass --dir %s=PATH", entry.slug)

	origin, hasOrigin := sandbox.Origin.Get()
	if !hasOrigin {
		return "", "", fmt.Errorf("the discobox has no recorded origin, so nothing says which directory it came from; %s", needDir)
	}
	if origin.HostId != hostID {
		return "", "", fmt.Errorf("the discobox was created on a different machine (origin host %s%s, this machine is %s); %s",
			origin.HostId, formatOriginHostname(origin), hostID, needDir)
	}

	local := strings.TrimSpace(entry.source.LocalDirectory.Or(""))
	if local == "" {
		return "", "", fmt.Errorf("source %q has no local directory recorded — it was cloned from a remote rather than pushed from a checkout here; %s", entry.slug, needDir)
	}
	info, err := os.Stat(local)
	if err != nil {
		return "", "", fmt.Errorf("the directory source %q came from, %s, is not readable on this machine (%w); %s", entry.slug, local, err, needDir)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("the path source %q came from, %s, is no longer a directory; %s", entry.slug, local, needDir)
	}
	return local, hostDirFromSandboxOrigin, nil
}

// formatOriginHostname adds the origin machine's display hostname when the
// sandbox recorded one. The host ID alone identifies the machine but tells
// nobody which machine it was.
func formatOriginHostname(origin apimodel.Origin) string {
	if hostname := strings.TrimSpace(origin.Hostname.Or("")); hostname != "" {
		return " " + strconv.Quote(hostname)
	}
	return ""
}

// sandboxSourceDirty reports whether a source's working tree inside the
// sandbox has uncommitted changes, via `git status --porcelain` over the exec
// API. Fetching a source's commits only sees committed history (ADR 0014
// §3): this is the one thing that requires exec instead.
func (a *App) sandboxSourceDirty(ctx context.Context, projectID, sandboxID, workdir string) (bool, string, error) {
	stdout, stderr, code, err := a.sandboxCommandOutput(ctx, projectID, sandboxID, workdir, []string{"git", "status", "--porcelain"})
	if err != nil {
		return false, "", err
	}
	if code != 0 {
		return false, "", fmt.Errorf("git status: %s", strings.TrimSpace(stderr+stdout))
	}
	return strings.TrimSpace(stdout) != "", stdout, nil
}

func shortSHA(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
