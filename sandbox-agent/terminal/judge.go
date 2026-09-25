package terminal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// The judge runtime's one job (ADR 26-09-22-838 §1): a sandbox in judge mode answers
// judging asks from its pool, and is worked in by nobody.
//
// The ask reaches the harness through the image's own discobox-prompt, with
// the prompt, the schema and --no-tools this process supplies. A caller
// supplies evidence and nothing else: no prompt, no model, no executable, no
// schema. That is what keeps the question the same wherever it is asked from,
// and it is why the job arrives as a judge.Job rather than as text.

// judgePrompt is the wrapper every harness image installs. It is named rather
// than pathed because that is the convention its callers use, and it is
// resolved against the harness environment's own PATH (harness/DESIGN.md).
const judgePrompt = "discobox-prompt"

// Judge answers one ask about one job. Each is a fresh run of the wrapper with
// nothing carried over from the last: no conversation, no history, and no
// state between two requests that happen to be judged by the same runtime.
//
// Asks are answered in parallel. Each run is its own process in its own
// process group, with its own harness state (the wrapper keeps what its CLI
// writes in a directory of the run's own, as it does with --no-tools), and
// every discobox in the project is judged here — so one at a time would make
// every credential-bearing request in the project wait on every other. What
// is bounded is how many run at once (maxJudgingRuns), for the memory each
// harness CLI takes. An ask past that waits for a run to finish for at most
// judgingWait, and is Busy when none frees up.
func (s *Service) Judge(ctx context.Context, job judge.Job) (judge.Answer, error) {
	if s.harnessMode != config.HarnessModeJudge {
		return judge.Answer{}, errors.New("this discobox is not a judge")
	}
	prompt, err := judge.Prompt(job)
	if err != nil {
		return judge.Answer{}, err
	}
	// The wait has a bound of its own, and not only the caller's: the caller
	// here is the control plane, whose request carries no deadline this
	// process can see, so without one an ask would wait until the control
	// plane hung up — and a 429 written then reaches nobody.
	wait, stopWaiting := context.WithTimeout(ctx, s.judgingWait)
	select {
	case s.judging <- struct{}{}:
		stopWaiting()
		defer func() { <-s.judging }()
	case <-wait.Done():
		stopWaiting()
		return judge.Answer{}, fmt.Errorf("%w: %w", errBusy, wait.Err())
	}
	// Whichever is sooner: the caller's deadline, or this ceiling on the run,
	// which is what bounds it for a caller that passed none (judge.Timeout).
	ctx, cancel := context.WithTimeout(ctx, judge.Timeout)
	defer cancel()

	env := s.env
	if s.secretEnv != nil {
		// Read fresh, as an exec does: the judge's own credential is an
		// ordinary harness secret, and it is refreshed under it.
		env = execs.MergeEnv(env, s.exportedSecretEnv())
	}
	workdir, err := s.execs.DefaultWorkdir()
	if err != nil {
		return judge.Answer{}, err
	}
	if err := s.installer.EnsureInstalled(ctx, s.harness, workdir, env); err != nil {
		return judge.Answer{}, fmt.Errorf("the judge's harness is not installed: %w", err)
	}
	out, err := s.execs.RunOnce(ctx, execs.OnceRequest{
		Command: []string{judgePrompt,
			"--model", judge.Role,
			"--system", judge.System,
			"--prompt", prompt,
			"--output-schema", judge.Schema,
			"--no-tools"},
		Env:       env,
		MaxOutput: judge.MaxOutput,
	})
	if err != nil {
		// What the wrapper printed on stderr stays out of the answer: this
		// runs with the project's harness credential in its environment, and
		// a CLI that cannot authenticate prints back what it tried. It is
		// recorded here, where the sandbox's own log is, rather than sent to
		// whoever asked (ADR 26-09-22-838 §8).
		var failure *execs.OnceFailure
		if errors.As(err, &failure) && failure.Stderr != "" {
			slog.DebugContext(ctx, "the judge's wrapper failed", "stderr", failure.Stderr)
		}
		return judge.Answer{}, fmt.Errorf("the judge could not be asked: %w", err)
	}
	answer, err := judge.Decode(out)
	if err != nil {
		// What came back is not a verdict, and a caller must not be able to
		// read it as one. What it said is not passed on: it is model output
		// about a request the sandbox wrote, and the caller refuses either way.
		return judge.Answer{}, fmt.Errorf("the judge did not answer with a verdict: %w", err)
	}
	return answer, nil
}

// maxJudgingRuns is how many asks a judge answers at once. A run is a harness
// CLI, and this is what bounds the memory they take together; the model
// account's own rate limit is the other bound, and a run it refuses fails
// like any other.
const maxJudgingRuns = 16

// judgeQueueWait is how long an ask waits for a run when every one is in use.
// With the run's own ceiling (judge.Timeout) it stays inside the bound the
// control plane puts on the ask (judge.Timeout plus its routing grace), so a
// judge too busy to answer says so while the control plane is still there to
// read it, and the pool is told the judge is busy rather than nothing.
const judgeQueueWait = 15 * time.Second

// errBusy is an ask that waited for a run for as long as it could.
var errBusy = errors.New("every judging run was in use for as long as the ask could wait")

// Busy reports whether an error is an ask that never started because every
// run was in use, which a caller may retry rather than treat as a refusal.
func Busy(err error) bool { return errors.Is(err, errBusy) }
