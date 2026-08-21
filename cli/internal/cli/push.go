package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxpush"
	"github.com/discobox-ai/discobox/internal/gitutil"
	"github.com/discobox-ai/discobox/internal/hostid"
)

// newPushCommand implements `discobox push` (ADR 0058): sending local commits into
// the origin repository a push-delivered sandbox fetches from, so work done here
// since the sandbox was created can be rebased onto there.
func (a *App) newPushCommand() *cobra.Command {
	var sourceSlug string
	var dirOverrides []string
	var branch string
	var force bool
	cmd := &cobra.Command{
		Use:   "push [DISCOBOX_ID] [flags]",
		Short: "Push local commits into a discobox's origin, to rebase onto there",
		Long: `Send this machine's commits into the origin repository a discobox fetches from,
per docs/adr/0058.

Commits only ever move local -> the discobox's origin. Nothing in the discobox
moves: no branch of its own changes, nothing is rebased, nothing is checked out.
Inside the discobox the new commits show up as origin/<branch>, to be picked up
whenever the work there is ready for them:

    git fetch origin && git rebase origin/<branch>

Without DISCOBOX_ID the discobox is taken from the ones "discobox ls" shows for the
current project directory: the only one when there is one, otherwise you are
asked to pick.

This applies to each source that was delivered by pushing it — one on a machine
or a provider that cannot read its directory. Every push-delivered source of the
discobox is pushed; --source narrows to one, named by its slug. A source the
discobox reads live already shows new commits without any push, and one cloned
from a remote URL fetches that remote itself; either way there is nothing to send
and push says so.

For each source, the branch it was created from is pushed by default. --branch pushes a
different local branch instead, landing under its own name, which the discobox
then sees as origin/<that branch> — useful for offering it a branch to rebase
onto or cherry-pick from without touching the one it tracks.

Uncommitted changes are never pushed; only commits. They are reported so it is
clear what stayed behind.

A push may rewind the discobox's origin — that is what a local rebase or amend
means — but not silently: it is refused if the origin has moved since this
machine last pushed, or if it holds commits this machine did not put there.
--force pushes anyway, and also pushes a history unrelated to what the discobox
holds.`,
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
			return a.runPush(cmd, sandboxArg, sourceSlug, overrides, branch, force)
		},
	}
	cmd.Flags().StringVar(&sourceSlug, "source", "", "Push only the source with this slug, instead of every push-delivered source on the discobox")
	cmd.Flags().StringArrayVar(&dirOverrides, "dir", nil, "Local directory to push a source from, as slug=path; required for a source with no known local directory on this machine")
	cmd.Flags().StringVar(&branch, "branch", "", "Local branch to push, landing under the same name in the discobox's origin; defaults to the branch the source was created from")
	cmd.Flags().BoolVar(&force, "force", false, "Push even if the origin has moved since this machine last pushed, or holds an unrelated history")
	return cmd
}

// pushReport is one run of `discobox push`, per source.
type pushReport struct {
	SandboxID   string             `json:"sandboxId"`
	SandboxName string             `json:"sandboxName,omitempty"`
	Sources     []pushSourceReport `json:"sources"`
}

type pushSourceReport struct {
	Slug       string `json:"slug"`
	Status     string `json:"status"`
	HostPath   string `json:"hostPath,omitempty"`
	LocalRev   string `json:"localRev,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Commit     string `json:"commit,omitempty"`
	Previous   string `json:"previous,omitempty"`
	Forced     bool   `json:"forced,omitempty"`
	DirtyFiles int    `json:"dirtyFiles,omitempty"`
	Error      string `json:"error,omitempty"`
}

const (
	pushStatusPushed   = "pushed"
	pushStatusUpToDate = "up-to-date"
	pushStatusSkipped  = "skipped"
	pushStatusError    = "error"
)

func (a *App) runPush(cmd *cobra.Command, sandboxArg, onlySlug string, dirOverrides map[string]string, branch string, force bool) error {
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
	// An archived sandbox has no container for the git route to start, so the
	// push would fail in the proxy with nothing useful to say.
	if state := sandboxDisplayState(*sandbox); state == "archived" || state == "archiving" {
		return fmt.Errorf("the discobox is %s; unarchive it before pushing to its origin", state)
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
	if a.debug {
		stderr := cmd.ErrOrStderr()
		ctx = gitutil.WithTracer(ctx, func(dir string, args []string) {
			fmt.Fprintf(stderr, "+ git -C %s %s\n", dir, strings.Join(args, " "))
		})
	}

	out := cmd.OutOrStdout()
	text := a.output != "json"
	report := pushReport{SandboxID: sandboxID, SandboxName: sandbox.Config.Name}
	for _, entry := range sources {
		source := pushOneSource(ctx, projectID, sandboxID, sandbox, host, gitServerURL, a.token, entry, dirOverrides, branch, force, onlySlug != "")
		report.Sources = append(report.Sources, source)
		if text {
			printPushSource(cmd, source)
		}
	}
	if !text {
		if err := writeJSON(out, report); err != nil {
			return err
		}
	}
	var failures []string
	for _, source := range report.Sources {
		if source.Status == pushStatusError {
			failures = append(failures, source.Slug)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d sources did not push: %s", len(failures), len(report.Sources), strings.Join(failures, ", "))
	}
	return nil
}

// pushOneSource pushes one source and accounts for what it did, never returning
// an error: every outcome is a status on the report, so a sandbox with several
// sources reports them all rather than stopping at the first that has nothing to
// send.
func pushOneSource(ctx context.Context, projectID, sandboxID string, sandbox *apimodel.Sandbox, hostID, gitServerURL, token string, entry applySourceEntry, dirOverrides map[string]string, branch string, force, explicit bool) pushSourceReport {
	report := pushSourceReport{Slug: entry.slug, Status: pushStatusError}
	fail := func(format string, args ...any) pushSourceReport {
		report.Status, report.Error = pushStatusError, fmt.Sprintf(format, args...)
		return report
	}
	// A source the sandbox reaches on its own is not an error unless it is the
	// one that was asked for by name: pushing a sandbox with a mix of sources
	// should send what it can and say what it skipped.
	if err := sandboxpush.CheckPushDelivered(entry.source); err != nil {
		if explicit {
			return fail("%v", err)
		}
		report.Status, report.Error = pushStatusSkipped, err.Error()
		return report
	}
	hostDir, _, err := resolveApplyHostDir(sandbox, hostID, entry, dirOverrides)
	if err != nil {
		return fail("%v", err)
	}
	repoRoot, err := gitutil.Root(ctx, hostDir)
	if errors.Is(err, gitutil.ErrNotARepository) {
		// The source was delivered from a repository built over the directory
		// for one run and thrown away afterwards (ADR 0045), so there is no
		// history here to push, and each run's would be unrelated anyway.
		return fail("%s is not a Git repository, so there are no commits here to push; `git init` there and commit first", hostDir)
	}
	if err != nil {
		return fail("read the Git repository at %s: %v", hostDir, err)
	}
	report.HostPath = repoRoot

	result, err := sandboxpush.Push(ctx, repoRoot, gitServerURL, projectID, sandboxID, token, entry.source, sandboxpush.Options{Branch: branch, Force: force})
	report.LocalRev, report.Branch, report.Commit = result.LocalRev, result.Branch, result.Commit
	report.Previous, report.Forced, report.DirtyFiles = result.Lease, result.Forced, result.DirtyFiles
	if err != nil {
		return fail("%v", err)
	}
	if result.UpToDate {
		report.Status = pushStatusUpToDate
		return report
	}
	report.Status = pushStatusPushed
	return report
}

func printPushSource(cmd *cobra.Command, report pushSourceReport) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s\n", report.Slug)
	switch report.Status {
	case pushStatusPushed:
		fmt.Fprintf(out, "  local     %s %s\n", report.HostPath, report.LocalRev)
		fmt.Fprintf(out, "  origin    %s -> %s\n", shortCommit(report.Previous), shortCommit(report.Commit))
		fmt.Fprintf(out, "  in the discobox: git fetch origin && git rebase origin/%s\n", report.Branch)
		if report.Forced {
			fmt.Fprintf(out, "  forced\n")
		}
		if report.DirtyFiles > 0 {
			fmt.Fprintf(out, "  %d uncommitted change(s) stayed here — only commits are pushed\n", report.DirtyFiles)
		}
	case pushStatusUpToDate:
		fmt.Fprintf(out, "  up to date: origin already has %s\n", shortCommit(report.Commit))
		if report.DirtyFiles > 0 {
			fmt.Fprintf(out, "  %d uncommitted change(s) stayed here — only commits are pushed\n", report.DirtyFiles)
		}
	case pushStatusSkipped:
		fmt.Fprintf(out, "  skipped: %s\n", report.Error)
	default:
		fmt.Fprintf(out, "  ERROR: %s\n", report.Error)
	}
}
