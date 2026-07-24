package cli

import (
	"context"
	"fmt"
	"io"
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
// committed source changes into the host repository they started from, via
// fetch + cherry-pick, never merge.
func (a *App) newApplyCommand() *cobra.Command {
	var sourceSlug string
	var dirOverrides []string
	cmd := &cobra.Command{
		Use:   "apply [SANDBOX_ID] [flags]",
		Short: "Apply a sandbox's committed source changes onto a host working tree",
		Long: `Fetch a sandbox's source commits and cherry-pick them onto the host working
tree they started from, per docs/adr/0014.

Without SANDBOX_ID the sandbox is taken from the ones "disco ls" shows for the
current project directory: the only one when there is one, otherwise you are
asked to pick.

Every source on the sandbox is applied by default (the primary source plus any
secondary ones); --source narrows to one, named by its slug.

Each source is only applied into the local directory the sandbox knows it came
from, on this host. A source with no known local directory here (a different
host, or a remote-cloned source) needs an explicit --dir slug=path.

Uncommitted changes in the sandbox are never applied: only what has been
committed there. If cherry-picking a source's commits does not apply cleanly,
nothing about the host repository changes; the commands to reproduce and
resolve it manually are printed instead.`,
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
	cmd.Flags().StringArrayVar(&dirOverrides, "dir", nil, "Host directory to apply a source into, as slug=path; required for a source with no known local directory on this host")
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

	out := cmd.OutOrStdout()
	var failures int
	for _, s := range sources {
		if err := a.applyOneSource(ctx, out, client, projectID, sandboxID, sandbox, host, s, dirOverrides); err != nil {
			failures++
			fmt.Fprintf(out, "source %s: %v\n", s.slug, err)
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d sources did not apply; see above", failures, len(sources))
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

func (a *App) applyOneSource(ctx context.Context, out io.Writer, client *apiclientgen.Client, projectID, sandboxID string, sandbox *apimodel.Sandbox, hostID string, entry applySourceEntry, dirOverrides map[string]string) error {
	hostDir, err := resolveApplyHostDir(sandbox, hostID, entry, dirOverrides)
	if err != nil {
		return err
	}
	repoRoot, err := gitutil.Root(ctx, hostDir)
	if err != nil {
		return fmt.Errorf("%s is not a Git repository: %w", hostDir, err)
	}

	workdir := ""
	if dest, ok := entry.source.Destination.Get(); ok {
		if wd, ok := dest.WorkingDirectory.Get(); ok {
			workdir = wd
		} else if dir, ok := dest.Directory.Get(); ok {
			workdir = dir
		}
	}
	if workdir != "" {
		dirty, status, err := a.sandboxSourceDirty(ctx, projectID, sandboxID, workdir)
		if err != nil {
			return fmt.Errorf("check sandbox working tree: %w", err)
		}
		if dirty {
			fmt.Fprintf(out, "source %s: sandbox has uncommitted changes, skipping:\n%s\n", entry.slug, status)
			return nil
		}
	}

	tip, err := sandboxapply.FetchSource(ctx, repoRoot, a.serverURL, projectID, sandboxID, a.token, entry.source)
	if err != nil {
		return err
	}

	base := lastAppliedCommit(sandbox, entry.slug)
	if base == "" {
		base, err = gitapply.MergeBase(ctx, repoRoot, tip)
		if err != nil {
			return err
		}
	}
	if base == tip {
		fmt.Fprintf(out, "source %s: already up to date\n", entry.slug)
		return nil
	}

	result, err := gitapply.Attempt(ctx, repoRoot, base, tip)
	if err != nil {
		return err
	}
	if !result.Landed {
		// The fetch above already landed ref locally in repoRoot, so no
		// further "git fetch" is needed here — only the cherry-pick that
		// actually reproduces the conflict.
		ref := sandboxapply.FetchRef(sandboxID, entry.slug)
		return fmt.Errorf("commit %s did not apply cleanly; nothing in %s changed. Resolve it there directly:\n"+
			"  git cherry-pick %s..%s",
			shortSHA(result.ConflictCommit), repoRoot, shortSHA(base), ref)
	}

	if _, err := client.CompleteSandboxApply(ctx, &apimodel.CompleteSandboxApplyBody{
		Slug:       entry.slug,
		Commit:     tip,
		HostCommit: result.HostTip,
		HostId:     hostID,
		HostPath:   repoRoot,
	}, apiclientgen.CompleteSandboxApplyParams{ProjectId: projectID, SandboxId: sandboxID}); err != nil {
		return fmt.Errorf("commits landed at %s but recording the apply failed: %w", shortSHA(result.HostTip), err)
	}

	fmt.Fprintf(out, "source %s: applied to %s at %s\n", entry.slug, repoRoot, shortSHA(result.HostTip))
	return nil
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
	return "", fmt.Errorf("no known local directory on this host; pass --dir %s=PATH", entry.slug)
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
