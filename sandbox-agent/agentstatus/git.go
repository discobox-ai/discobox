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
	return status
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
