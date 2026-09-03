package access

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// factsTimeout bounds every git lookup gatherFacts makes, combined. It is far
// shorter than judgeTimeout and is not carved out of it — a fact is
// best-effort context for the prompt, not something worth spending the
// model's own budget waiting on, and a git invocation that hangs (a
// credential helper prompting on a terminal that is not there, for instance)
// must not turn into a slow denial on top of a normal one.
const factsTimeout = 5 * time.Second

// facts is what gatherFacts could establish about where the command runs and,
// for a command naming a git ref, what that ref resolves to (ADR 0090 §2).
// Every field is best-effort and independently optional: a value the CLI
// could not establish is left empty, and judgePrompt omits what it did not
// get rather than sending a placeholder.
type facts struct {
	repoRoot   string
	refSHA     string
	refSubject string
}

// gatherFacts runs a fixed, argument-free set of lookups against the current
// directory and, for a git command, the ref it names. It asks; it does not
// look — there is no path here that takes a hint from the argv about where
// else to check or what else to run (ADR 0090 §2). It never fails the
// caller: an error here means the fact is missing from the prompt, not that
// judging stops.
func gatherFacts(ctx context.Context, command []string) facts {
	ctx, cancel := context.WithTimeout(ctx, factsTimeout)
	defer cancel()

	var f facts
	f.repoRoot = gitOutput(ctx, "rev-parse", "--show-toplevel")
	if len(command) > 0 && command[0] == "git" {
		f.refSHA, f.refSubject = gitRefFact(ctx, command[1:])
	}
	return f
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
