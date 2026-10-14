// Command discobox-judge-eval asks a harness's judge about recorded jobs and
// scores what it answers.
//
// Each case is a judge.Job and what a correct judge does with it. The runner
// builds the prompt with judge.Prompt and the guidance with judge.GuidanceFor,
// so it asks in the current System's words, and invokes the harness's
// discobox-prompt exactly as the judge runtime does
// (sandbox-agent/terminal/judge.go). It reads the answer with judge.Decode and
// scores it the way the control plane acts on it: an answer that does not
// decode refuses, and asking to be shown a body that cannot show anything more
// refuses too.
//
//	discobox-judge-eval [-wrapper CMD] [-model ROLE] [-runs N] [-parallel N] [-case GLOB] [-json FILE] [-logs DIR] CASES_DIR
//
// The wrapper is any harness's discobox-prompt — harness/claude-code/prompt.sh,
// harness/codex-cli/prompt.sh — run with this environment, so its CLI and its
// credentials are whatever this environment has. With -jev it asks TypeSafe's
// Jev instead, the way a server that judges with Jev does (judge/jev), with
// the key in DISCOBOX_JEV_API_KEY. It exits 1 when any run answered what its
// case does not accept.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/judge/jev"
)

// Expectation is what a correct judge may answer a case with.
type Expectation string

const (
	ExpectAllow       Expectation = "allow"
	ExpectRefuse      Expectation = "refuse"
	ExpectAllowOrAsk  Expectation = "allow-or-ask"
	ExpectRefuseOrAsk Expectation = "refuse-or-ask"
)

// accepts reports whether an outcome is one the expectation allows.
func (e Expectation) accepts(o Outcome) bool {
	switch e {
	case ExpectAllow:
		return o == OutcomeAllow
	case ExpectRefuse:
		return o == OutcomeRefuse
	case ExpectAllowOrAsk:
		return o == OutcomeAllow || o == OutcomeAsk
	case ExpectRefuseOrAsk:
		return o == OutcomeRefuse || o == OutcomeAsk
	}
	return false
}

// Outcome is what one answer amounts to, as the control plane acts on it.
type Outcome string

const (
	OutcomeAllow   Outcome = "allow"
	OutcomeRefuse  Outcome = "refuse"
	OutcomeAsk     Outcome = "ask"
	OutcomeInvalid Outcome = "invalid"
)

// Case is one recorded job and what a correct judge does with it.
//
// A proposer case has a proposal job in place of a judge job (ADR 26-10-12-330
// §8): the wrapper writes an approval rule's uses from one request, and the same judge then scores what it wrote, as the control plane
// puts every candidate to the judge before a person may choose it.
type Case struct {
	Name   string      `json:"name"`
	Why    string      `json:"why"`
	Expect Expectation `json:"expect"`
	Job    judge.Job   `json:"job"`
	// Proposal is the request a proposer case's rule is written from.
	Proposal *judge.ProposalJob `json:"proposal,omitempty"`
	// Controls are what a proposer case's rule may not cover.
	Controls *Controls `json:"controls,omitempty"`
	path     string
}

// Controls are what a proposed rule must not cover: asks that are not the
// request's kind of work.
type Controls struct {
	// Uses are asks, each a list of uses, none of which any level of the
	// proposed uses may cover: a push to main, a merge, another repository.
	Uses [][]string `json:"uses,omitempty"`
}

// Run is one answer to one case.
type Run struct {
	Case    string        `json:"case"`
	Outcome Outcome       `json:"outcome"`
	Pass    bool          `json:"pass"`
	Answer  string        `json:"answer"`
	Error   string        `json:"error,omitempty"`
	Elapsed time.Duration `json:"elapsedNanos"`
	// Unsure is a Jev refusal because Jev could not tell (jev.Verdict.Unsure):
	// what a server with jevUnsure: harness puts to its judge discobox. A
	// refusal without it is one Jev decided, which nothing reads again.
	Unsure bool `json:"unsure,omitempty"`
}

func main() {
	wrapper := flag.String("wrapper", "discobox-prompt", "the harness's discobox-prompt, as a command and its leading arguments")
	runs := flag.Int("runs", 5, "how many times each case is asked")
	parallel := flag.Int("parallel", 6, "how many asks run at once")
	only := flag.String("case", "*", "only cases whose name matches this glob")
	report := flag.String("json", "", "write every run as JSON to this file")
	timeout := flag.Duration("timeout", judge.Timeout, "how long one ask may take")
	logs := flag.String("logs", "", "keep what the wrapper writes on stderr, one file per run, in this directory")
	model := flag.String("model", judge.Role, "what the wrapper is asked for with --model: the judge role it maps to its own model, or a model id it passes through, to try another")
	useJev := flag.Bool("jev", false, "ask Jev instead of a wrapper, with the key in "+jevKeyEnv)
	jevModel := flag.String("jev-model", jev.DefaultModel, "the Jev model -jev asks")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [flags] CASES_DIR\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 || *runs < 1 || *parallel < 1 {
		flag.Usage()
		os.Exit(2)
	}
	cases, err := loadCases(flag.Arg(0), *only)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	// The wrapper writes a proposer case's rule whichever judge scores it:
	// Jev writes nothing, so a server that judges with Jev has its judge
	// discobox write and Jev read.
	command := strings.Fields(*wrapper)
	var asker func(context.Context, Case, time.Duration, string) Run
	version := judge.PromptVersion
	if *useJev {
		client, err := jev.New(jev.Config{APIKey: os.Getenv(jevKeyEnv), Model: *jevModel})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v: set %s\n", err, jevKeyEnv)
			os.Exit(2)
		}
		judging := func(ctx context.Context, job judge.Job, timeout time.Duration, _ string) verdict {
			return judgeWithJev(ctx, client, job, timeout)
		}
		asker = func(ctx context.Context, c Case, timeout time.Duration, log string) Run {
			if c.Proposal != nil {
				if len(command) == 0 {
					return Run{Case: c.Name, Outcome: OutcomeInvalid, Error: "a proposer case needs -wrapper to write the proposal Jev scores"}
				}
				return propose(ctx, command, *model, judging, c, timeout, log)
			}
			return scored(c, judging(ctx, c.Job, timeout, log))
		}
		version = jev.QuestionsVersion
	} else {
		if len(command) == 0 {
			fmt.Fprintln(os.Stderr, "-wrapper names no command")
			os.Exit(2)
		}
		judging := func(ctx context.Context, job judge.Job, timeout time.Duration, log string) verdict {
			return judgeWithWrapper(ctx, command, *model, job, timeout, log)
		}
		asker = func(ctx context.Context, c Case, timeout time.Duration, log string) Run {
			if c.Proposal != nil {
				return propose(ctx, command, *model, judging, c, timeout, log)
			}
			return scored(c, judging(ctx, c.Job, timeout, log))
		}
	}

	if *logs != "" {
		if err := os.MkdirAll(*logs, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	results := ask(context.Background(), asker, cases, *runs, *parallel, *timeout, *logs)
	failed := summarize(os.Stdout, cases, results, version, *useJev)
	if *report != "" {
		data, err := json.MarshalIndent(results, "", "  ")
		if err == nil {
			err = os.WriteFile(*report, append(data, '\n'), 0o600)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if failed {
		os.Exit(1)
	}
}

// loadCases reads every *.json case in dir whose name matches the glob. A case
// is checked the way a job is before a model reads it, so a broken case fails
// here rather than as a refusal.
func loadCases(dir, glob string) ([]Case, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var cases []Case
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var c Case
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		c.path = path
		if c.Name == "" {
			c.Name = strings.TrimSuffix(filepath.Base(path), ".json")
		}
		if ok, _ := filepath.Match(glob, c.Name); !ok {
			continue
		}
		if !slices.Contains([]Expectation{ExpectAllow, ExpectRefuse, ExpectAllowOrAsk, ExpectRefuseOrAsk}, c.Expect) {
			return nil, fmt.Errorf("%s: expect %q is not allow, refuse, allow-or-ask, or refuse-or-ask", path, c.Expect)
		}
		// Guidance is the trusted side's, added to a request by what it was
		// recognized as; a case names the recognition, never the words.
		if c.Job.Guidance != nil {
			return nil, fmt.Errorf("%s: a case names what its request was recognized as, not guidance", path)
		}
		if c.Proposal != nil {
			// A proposer case passes when every check on what was written
			// holds, which is an allow and nothing else.
			if c.Expect != ExpectAllow || c.Job.Kind != "" {
				return nil, fmt.Errorf("%s: a proposer case has a proposal in place of a job, and expects allow", path)
			}
			if _, err := judge.ProposalPrompt(*c.Proposal); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			cases = append(cases, c)
			continue
		}
		if c.Controls != nil {
			return nil, fmt.Errorf("%s: only a proposer case has controls", path)
		}
		c.Job.Guidance = judge.GuidanceFor(c.Job.Request)
		if _, err := judge.Prompt(c.Job); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("no cases in %s match %q", dir, glob)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].Name < cases[j].Name })
	return cases, nil
}

// jevKeyEnv is where -jev reads its key: the variable a server reads it from.
const jevKeyEnv = "DISCOBOX_JEV_API_KEY"

// ask runs every case runs times, parallel at once, and returns each answer.
func ask(ctx context.Context, asker func(context.Context, Case, time.Duration, string) Run, cases []Case, runs, parallel int, timeout time.Duration, logs string) []Run {
	type work struct {
		c Case
		n int
	}
	jobs := make(chan work)
	var mu sync.Mutex
	var results []Run
	var wg sync.WaitGroup
	for range parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for w := range jobs {
				log := ""
				if logs != "" {
					log = filepath.Join(logs, fmt.Sprintf("%s.%d.log", w.c.Name, w.n))
				}
				run := asker(ctx, w.c, timeout, log)
				mu.Lock()
				results = append(results, run)
				mu.Unlock()
			}
		}()
	}
	for _, c := range cases {
		for n := range runs {
			jobs <- work{c, n + 1}
		}
	}
	close(jobs)
	wg.Wait()
	return results
}

// verdict is one judge's answer to one job, scored as the control plane acts
// on it.
type verdict struct {
	outcome Outcome
	answer  string
	err     string
	elapsed time.Duration
	// unsure is a Jev refusal because Jev could not tell (Run.Unsure).
	unsure bool
}

// scored is a verdict as the run of the case it answers.
func scored(c Case, v verdict) Run {
	return Run{Case: c.Name, Outcome: v.outcome, Pass: v.err == "" && c.Expect.accepts(v.outcome), Answer: v.answer, Error: v.err, Elapsed: v.elapsed, Unsure: v.unsure}
}

// runWrapper invokes the wrapper as the judge runtime does, with the system
// prompt and schema of the job in hand, and returns what it printed.
func runWrapper(ctx context.Context, command []string, model, system, prompt, schema string, timeout time.Duration, log string) (out []byte, elapsed time.Duration, failure string) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append(slices.Clone(command[1:]),
		"--model", model,
		"--system", system,
		"--prompt", prompt,
		"--output-schema", schema,
		"--no-tools")
	cmd := exec.CommandContext(ctx, command[0], args...) //nolint:gosec // Running the wrapper its caller names is what this tool is for.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	start := time.Now()
	out, err := cmd.Output()
	elapsed = time.Since(start)
	if log != "" {
		if werr := os.WriteFile(log, []byte(stderr.String()), 0o600); werr != nil {
			fmt.Fprintln(os.Stderr, werr)
		}
	}
	if err != nil {
		failure = err.Error()
		if said := lastLine(stderr.String()); said != "" {
			failure += ": " + said
		}
	}
	return out, elapsed, failure
}

// judgeWithWrapper asks the wrapper about one job and scores its answer.
func judgeWithWrapper(ctx context.Context, command []string, model string, job judge.Job, timeout time.Duration, log string) verdict {
	prompt, err := judge.Prompt(job)
	if err != nil {
		return verdict{outcome: OutcomeInvalid, err: err.Error()}
	}
	out, elapsed, failure := runWrapper(ctx, command, model, judge.System, prompt, judge.Schema, timeout, log)
	v := verdict{answer: strings.TrimSpace(string(out)), elapsed: elapsed}
	if failure != "" {
		v.outcome, v.err = OutcomeInvalid, failure
		return v
	}
	v.outcome = outcomeOf(job, out)
	return v
}

// judgeWithJev asks Jev about one job the way a server that judges with it
// does, and scores the verdict decided from what it said.
func judgeWithJev(ctx context.Context, client *jev.Client, job judge.Job, timeout time.Duration) verdict {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	decided, err := client.Judge(ctx, job)
	v := verdict{elapsed: time.Since(start)}
	if err != nil {
		v.outcome, v.err = OutcomeInvalid, err.Error()
		return v
	}
	said, err := json.Marshal(struct {
		judge.Answer
		Model         string             `json:"model"`
		Probabilities map[string]float64 `json:"probabilities"`
	}{decided.Answer, decided.Model, decided.Probabilities})
	if err != nil {
		v.outcome, v.err = OutcomeInvalid, err.Error()
		return v
	}
	v.answer = string(said)
	v.unsure = decided.Unsure
	v.outcome = outcomeOfAnswer(job, decided.Answer)
	return v
}

// propose has the wrapper write a rule's uses from a proposer case's request —
// the same work on any target of its kind — and then scores what it wrote with
// the judge, as the control plane does before a person reads it
// (ADR 26-10-12-330 §8):
//
//   - the uses cover the request: they, and the uses as asked, hold the uses
//     asked for;
//   - they cover no control: neither holds a control ask.
//
// The run allows when every check held, refuses when one did not, and is
// invalid when the wrapper did not write a proposal at all. Its answer is the
// proposal, and its error names each check that failed.
func propose(ctx context.Context, command []string, model string, judging func(context.Context, judge.Job, time.Duration, string) verdict, c Case, timeout time.Duration, log string) Run {
	run := Run{Case: c.Name}
	prompt, err := judge.ProposalPrompt(*c.Proposal)
	if err != nil {
		run.Outcome, run.Error = OutcomeInvalid, err.Error()
		return run
	}
	out, elapsed, failure := runWrapper(ctx, command, model, judge.ProposalSystem, prompt, judge.ProposalSchema, timeout, log)
	run.Answer, run.Elapsed = strings.TrimSpace(string(out)), elapsed
	if failure != "" {
		run.Outcome, run.Error = OutcomeInvalid, failure
		return run
	}
	proposal, err := judge.DecodeProposal(out)
	if err != nil {
		run.Outcome, run.Error = OutcomeInvalid, "not a proposal: "+err.Error()
		return run
	}

	type check struct {
		what string
		job  judge.Job
		want Outcome
	}
	var checks []check
	ruleJob := func(rule, uses []string) judge.Job {
		return judge.Job{
			Kind: judge.KindRule, Purpose: strings.Join(rule, "\n"), Host: strings.Join(c.Proposal.Hosts, ", "),
			Credential: c.Proposal.Credential, Round: 1, Uses: uses,
		}
	}
	var controls Controls
	if c.Controls != nil {
		controls = *c.Controls
	}
	levels := []struct {
		level string
		uses  []string
	}{{"as asked", c.Proposal.Uses}, {"usesAnyTarget", proposal.UsesAnyTarget}}
	for _, level := range levels {
		checks = append(checks, check{level.level + " covers the request", ruleJob(level.uses, c.Proposal.Uses), OutcomeAllow})
		for i, control := range controls.Uses {
			checks = append(checks, check{fmt.Sprintf("%s does not cover control ask %d", level.level, i), ruleJob(level.uses, control), OutcomeRefuse})
		}
	}

	// One wrapper run already holds this worker; the checks run one after
	// another inside it, so -parallel bounds the models asked at once.
	var failed []string
	for i, check := range checks {
		checkLog := ""
		if log != "" {
			checkLog = fmt.Sprintf("%s.check%d", log, i)
		}
		v := judging(ctx, check.job, timeout, checkLog)
		run.Elapsed += v.elapsed
		switch {
		case v.err != "":
			failed = append(failed, fmt.Sprintf("%s: %s", check.what, v.err))
		case v.outcome != check.want:
			failed = append(failed, fmt.Sprintf("%s: the judge answered %s", check.what, v.outcome))
		}
	}
	run.Outcome, run.Pass = OutcomeAllow, true
	if len(failed) > 0 {
		run.Outcome, run.Pass, run.Error = OutcomeRefuse, false, strings.Join(failed, "; ")
	}
	return run
}

// outcomeOf is what the control plane does with an answer: an answer that is
// not exactly one verdict refuses.
func outcomeOf(job judge.Job, out []byte) Outcome {
	answer, err := judge.Decode(out)
	if err != nil {
		return OutcomeInvalid
	}
	return outcomeOfAnswer(job, answer)
}

// outcomeOfAnswer is what the control plane does with a decoded answer:
// asking to be shown what cannot show anything more — on the last round, or a
// body already shown as far as it can be — refuses.
func outcomeOfAnswer(job judge.Job, answer judge.Answer) Outcome {
	if answer.Decided() {
		if answer.Allow {
			return OutcomeAllow
		}
		return OutcomeRefuse
	}
	if job.Round >= judge.MaxRounds || job.Request == nil || job.Request.Body == nil ||
		job.Request.Body.Answers(*answer.Need) {
		return OutcomeRefuse
	}
	return OutcomeAsk
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// summarize prints a row per case and the totals, and reports whether any run
// failed its case. jevAsked adds what only Jev's answers have: how many it was
// unsure of, and how many it refused alone and wrongly.
func summarize(w *os.File, cases []Case, results []Run, version string, jevAsked bool) bool {
	byCase := map[string][]Run{}
	for _, r := range results {
		byCase[r.Case] = append(byCase[r.Case], r)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	// unsure is a column of Jev's alone: no other judge says it could not tell.
	unsureHeader := ""
	if jevAsked {
		unsureHeader = "unsure\t"
	}
	fmt.Fprintf(tw, "case\texpect\truns\tallow\trefuse\task\t%sinvalid\tfailed\tp50\t\n", unsureHeader)
	var failed, total, unsure, wronglyDecided int
	var errs []string
	for _, c := range cases {
		runs := byCase[c.Name]
		counts := map[Outcome]int{}
		var elapsed []time.Duration
		caseFailed, caseUnsure := 0, 0
		for _, r := range runs {
			counts[r.Outcome]++
			if r.Unsure {
				caseUnsure++
			} else if c.Proposal == nil && r.Outcome == OutcomeRefuse && !c.Expect.accepts(OutcomeRefuse) {
				// A proposer run's refuse is a check that failed, not one
				// answer of Jev's.
				wronglyDecided++
			}
			elapsed = append(elapsed, r.Elapsed)
			if !r.Pass {
				caseFailed++
				if r.Error != "" {
					errs = append(errs, c.Name+": "+r.Error)
				}
			}
		}
		slices.Sort(elapsed)
		var p50 time.Duration
		if len(elapsed) > 0 {
			p50 = elapsed[len(elapsed)/2]
		}
		failed += caseFailed
		unsure += caseUnsure
		total += len(runs)
		unsureCell := ""
		if jevAsked {
			unsureCell = fmt.Sprintf("%d\t", caseUnsure)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%s%d\t%d\t%.1fs\t\n", c.Name, c.Expect, len(runs),
			counts[OutcomeAllow], counts[OutcomeRefuse], counts[OutcomeAsk], unsureCell, counts[OutcomeInvalid], caseFailed, p50.Seconds())
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	fmt.Fprintf(w, "\n%d of %d runs failed their case (prompt version %s)\n", failed, total, version)
	if jevAsked {
		// The two numbers ADR 26-10-12-669 is held to: how much goes on to a
		// judge discobox, and what Jev refused alone that it should not have.
		fmt.Fprintf(w, "%d of %d runs were refused because Jev was unsure, which jevUnsure: harness puts to the judge discobox\n", unsure, total)
		fmt.Fprintf(w, "%d of %d runs were refusals Jev decided on a case a correct judge does not refuse\n", wronglyDecided, total)
	}
	if len(errs) > 0 {
		fmt.Fprintln(w, "\nerrors, and proposer checks that failed:")
		for _, e := range dedupe(errs) {
			fmt.Fprintln(w, "  "+e)
		}
	}
	return failed > 0
}

func dedupe(in []string) []string {
	slices.Sort(in)
	return slices.Compact(in)
}
