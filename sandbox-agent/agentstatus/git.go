package agentstatus

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/obot-platform/discobox/sandbox-agent/execs"
	"github.com/obot-platform/discobox/sandboxconfig"
)

const (
	perSourceTimeout    = 2 * time.Second
	totalSourcesBudget  = 8 * time.Second
	maxPorcelainCapture = 64 * 1024
)

// ComputeGitStatus reports git status for every source, bounded so one huge
// or unreachable source cannot stall or balloon the whole response: each
// source gets its own timeout and output cap, and a failure on one source is
// recorded on that source alone. user, when set, is the sandbox's resolved
// user (the same identity exec'd terminals and execs already run as; see
// execs.AgentSysProcAttr) — git runs as that user rather than as
// sandbox-agent's own root process, since sources are owned by it and git
// itself refuses to operate as a different, mismatched owner.
func ComputeGitStatus(ctx context.Context, sources []sandboxconfig.Source, user *execs.User) []GitSourceStatus {
	if len(sources) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, totalSourcesBudget)
	defer cancel()
	out := make([]GitSourceStatus, 0, len(sources))
	for _, source := range sources {
		out = append(out, gitStatusForSource(ctx, source, user))
	}
	return out
}

func gitStatusForSource(ctx context.Context, source sandboxconfig.Source, user *execs.User) GitSourceStatus {
	status := GitSourceStatus{Slug: source.Slug, Target: source.Target, ObservedAt: time.Now().UTC()}
	sourceCtx, cancel := context.WithTimeout(ctx, perSourceTimeout)
	defer cancel()
	output, truncated, err := runGitStatus(sourceCtx, source.Target, user)
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.Porcelain = output
	status.Truncated = truncated
	status.Branch, status.HeadCommit, status.Ahead, status.Behind = parseBranchHeader(output)
	status.Clean = !hasFileChanges(output)
	// The diff stat lets a listing show what a sandbox has changed without
	// running git from outside — the point of reporting status at all. A base
	// the repository does not have (or a failure of any kind) drops the stat,
	// not the source: the status above is still true.
	if base, ok := resolveDiffBase(sourceCtx, source, user); ok {
		if files, added, deleted, ok := runDiffShortstat(sourceCtx, source.Target, base, user); ok {
			status.DiffBase = base
			status.DiffFiles, status.DiffAdded, status.DiffDeleted = files, added, deleted
		}
	}
	return status
}

// resolveDiffBase is the commit the diff stat measures from: the commit the
// source was spawned at, forwarded to the merge base with its upstream
// tracking ref once the sandbox has fetched — so commits it pulled rather
// than wrote stop counting as its changes. This is ADR 0018's base
// resolution, carried into the agent when the command that ran it went away
// (ADR 0037).
func resolveDiffBase(ctx context.Context, source sandboxconfig.Source, user *execs.User) (string, bool) {
	spawn := strings.TrimSpace(source.BaseCommit)
	if spawn == "" {
		return "", false
	}
	base, err := gitOutput(ctx, source.Target, user, "rev-parse", "--verify", "--quiet", spawn+"^{commit}")
	if err != nil {
		return "", false
	}
	upstream := strings.TrimSpace(source.UpstreamRef)
	if upstream == "" {
		return base, true
	}
	// The upstream ref is verified rather than assumed: a push-delivered
	// source has no remote at all, and an unfetched clone's tip is the spawn
	// commit anyway. When it does resolve, only ever move forward — a
	// rewritten upstream leaves a merge base *older* than the spawn commit,
	// and taking it would widen the diff with commits the sandbox never wrote.
	tip, err := gitOutput(ctx, source.Target, user, "rev-parse", "--verify", "--quiet", upstream+"^{commit}")
	if err != nil {
		return base, true
	}
	merged, err := gitOutput(ctx, source.Target, user, "merge-base", "HEAD", tip)
	if err != nil {
		return base, true
	}
	if merged != base && gitRun(ctx, source.Target, user, "merge-base", "--is-ancestor", base, merged) == nil {
		return merged, true
	}
	return base, true
}

// runDiffShortstat counts what the working tree holds that base does not —
// committed and uncommitted tracked changes both, untracked files not — as
// `git diff --shortstat` reports it.
func runDiffShortstat(ctx context.Context, dir, base string, user *execs.User) (files, added, deleted int, ok bool) {
	out, err := gitOutput(ctx, dir, user, "diff", "--shortstat", base)
	if err != nil {
		return 0, 0, 0, false
	}
	files, added, deleted = parseShortstat(out)
	return files, added, deleted, true
}

// gitOutput runs one git command in dir as the sandbox's user and returns its
// trimmed stdout.
func gitOutput(ctx context.Context, dir string, user *execs.User, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // args are fixed subcommands plus manifest-authored refs, passed as argv, never through a shell.
	attr, err := execs.AgentSysProcAttr(user)
	if err != nil {
		return "", err
	}
	cmd.SysProcAttr = attr
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// gitRun runs one git command in dir as the sandbox's user for its exit
// status alone.
func gitRun(ctx context.Context, dir string, user *execs.User, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // args are fixed subcommands plus manifest-authored refs, passed as argv, never through a shell.
	attr, err := execs.AgentSysProcAttr(user)
	if err != nil {
		return err
	}
	cmd.SysProcAttr = attr
	return cmd.Run()
}

// parseShortstat picks the counts out of git's summary line, which reads
// "3 files changed, 61 insertions(+), 12 deletions(-)" — with any clause
// absent when it is zero, and the whole line absent when nothing changed.
func parseShortstat(out string) (files, added, deleted int) {
	for _, clause := range strings.Split(out, ",") {
		fields := strings.Fields(clause)
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(fields[1], "file"):
			files = n
		case strings.HasPrefix(fields[1], "insertion"):
			added = n
		case strings.HasPrefix(fields[1], "deletion"):
			deleted = n
		}
	}
	return files, added, deleted
}

func runGitStatus(ctx context.Context, dir string, user *execs.User) (output string, truncated bool, err error) {
	if strings.TrimSpace(dir) == "" {
		return "", false, errors.New("source target is empty")
	}
	info, statErr := os.Stat(dir)
	if statErr != nil {
		return "", false, statErr
	}
	if !info.IsDir() {
		return "", false, errors.New("source target is not a directory")
	}
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain=v2", "--branch")
	attr, err := execs.AgentSysProcAttr(user)
	if err != nil {
		return "", false, err
	}
	cmd.SysProcAttr = attr
	stdout := &limitedBuffer{max: maxPorcelainCapture}
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", false, errors.New(msg)
	}
	return stdout.buf.String(), stdout.truncated, nil
}

// limitedBuffer caps how much of a command's stdout is retained without
// blocking or failing the write, so a huge dirty tree cannot balloon memory
// or stall the process — excess bytes are silently discarded and Truncated
// is reported instead.
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := b.max - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

// parseBranchHeader reads porcelain v2's "# branch.*" header lines. Missing
// fields (e.g. no upstream, or the initial commit) are left zero-valued.
func parseBranchHeader(output string) (branch, headCommit string, ahead, behind int) {
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(line, "# branch.oid "):
			headCommit = strings.TrimPrefix(line, "# branch.oid ")
		case strings.HasPrefix(line, "# branch.head "):
			branch = strings.TrimPrefix(line, "# branch.head ")
		case strings.HasPrefix(line, "# branch.ab "):
			for _, field := range strings.Fields(strings.TrimPrefix(line, "# branch.ab ")) {
				switch {
				case strings.HasPrefix(field, "+"):
					ahead, _ = strconv.Atoi(strings.TrimPrefix(field, "+"))
				case strings.HasPrefix(field, "-"):
					behind, _ = strconv.Atoi(strings.TrimPrefix(field, "-"))
				}
			}
		}
	}
	return branch, headCommit, ahead, behind
}

// hasFileChanges reports whether any non-header line is present, i.e. the
// working tree is dirty.
func hasFileChanges(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return true
	}
	return false
}
