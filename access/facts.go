package access

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
)

// factsTimeout bounds every git lookup gatherFacts makes, combined. A fact
// is best-effort context for the judge, and a git invocation that hangs (a
// credential helper prompting on a terminal that is not there, for instance)
// must not hold the command up on top of the judge's own time.
const factsTimeout = 5 * time.Second

// gatherFacts reports where the command runs and, for a command naming a git
// ref, what that ref resolves to (ADR 0090 §2), for the judge to weigh as the
// sandbox's claim. Every field is best-effort and independently optional: a
// value this could not establish is left empty rather than sent as a
// placeholder, and nil means nothing was established at all.
//
// It runs a fixed, argument-free set of lookups against the current directory
// and, for a git command, the ref it names. It asks; it does not look — there
// is no path here that takes a hint from the argv about where else to check or
// what else to run. It never fails the caller: an error here means a fact is
// missing, not that the command stops.
func gatherFacts(ctx context.Context, command []string) *agentcreds.Reported {
	ctx, cancel := context.WithTimeout(ctx, factsTimeout)
	defer cancel()

	var reported agentcreds.Reported
	if cwd, err := os.Getwd(); err == nil {
		reported.WorkingDirectory = cwd
	}
	reported.RepositoryRoot = gitOutput(ctx, "rev-parse", "--show-toplevel")
	if len(command) > 0 && command[0] == "git" {
		reported.RefCommit, reported.RefSubject = gitRefFact(ctx, command[1:])
	}
	for _, field := range []*string{&reported.WorkingDirectory, &reported.RepositoryRoot, &reported.RefCommit, &reported.RefSubject} {
		// A path or a line longer than the judge accepts is not one worth
		// refusing the command over; it is cut, at a character.
		if len(*field) > agentcreds.MaxReportedBytes {
			*field = string(trimPartialRune([]byte((*field)[:agentcreds.MaxReportedBytes])))
		}
	}
	if reported == (agentcreds.Reported{}) {
		return nil
	}
	return &reported
}

// gitRefFact tries every non-flag argument after "git" as a ref, in the
// order they appear, including each half of a "left:right" refspec, and
// reports the first one that resolves to a commit. A command naming several
// refs — a push's source and destination, for instance — gets one fact
// rather than a menu: enough to catch a refspec that names something unlike
// the approved sentence, which is all ADR 0090 §3 claims for this.
func gitRefFact(ctx context.Context, args []string) (sha, subject string) {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		candidates := []string{arg}
		if left, right, ok := strings.Cut(arg, ":"); ok {
			candidates = []string{left, right}
		}
		for _, candidate := range candidates {
			candidate = strings.TrimSpace(candidate)
			if candidate == "" {
				continue
			}
			if sha = gitOutput(ctx, "rev-parse", "--verify", candidate+"^{commit}"); sha != "" {
				return sha, gitOutput(ctx, "log", "-1", "--format=%s", sha)
			}
		}
	}
	return "", ""
}

// gitOutput runs one read-only git query with the repository's own
// configuration held at arm's length. The repository this queries is written
// by the agent this whole query exists to help judge, and a config-driven
// hook is exactly the tool access ADR 0090 §2 refuses: `core.pager`, a
// `diff.external`, and a textconv filter are all ways `.git/config` runs a
// command of the repository's choosing instead of git's. None of the calls
// this file makes ever diffs or shows a patch, so none of them is reachable
// today — the guard is here so that stays true if one is added later, not
// because today's calls need it.
//
// A failure of any kind — not a repository, git not installed, the ref does
// not resolve — is reported as an absent fact, not an error: gatherFacts has
// nothing useful to do with why a lookup came back empty.
func gitOutput(ctx context.Context, args ...string) string {
	full := append([]string{"-c", "core.pager=cat", "-c", "diff.external=", "--no-pager"}, args...)
	//nolint:gosec // Fixed git subcommands; the only caller-influenced argument is a ref name, never a shell.
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
