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
// A rule case (ADR 26-10-12-330, ADR 26-10-22-689) is an approval
// rule's policy and the policy a request asked for, held to it as the server
// holds it: code first (policy.Within), and only the conditions code cannot
// compare are put to the judge, one rule job each. A case code decides asks
// nothing, and is scored from code's answer.
//
// A command or request case may carry its approval as a policy too
// (ADR 26-10-20-782 §1), and is then also asked as <name>@policy, judged
// against the policy's statements bound to the discobox asking as the
// server judges a grant that carries one; the summary compares the two.
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
	"errors"
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
	"github.com/discobox-ai/discobox/policy"
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
// A rule case has a rule in place of a judge job: the policy of an approval
// rule and the policy a request asked for, which code holds to it before any
// judge is asked. A proposer case has a proposal (ADR 26-10-12-330 §8): the
// wrapper writes an approval rule's policy from one request, and it is held
// to covering that request, and none of the controls, the way a rule case
// is, as the control plane holds every proposal before a person may choose
// it.
type Case struct {
	Name   string      `json:"name"`
	Why    string      `json:"why"`
	Expect Expectation `json:"expect"`
	Job    judge.Job   `json:"job"`
	// Rule is a rule case's rule and the request held to it.
	Rule *RuleCase `json:"rule,omitempty"`
	// Proposal is the request a proposer case's rule is written from: a
	// RuleCase with no rule policy, since the wrapper writes it.
	Proposal *RuleCase `json:"proposal,omitempty"`
	// Controls are requests a proposer case's rule may not cover: a push to
	// main, a merge, another repository.
	Controls []WrittenPolicy `json:"controls,omitempty"`
	// Policy is the same approval as a grant's policy (ADR 26-10-20-782 §1):
	// a command or request case with one is also judged against it, as the
	// case named with policySuffix, so the two forms score side by side.
	Policy *CasePolicy `json:"policy,omitempty"`
	path   string
	// sentence is the case a policy case was derived from, by name.
	sentence string
	// allowsNothing is a policy case whose policy has no allow left once its
	// variables are bound: the server refuses it without asking, and so does
	// this runner.
	allowsNothing bool
	// held is what code found holding a rule case's request to its rule.
	held judge.RuleHold
}

// CasePolicy is a case's approval written as a policy, as a person would have
// approved it for the discobox asking.
type CasePolicy struct {
	// Statements are the policy's statements as sentences (policy.Parse).
	Statements []string `json:"statements"`
	// Definitions say what each word the statements use means, other than
	// the defaults.
	Definitions map[string]string `json:"definitions,omitempty"`
	// Facts are the control plane's record of the discobox asking, which the
	// statements' variables resolve against at approval and bind to when it
	// is judged. A case whose discobox has no record has none, and a variable
	// then matches nothing.
	Facts *policy.Facts `json:"facts,omitempty"`
	// Target is the record of the discobox a call to the discobox API names,
	// for a case that makes one.
	Target *policy.Facts `json:"target,omitempty"`
}

// policySuffix names the case judged against a case's policy rather than its
// sentence use.
const policySuffix = "@policy"

// underPolicy is c judged against its policy, as the server builds the job:
// the policy approved for the discobox its facts describe, then bound to it.
func (c Case) underPolicy() (Case, error) {
	statements, err := policy.ParseStatements(c.Policy.Statements)
	if err != nil {
		return Case{}, err
	}
	var facts policy.Facts
	if c.Policy.Facts != nil {
		facts = *c.Policy.Facts
	}
	approved := policy.Policy{Statements: statements, Definitions: c.Policy.Definitions}
	if err := approved.Validate(); err != nil {
		return Case{}, err
	}
	approved = approved.Resolve(facts)
	derived := c
	derived.Name, derived.sentence, derived.Policy = c.Name+policySuffix, c.Name, nil
	// A case with a record is told what the server states from it, by the
	// functions the server states it with, so the facts and the record a
	// policy's variables bind to are one record. One without keeps the facts
	// it was recorded with.
	switch {
	case c.Policy.Target != nil && c.Policy.Facts == nil:
		return Case{}, errors.New("a target is stated beside the discobox asking: give facts too")
	case c.Policy.Target != nil:
		derived.Job.Facts = judge.TargetFacts(judge.DiscoboxOf(facts), judge.DiscoboxOf(*c.Policy.Target))
	case c.Policy.Facts != nil:
		derived.Job.Facts = judge.Facts(judge.DiscoboxOf(facts))
	}
	derived.Job, err = derived.Job.UnderPolicy(approved, facts)
	if errors.Is(err, judge.ErrPolicyAllowsNothing) {
		derived.allowsNothing = true
		return derived, nil
	}
	return derived, err
}

// RuleCase is an approval rule's reading: the rule's policy, the policy a
// request asked for, and the record of the discobox that asked, which both
// resolve their variables against — a rule's variables bind to whichever
// discobox asks.
type RuleCase struct {
	// Policy is the rule's, a person's: absent in a proposal, whose rule the
	// wrapper writes.
	Policy *WrittenPolicy `json:"policy,omitempty"`
	// Request is the policy the discobox asked for.
	Request WrittenPolicy `json:"request"`
	// Facts are the control plane's record of the discobox that asked. A
	// request is always filed by a discobox, so a case always has one.
	Facts policy.Facts `json:"facts"`
	// Credential names what the rule approves, and Hosts where it may go.
	Credential string   `json:"credential"`
	Hosts      []string `json:"hosts"`
}

// WrittenPolicy is a policy as an agent or a person writes it: statements as
// sentences (policy.Parse) and the definitions of their words.
type WrittenPolicy struct {
	Statements  []string          `json:"statements"`
	Definitions map[string]string `json:"definitions,omitempty"`
}

// parse is w as a policy a person could approve.
func (w WrittenPolicy) parse() (policy.Policy, error) {
	statements, err := policy.ParseStatements(w.Statements)
	if err != nil {
		return policy.Policy{}, err
	}
	p := policy.Policy{Statements: statements, Definitions: w.Definitions}
	return p, p.Validate()
}

// settled is coverage as decided: by code, or by code and then the judge
// asked each question, the way the server asks — one allow asked for at a
// time, against each rule allow in turn until one is allowed; an allow none
// is allowed against is not within, and nothing further is asked.
type settled struct {
	covered bool
	// code is coverage code decided without asking a judge.
	code    bool
	reason  string
	answers []string
	err     string
	elapsed time.Duration
	unsure  bool
	input   json.RawMessage
}

func settle(ctx context.Context, judging judgeFunc, held judge.RuleHold, timeout time.Duration, log string) settled {
	if len(held.Questions) == 0 {
		return settled{covered: held.Covered(), code: true, reason: held.Reason}
	}
	var out settled
	asked := 0
	for _, jobs := range held.Questions {
		within, unsure := false, false
		for _, job := range jobs {
			questionLog := ""
			if log != "" {
				questionLog = fmt.Sprintf("%s.question%d", log, asked)
			}
			asked++
			v := judging(ctx, job, timeout, questionLog)
			out.elapsed += v.elapsed
			out.input = v.input
			if v.answer != "" {
				out.answers = append(out.answers, v.answer)
			}
			if v.err != "" {
				out.err = fmt.Sprintf("%s against %s: %s", job.Uses[0], job.Purposes[0], v.err)
				return out
			}
			if v.outcome == OutcomeAllow {
				within = true
				break
			}
			unsure = unsure || v.unsure
		}
		if !within {
			out.unsure = unsure
			out.reason = jobs[0].Uses[0] + ": the judge did not find its conditions within the rule's"
			return out
		}
	}
	out.covered = true
	out.reason = "code found every allow asked for within an allow of the rule but for conditions, and the judge found the conditions within"
	return out
}

// judgeFunc asks one judge about one job, as -jev or the wrapper says.
type judgeFunc func(context.Context, judge.Job, time.Duration, string) verdict

// judgeRule scores a rule case from its coverage: an allow when the request
// is covered, a refuse when it is not, decided by code or, for its
// conditions, by the judge.
func judgeRule(ctx context.Context, judging judgeFunc, c Case, timeout time.Duration, log string) Run {
	s := settle(ctx, judging, c.held, timeout, log)
	run := Run{Case: c.Name, Outcome: OutcomeRefuse, Code: s.code, Elapsed: s.elapsed, Unsure: s.unsure, Input: s.input, Error: s.err}
	if s.err != "" {
		run.Outcome = OutcomeInvalid
	} else if s.covered {
		run.Outcome, run.Unsure = OutcomeAllow, false
	}
	run.Answer = strings.Join(append([]string{s.reason}, s.answers...), "\n")
	run.Pass = run.Error == "" && c.Expect.accepts(run.Outcome)
	return run
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
	// Code is a rule case code decided alone: no judge was asked.
	Code bool `json:"code,omitempty"`
	// Input is the body of the request a -jev run sent Jev
	// (jev.Verdict.Input), as a server records it on a verdict: what to read
	// when a question is answered wrongly.
	Input json.RawMessage `json:"input,omitempty"`
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
		judging := judgeFunc(func(ctx context.Context, job judge.Job, timeout time.Duration, _ string) verdict {
			return judgeWithJev(ctx, client, job, timeout)
		})
		asker = func(ctx context.Context, c Case, timeout time.Duration, log string) Run {
			switch {
			case c.Rule != nil:
				return judgeRule(ctx, judging, c, timeout, log)
			case c.Proposal != nil:
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
		judging := judgeFunc(func(ctx context.Context, job judge.Job, timeout time.Duration, log string) verdict {
			return judgeWithWrapper(ctx, command, *model, job, timeout, log)
		})
		asker = func(ctx context.Context, c Case, timeout time.Duration, log string) Run {
			switch {
			case c.Rule != nil:
				return judgeRule(ctx, judging, c, timeout, log)
			case c.Proposal != nil:
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
		if !matches(glob, c.Name) && (c.Policy == nil || !matches(glob, c.Name+policySuffix)) {
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
		if c.Rule != nil || c.Proposal != nil {
			if err := c.loadRule(); err != nil {
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
		if c.Policy != nil {
			if c.Job.Kind != judge.KindCommand && c.Job.Kind != judge.KindRequest {
				return nil, fmt.Errorf("%s: only a command or request case has a policy", path)
			}
			derived, err := c.underPolicy()
			if err != nil {
				return nil, fmt.Errorf("%s: policy: %w", path, err)
			}
			if !derived.allowsNothing {
				if _, err := judge.Prompt(derived.Job); err != nil {
					return nil, fmt.Errorf("%s: policy: %w", path, err)
				}
			}
			if matches(glob, derived.Name) {
				cases = append(cases, derived)
			}
		}
		if !matches(glob, c.Name) {
			continue
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("no cases in %s match %q", dir, glob)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].Name < cases[j].Name })
	return cases, nil
}

// loadRule checks a rule or proposer case the way the server checks a rule
// and a request before it compares them, and has code hold a rule case's
// request to its rule, so a run asks only what code left open.
func (c *Case) loadRule() error {
	switch {
	case c.Job.Kind != "" || c.Policy != nil:
		return errors.New("a rule or proposer case has a rule or a proposal in place of a job")
	case c.Rule != nil && c.Proposal != nil:
		return errors.New("a case is a rule case or a proposer case, not both")
	case c.Rule != nil && (c.Rule.Policy == nil || c.Controls != nil):
		return errors.New("a rule case has the rule's policy, and no controls")
	case c.Rule != nil && c.Expect != ExpectAllow && c.Expect != ExpectRefuse:
		// Nothing more can be shown to a rule's judge.
		return errors.New("a rule case expects allow or refuse")
	case c.Proposal != nil && (c.Proposal.Policy != nil || c.Expect != ExpectAllow):
		// A proposer case passes when every check on what was written
		// holds, which is an allow and nothing else.
		return errors.New("a proposer case has no rule policy, since the wrapper writes it, and expects allow")
	}
	rc := c.Rule
	if rc == nil {
		rc = c.Proposal
	}
	if rc.Facts.Discobox.ID == "" {
		return errors.New("a request is filed by a discobox: give its facts")
	}
	request, err := rc.Request.parse()
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if c.Proposal != nil {
		if _, err := judge.ProposalPrompt(proposalJob(*c.Proposal, request)); err != nil {
			return err
		}
		for i, control := range c.Controls {
			if _, err := control.parse(); err != nil {
				return fmt.Errorf("control %d: %w", i, err)
			}
		}
		return nil
	}
	rule, err := rc.Policy.parse()
	if err == nil {
		err = judge.RuleProblem(rule)
	}
	if err != nil {
		return fmt.Errorf("rule: %w", err)
	}
	c.held, err = judge.HoldToRule(request, rule, rc.Facts, rc.Credential, rc.Hosts)
	return err
}

// proposalJob is what the server writes a proposal from: the request's
// statements as their sentences, its definitions, credential and hosts.
func proposalJob(rc RuleCase, request policy.Policy) judge.ProposalJob {
	return judge.ProposalJob{
		Statements:  policy.FormatStatements(request.Statements),
		Definitions: request.Definitions,
		Credential:  rc.Credential,
		Hosts:       rc.Hosts,
	}
}

func matches(glob, name string) bool {
	ok, _ := filepath.Match(glob, name)
	return ok
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
				var run Run
				if w.c.allowsNothing {
					// Refused as the server refuses it: nothing is asked.
					run = Run{Case: w.c.Name, Outcome: OutcomeRefuse, Pass: w.c.Expect.accepts(OutcomeRefuse), Answer: judge.ErrPolicyAllowsNothing.Error()}
				} else {
					run = asker(ctx, w.c, timeout, log)
				}
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
	// input is what Jev was sent (Run.Input).
	input json.RawMessage
}

// scored is a verdict as the run of the case it answers.
func scored(c Case, v verdict) Run {
	return Run{Case: c.Name, Outcome: v.outcome, Pass: v.err == "" && c.Expect.accepts(v.outcome), Answer: v.answer, Error: v.err, Elapsed: v.elapsed, Unsure: v.unsure, Input: v.input}
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
	v.input = decided.Input
	v.outcome = outcomeOfAnswer(job, decided.Answer)
	return v
}

// propose has the wrapper write a rule's policy from a proposer case's
// request — the same work on any target of its kind — and holds what it wrote
// the way the control plane does before a person reads it
// (ADR 26-10-12-330 §8): parsed, given the request's definitions of the
// words it uses, and checked as a rule's policy (writtenPolicy), then
//
//   - it covers the request, as a rule case is covered: by code, and by the
//     judge for the conditions code leaves open;
//   - it keeps every deny asked for, as ProposalSystem says: a rule that
//     drops one still covers the request, since a request need carry only
//     the rule's denies, but approves later asks without it;
//   - it covers no control.
//
// The run allows when every check held, refuses when one did not, and is
// invalid when the wrapper did not write a proposal at all. Its answer is the
// proposal, and its error names each check that failed.
func propose(ctx context.Context, command []string, model string, judging judgeFunc, c Case, timeout time.Duration, log string) Run {
	run := Run{Case: c.Name}
	request, err := c.Proposal.Request.parse()
	if err != nil {
		run.Outcome, run.Error = OutcomeInvalid, err.Error()
		return run
	}
	prompt, err := judge.ProposalPrompt(proposalJob(*c.Proposal, request))
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
	written, err := judge.WrittenPolicy(proposal.StatementsAnyTarget, request.Definitions)
	if err != nil {
		// Written, and refused as the server refuses it: shown to nobody.
		run.Outcome, run.Error = OutcomeRefuse, "the policy written is not one a rule may have: "+err.Error()
		return run
	}

	type check struct {
		what  string
		given policy.Policy
		want  bool
	}
	checks := []check{{"the policy written covers the request", request, true}}
	for i, control := range c.Controls {
		given, err := control.parse()
		if err != nil {
			run.Outcome, run.Error = OutcomeInvalid, fmt.Sprintf("control %d: %v", i, err)
			return run
		}
		checks = append(checks, check{fmt.Sprintf("the policy written does not cover control %d", i), given, false})
	}

	var failed []string
	// As the server holds a proposal: covering the request is not enough, a
	// rule must keep every deny asked for.
	facts := c.Proposal.Facts
	if err := policy.CarriesDenies(written, written.Resolve(facts).Resolved, request, request.Resolve(facts).Resolved); err != nil {
		failed = append(failed, "the policy written drops a deny the request asked for: "+err.Error())
	}
	// One wrapper run already holds this worker; the checks run one after
	// another inside it, so -parallel bounds the models asked at once.
	for i, check := range checks {
		checkLog := ""
		if log != "" {
			checkLog = fmt.Sprintf("%s.check%d", log, i)
		}
		held, err := judge.HoldToRule(check.given, written, c.Proposal.Facts, c.Proposal.Credential, c.Proposal.Hosts)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", check.what, err))
			continue
		}
		s := settle(ctx, judging, held, timeout, checkLog)
		run.Elapsed += s.elapsed
		decidedBy := "the judge"
		if s.code {
			decidedBy = "code"
		}
		switch {
		case s.err != "":
			failed = append(failed, fmt.Sprintf("%s: %s", check.what, s.err))
		case s.covered != check.want:
			failed = append(failed, fmt.Sprintf("%s: %s found otherwise (%s)", check.what, decidedBy, s.reason))
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
			} else if c.Proposal == nil && !r.Code && r.Outcome == OutcomeRefuse && !c.Expect.accepts(OutcomeRefuse) {
				// A proposer run's refuse is a check that failed, and a
				// refuse code decided is code's: neither is one answer of
				// Jev's.
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
	reportRuleCases(w, cases, byCase)
	comparePolicies(w, cases, byCase)
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

// reportRuleCases prints how many rule cases code decided alone, asking no
// judge, and which: those measure policy.Within, not the judge.
func reportRuleCases(w *os.File, cases []Case, byCase map[string][]Run) {
	var rules int
	var code []string
	for _, c := range cases {
		if c.Rule == nil {
			continue
		}
		rules++
		if runs := byCase[c.Name]; len(runs) > 0 && runs[0].Code {
			code = append(code, c.Name)
		}
	}
	if rules == 0 {
		return
	}
	fmt.Fprintf(w, "%d of %d rule cases were decided by code, asking no judge", len(code), rules)
	if len(code) > 0 {
		fmt.Fprintf(w, ": %s", strings.Join(code, ", "))
	}
	fmt.Fprintln(w)
}

// comparePolicies prints, over the cases asked both ways, how the same
// approval scored as a sentence use and as a policy: the measure a policy is
// held to is at least as many runs passing.
func comparePolicies(w *os.File, cases []Case, byCase map[string][]Run) {
	asked := map[string]bool{}
	for _, c := range cases {
		asked[c.Name] = true
	}
	var pairs, sentencePassed, sentenceRuns, policyPassed, policyRuns int
	var worse []string
	for _, c := range cases {
		if c.sentence == "" || !asked[c.sentence] {
			continue
		}
		pairs++
		passed := func(runs []Run) (n int) {
			for _, r := range runs {
				if r.Pass {
					n++
				}
			}
			return n
		}
		s, p := byCase[c.sentence], byCase[c.Name]
		sentencePassed += passed(s)
		sentenceRuns += len(s)
		policyPassed += passed(p)
		policyRuns += len(p)
		if len(s) > 0 && len(p) > 0 && passed(p)*len(s) < passed(s)*len(p) {
			worse = append(worse, c.sentence)
		}
	}
	if pairs == 0 {
		return
	}
	fmt.Fprintf(w, "%d cases asked both ways: %d of %d runs passed under the sentence use, %d of %d under the policy\n",
		pairs, sentencePassed, sentenceRuns, policyPassed, policyRuns)
	if len(worse) > 0 {
		slices.Sort(worse)
		fmt.Fprintf(w, "worse under the policy: %s\n", strings.Join(worse, ", "))
	}
}

func dedupe(in []string) []string {
	slices.Sort(in)
	return slices.Compact(in)
}
