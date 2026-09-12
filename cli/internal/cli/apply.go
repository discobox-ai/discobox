package cli

import (
	"context"
	"encoding/json"
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
	"github.com/discobox-ai/discobox/cli/internal/gitunborn"
	"github.com/discobox-ai/discobox/cli/internal/sandboxapply"
	"github.com/discobox-ai/discobox/internal/hostid"
	"github.com/discobox-ai/x/gitutil"
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
	printer := newApplyPrinter(out, a.output != "json")
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
	if reportPath := os.Getenv(applyReportPathEnv); reportPath != "" {
		data, err := json.Marshal(report)
		if err != nil {
			return fmt.Errorf("encode apply result: %w", err)
		}
		if err := os.WriteFile(reportPath, data, 0o600); err != nil {
			return fmt.Errorf("write apply result: %w", err)
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
		printer.outcome(applyStatusError, "ERROR: %s", report.Error)
		return report
	}

	hostDir, dirOrigin, err := resolveApplyHostDir(sandbox, hostID, entry, dirOverrides)
	if err != nil {
		return a.unplacedSource(ctx, printer, projectID, sandboxID, sandbox, gitServerURL, entry, allowDirty, err)
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
	hostUnborn := gitunborn.HeadIsUnborn(ctx, repoRoot)
	if head, err := gitutil.ResolveCommit(ctx, repoRoot, "HEAD"); err == nil {
		report.HostBase = head
	}
	report.SandboxDir = sourceWorkdir(entry.source)
	report.SandboxRef = sandboxapply.FetchRef(sandboxID, entry.slug)
	// The repositories on both ends are named before anything is done to
	// them, so a slow fetch or a failure that follows already has its context
	// on screen.
	printer.sourceHeader(report)

	blocked, err := a.sandboxDirtyBlocks(ctx, printer, projectID, sandboxID, &report, allowDirty, dirOverrides[entry.slug])
	if err != nil {
		return fail("check discobox working tree: %v", err)
	}
	if blocked {
		return report
	}

	printer.note("fetching the discobox's commits")
	tip, err := sandboxapply.FetchSource(ctx, repoRoot, gitServerURL, projectID, sandboxID, a.token, entry.source)
	if err != nil {
		return fail("%v", err)
	}
	report.SandboxTip = tip
	printer.noteDetail("discobox tip %s", shortSHA(tip))

	lastCommit := ""
	if last, ok := lastApplied(sandbox, entry.slug); ok {
		lastCommit = last.Commit
	}
	discoboxBase := ""
	if entry.source.NoLocalCommits.Or(false) {
		// The discobox was created from a repository with no commits, so it
		// starts from an empty base commit of its own and shares nothing with
		// this repository by construction — there is no merge base to look for,
		// and everything after that base is the discobox's work. This holds
		// however many commits the user has made here since (ADR 0084 §1).
		discoboxBase = checkoutCommit(entry.source)
		if discoboxBase == "" {
			return fail("source %q records no base commit to apply from", entry.slug)
		}
	}
	report.Base, report.BaseOrigin, err = resolveApplyBase(ctx, repoRoot, tip, lastCommit, discoboxBase)
	if err != nil {
		// The overwhelmingly likely cause is that repoRoot is not the
		// repository this source came from — unrelated histories share no
		// commit — which is worth saying outright, since --dir is how a
		// caller points at the wrong one in the first place.
		return fail("the discobox's history has no commit in common with %s, so there is nothing to apply onto; is that the repository source %q came from? (%v)", repoRoot, entry.slug, err)
	}
	printer.note("base %s — %s", shortSHA(report.Base), formatBaseOrigin(report.BaseOrigin))

	if report.Base == tip {
		report.Status = applyStatusUpToDate
		printer.outcome(applyStatusUpToDate, "UP TO DATE: the discobox has no commits after %s, so local %s is unchanged", shortSHA(report.Base), applyTarget(report))
		return report
	}

	commits, err := gitutil.Log(ctx, repoRoot, report.Base+".."+tip)
	if err != nil {
		return fail("%v", err)
	}
	report.Commits = applyCommits(commits)
	printer.commitsToApply(report.Commits)
	printer.commitList(report.Commits)

	var result gitapply.Result
	if hostUnborn {
		// This apply is what gives the repository its history, so there is no
		// branch to fast-forward — only the files it was created from to
		// replace, and they have to still be those files.
		wantTree, carried, err := a.createdFromTree(ctx, repoRoot, gitServerURL, projectID, sandboxID, entry.source)
		if err != nil {
			return fail("%v", err)
		}
		printer.note("local %s has no commits yet: replaying them onto no history, then creating %s", repoRoot, applyTarget(report))
		result, err = gitapply.AttemptRoot(ctx, repoRoot, report.Base, tip, wantTree)
		if err != nil {
			return fail("%v", err)
		}
		if len(result.ChangedPaths) > 0 {
			report.Status = applyStatusBlocked
			report.LocalChanges = result.ChangedPaths
			report.NextSteps = localChangesNextSteps(sandboxID, entry.slug, repoRoot, dirOverrides[entry.slug], carried)
			printer.outcome(applyStatusBlocked, "BLOCKED: %s", blockedLocalChanges(repoRoot, carried))
			printer.detailLines(report.LocalChanges)
			printer.nextSteps(report.NextSteps)
			return report
		}
	} else {
		printer.note("cherry-picking them in a scratch worktree, then fast-forwarding local %s", applyTarget(report))
		result, err = gitapply.Attempt(ctx, repoRoot, report.Base, tip)
		if err != nil {
			return fail("%v", err)
		}
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
		printer.outcome(applyStatusConflict, "CONFLICT: %s%s did not apply cleanly", shortSHA(report.ConflictCommit), quoteSubject(commitSubject(report.Commits, report.ConflictCommit)))
		printer.noteDetail("nothing in %s changed; local %s is still at %s", repoRoot, applyTarget(report), shortSHA(report.HostBase))
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
	printer.outcome(applyStatusApplied, "APPLIED %d %s to local %s", len(report.Commits), pluralize("commit", len(report.Commits)), applyTarget(report))
	printer.appliedList(report.Commits)
	printer.landed(report)
	printer.note("recorded on discobox %s as applied to %s", sandboxID, repoRoot)
	return report
}

// resolveApplyBase chooses the exclusive end of the sandbox commit range.
// A prior apply is the narrowest answer while its sandbox-side commit remains
// in the current history. A rebase can rewrite that commit away; in that case
// it is a stale cursor, so the range is derived from the histories that exist
// now. Repositories created without commits retain their explicit empty base,
// because their histories need not share an ancestor (ADR 0084).
func resolveApplyBase(ctx context.Context, repoRoot, tip, lastCommit, discoboxBase string) (string, baseOrigin, error) {
	if lastCommit != "" && gitapply.IsAncestor(ctx, repoRoot, lastCommit, tip) {
		return lastCommit, baseOriginLastApplied, nil
	}
	if discoboxBase != "" {
		return discoboxBase, baseOriginDiscoboxBase, nil
	}
	base, err := gitapply.MergeBase(ctx, repoRoot, tip)
	if err != nil {
		return "", "", err
	}
	return base, baseOriginMergeBase, nil
}

// sandboxDirtyBlocks makes the discobox-side working-tree check that comes
// before either apply path decides anything, records what it found on the
// report, and says whether that ends the source.
//
// Both paths make it and both say the same thing about it, because it is the
// same fact about the same discobox: uncommitted work is never applied, and a
// source is not reported as drained while some of it is still sitting there.
// Where the two differ is only the re-run they print — a source with no local
// directory needs a --dir on it — which is what dir carries.
func (a *App) sandboxDirtyBlocks(ctx context.Context, printer applyPrinter, projectID, sandboxID string, report *applySourceReport, allowDirty bool, dir string) (bool, error) {
	if report.SandboxDir == "" {
		// The discobox never said where this source landed, so there is no
		// working tree to ask about.
		return false, nil
	}
	printer.note("checking the discobox working tree (git status --porcelain)")
	dirty, status, err := a.sandboxSourceDirty(ctx, projectID, sandboxID, report.SandboxDir)
	if err != nil {
		return false, err
	}
	switch {
	case dirty && !allowDirty:
		report.Status = applyStatusBlocked
		report.UncommittedChanges = statusLines(status)
		report.NextSteps = dirtyNextSteps(sandboxID, report.Slug, report.SandboxDir, dir)
		printer.outcome(applyStatusBlocked, "BLOCKED: the discobox has %d uncommitted %s; only committed work is applied",
			len(report.UncommittedChanges), pluralize("change", len(report.UncommittedChanges)))
		printer.detailLines(report.UncommittedChanges)
		printer.nextSteps(report.NextSteps)
		return true, nil
	case dirty:
		// --allow-dirty. The uncommitted work is still listed: it stays in
		// the sandbox, and the whole point of the flag is that the user
		// chose to leave it there rather than not knowing about it.
		report.UncommittedChanges = statusLines(status)
		report.DirtyIgnored = true
		printer.caution("--allow-dirty: applying anyway; %d uncommitted %s stay in the discobox and are not applied",
			len(report.UncommittedChanges), pluralize("change", len(report.UncommittedChanges)))
		printer.detailLines(report.UncommittedChanges)
	default:
		printer.noteDetail("clean, nothing uncommitted")
	}
	return false, nil
}

// unplacedSource ends a source whose local directory could not be resolved:
// a discobox created on another machine, a source cloned from a remote rather
// than pushed from a checkout here, or a directory that has since moved.
//
// Not knowing where a source would land only costs anything when it has
// commits to land. A source cloned from a remote and left alone in the
// discobox has nothing to place, and failing over it would fail every apply of
// every discobox that carries one, forever — so the discobox's tip is read
// straight off its repository, which ls-remote does without a local clone, and
// compared against the commit this source is already accounted for at: the
// last apply's, or the commit it was created at. Those match only when the
// discobox has committed nothing here, and that is the one case where an
// unknown directory is not a failure.
func (a *App) unplacedSource(ctx context.Context, printer applyPrinter, projectID, sandboxID string, sandbox *apimodel.Sandbox, gitServerURL string, entry applySourceEntry, allowDirty bool, dirErr error) applySourceReport {
	report := applySourceReport{
		Slug:          entry.slug,
		Status:        applyStatusError,
		SandboxDir:    sourceWorkdir(entry.source),
		SandboxRef:    sandboxapply.FetchRef(sandboxID, entry.slug),
		HostPathError: dirErr.Error(),
	}
	// The heading names the discobox side, which is the half of this source
	// that is known, and says in the local repo's own row why there is no
	// local repo — before anything else happens, because everything that
	// follows may hand the reader a --dir to fill in, and none of it explains
	// itself without this. Status is not decided by it: a discobox that has
	// committed nothing to this source needs no directory at all.
	printer.sourceHeader(report)
	fail := func() applySourceReport {
		report.Status = applyStatusError
		printer.outcome(applyStatusError, "ERROR: %s", report.Error)
		printer.nextSteps(report.NextSteps)
		return report
	}

	// The same working-tree check every other route makes, and for the same
	// reason: a source is never reported as having nothing to apply while
	// uncommitted work sits in it. The re-run it prints carries the --dir this
	// source will need once that work is committed.
	blocked, err := a.sandboxDirtyBlocks(ctx, printer, projectID, sandboxID, &report, allowDirty, applyDirPlaceholder)
	if err != nil {
		report.Error = fmt.Sprintf("%v (and its working tree could not be checked: %v)", dirErr, err)
		return fail()
	}
	if blocked {
		return report
	}

	report.Base, report.BaseOrigin = unplacedSourceBase(sandbox, entry)
	if report.Base == "" {
		// Nothing records the commit this source started from — a source
		// created from a URL and a branch name, never applied since, has only
		// the branch name — so no tip can be measured against anything and
		// whether there is work here cannot be told. Saying that is the honest
		// answer; assuming there is none would drop the discobox's commits on
		// the floor.
		report.Error = fmt.Sprintf("%v (and nothing records the commit it started from, so whether it has anything to apply cannot be told)", dirErr)
		return fail()
	}

	printer.note("reading the discobox's tip (git ls-remote), to see whether it has anything that needs one")
	tip, err := sandboxapply.Tip(ctx, gitServerURL, projectID, sandboxID, a.token, entry.source)
	if err != nil {
		report.Error = fmt.Sprintf("%v (and whether it has anything to apply could not be checked: %v)", dirErr, err)
		return fail()
	}
	report.SandboxTip = tip
	printer.noteDetail("discobox tip %s", shortSHA(tip))
	printer.note("base %s — %s", shortSHA(report.Base), formatBaseOrigin(report.BaseOrigin))

	if tip == report.Base {
		report.Status = applyStatusUpToDate
		printer.outcome(applyStatusUpToDate, "UP TO DATE: the discobox has no commits after %s, so there is nothing to apply and nothing needs a local directory here", shortSHA(report.Base))
		return report
	}
	report.Error = fmt.Sprintf("the discobox has commits after %s to apply, but %v", shortSHA(report.Base), dirErr)
	report.NextSteps = []applyNextStep{{
		Description: fmt.Sprintf("apply them into a local checkout of the repository source %q came from", entry.slug),
		Commands:    []string{applyRerun(sandboxID, entry.slug, applyDirPlaceholder)},
	}}
	return fail()
}

// unplacedSourceBase is the commit a source with no local directory is already
// accounted for at, and where that commit came from: the last apply of this
// source, or failing that the commit the source was created at. Anything the
// discobox has committed after it is work that would be applied, and is what
// makes an unknown local directory matter.
//
// It is deliberately not a merge base: there is no local repository here to
// take one against, which is the situation that got us here. For the same
// reason it cannot make resolveApplyBase's check that a recorded last-applied
// commit is still an ancestor of the tip — a rebase in the discobox can leave
// that cursor stale, and IsAncestor needs the objects. A stale one simply
// fails to match the tip, so the source is reported as having work to place
// and asks for a --dir; erring towards "there is something here" is the safe
// way to be wrong when the answer cannot be checked.
func unplacedSourceBase(sandbox *apimodel.Sandbox, entry applySourceEntry) (string, baseOrigin) {
	if last, ok := lastApplied(sandbox, entry.slug); ok && last.Commit != "" {
		return last.Commit, baseOriginLastApplied
	}
	if commit := checkoutCommit(entry.source); commit != "" {
		return commit, baseOriginSourceCheckout
	}
	return "", ""
}

// checkoutCommit is the commit a source was created against, which for a
// discobox created from a repository with no commits is the empty base commit
// its whole history hangs off.
func checkoutCommit(source apimodel.GitSource) string {
	checkout, ok := source.Checkout.Get()
	if !ok {
		return ""
	}
	return strings.TrimSpace(checkout.Commit.Or(""))
}

// createdFromTree is the tree the discobox was created from: the workspace
// snapshot it carried, or the empty tree when it carried nothing. The second
// return says which of those it was, because a working tree that no longer
// matches means something different in each case.
//
// It is what a local repository with no commits must still hold for its first
// apply to be safe, so a missing ref must not read as a changed working tree.
// The ref is resolved out of the repository first, where create wrote it, and
// fetched from the discobox's own origin if it is gone from here — delivery
// pushed it there, so the discobox is the second copy of the answer.
func (a *App) createdFromTree(ctx context.Context, repoRoot, gitServerURL, projectID, sandboxID string, source apimodel.GitSource) (string, bool, error) {
	ref := ""
	if workspace, ok := source.Workspace.Get(); ok {
		ref = strings.TrimSpace(workspace.SnapshotRef.Or(""))
	}
	if ref == "" {
		// Nothing was carried, so the discobox was created from an empty
		// working tree and that is what this one has to still be.
		tree, err := gitutil.EmptyTree(ctx, repoRoot)
		return tree, false, err
	}
	if tree, err := resolveTree(ctx, repoRoot, ref); err == nil {
		return tree, true, nil
	}
	if err := sandboxapply.Fetch(ctx, repoRoot, gitServerURL, projectID, sandboxID, a.token, source, "+"+ref+":"+ref); err != nil {
		return "", true, fmt.Errorf("fetch what the discobox was created from (%s), to check nothing here has changed since: %w", ref, err)
	}
	tree, err := resolveTree(ctx, repoRoot, ref)
	return tree, true, err
}

func resolveTree(ctx context.Context, repoRoot, rev string) (string, error) {
	out, err := gitutil.Output(ctx, repoRoot, nil, nil, "rev-parse", "--verify", rev+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("resolve tree of %s: %w", rev, err)
	}
	return strings.TrimSpace(out), nil
}

// blockedLocalChanges says why the local working tree is not what the discobox
// was created from. There are two ways to get here and they are different
// things to have done: the user changed a working tree the discobox was given,
// or the user told it not to carry that working tree in the first place
// (--include-dirty=false, or a repository that gained files after the create).
// Telling the second one their repository "has changed" accuses them of
// something they did not do.
func blockedLocalChanges(repoRoot string, carried bool) string {
	if carried {
		return fmt.Sprintf("local %s has changed since this discobox was created, and has no commits to keep those changes in", repoRoot)
	}
	return fmt.Sprintf("local %s holds files this discobox was never given, and has no commits to keep them in", repoRoot)
}

// localChangesNextSteps is the way out of a first apply refused because the
// local working tree is not what the discobox was created from. Committing the
// local work is the one that keeps it, and it is the same answer either way:
// the range being applied never depended on a shared history, so it
// cherry-picks onto whatever the user commits just as well.
//
// The alternative is not the same answer. Undoing an edit puts a carried
// working tree back the way the discobox found it; doing that to files the
// discobox was never given would mean deleting them, which is nobody's idea of
// a way out.
func localChangesNextSteps(sandboxID, slug, repoRoot, dir string, carried bool) []applyNextStep {
	rerun := applyRerun(sandboxID, slug, dir)
	alternative := "or look at what changed, and put it back the way the discobox found it"
	if !carried {
		alternative = "or look at what is here, and move aside anything the discobox's commits would land on"
	}
	return []applyNextStep{
		{
			Description: "commit the local files first, then apply the discobox's commits on top of them",
			Commands: []string{
				fmt.Sprintf("git -C %s add -A", repoRoot),
				fmt.Sprintf("git -C %s commit -m MESSAGE", repoRoot),
				rerun,
			},
		},
		{
			Description: alternative,
			Commands:    []string{fmt.Sprintf("git -C %s status", repoRoot)},
		},
	}
}

// applyFrom names where the commits landed from, for a line that reads as a
// range. A repository getting its first commits has no "from" commit to name.
func applyFrom(report applySourceReport) string {
	if report.HostBase == "" {
		return "no commits"
	}
	return shortSHA(report.HostBase)
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

// applyDirPlaceholder stands in for the local directory in a re-run printed for
// a source that has none: the user is the only one who knows which checkout it
// should be, and the command is printed to be filled in and run.
const applyDirPlaceholder = "PATH"

// applyRerun is the command that runs this apply again for one source. dir is
// the local directory to name in a --dir: the one the caller passed, or the
// PATH placeholder for a source that has no local directory to be found and
// cannot be re-run without being given one. Empty means the source resolves its
// own directory and needs no flag.
func applyRerun(sandboxID, slug, dir string) string {
	rerun := fmt.Sprintf("discobox apply %s --source %s", sandboxID, slug)
	if dir != "" {
		rerun += fmt.Sprintf(" --dir %s=%s", slug, dir)
	}
	return rerun
}

// dirtyNextSteps is the two ways out of a dirty sandbox working tree: commit
// the work there and apply it too, or apply only what is already committed and
// leave the rest. Both re-runs are spelled out for this exact source, --dir
// override included, so neither has to be reassembled by hand.
func dirtyNextSteps(sandboxID, slug, sandboxDir, dir string) []applyNextStep {
	rerun := applyRerun(sandboxID, slug, dir)
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
