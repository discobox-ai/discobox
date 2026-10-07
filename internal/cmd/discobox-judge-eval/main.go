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
type Case struct {
	Name   string      `json:"name"`
	Why    string      `json:"why"`
	Expect Expectation `json:"expect"`
	Job    judge.Job   `json:"job"`
	path   string
}

// Run is one answer to one case.
type Run struct {
	Case    string        `json:"case"`
	Outcome Outcome       `json:"outcome"`
	Pass    bool          `json:"pass"`
	Answer  string        `json:"answer"`
	Error   string        `json:"error,omitempty"`
	Elapsed time.Duration `json:"elapsedNanos"`
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
	var asker func(context.Context, Case, time.Duration, string) Run
	version := judge.PromptVersion
	if *useJev {
		client, err := jev.New(jev.Config{APIKey: os.Getenv(jevKeyEnv), Model: *jevModel})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v: set %s\n", err, jevKeyEnv)
			os.Exit(2)
		}
		asker = func(ctx context.Context, c Case, timeout time.Duration, _ string) Run {
			return askJev(ctx, client, c, timeout)
		}
		version = jev.QuestionsVersion
	} else {
		command := strings.Fields(*wrapper)
		if len(command) == 0 {
			fmt.Fprintln(os.Stderr, "-wrapper names no command")
			os.Exit(2)
		}
		asker = func(ctx context.Context, c Case, timeout time.Duration, log string) Run {
			return askOnce(ctx, command, *model, c, timeout, log)
		}
	}

	if *logs != "" {
		if err := os.MkdirAll(*logs, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	results := ask(context.Background(), asker, cases, *runs, *parallel, *timeout, *logs)
	failed := summarize(os.Stdout, cases, results, version)
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

// askOnce invokes the wrapper as the judge runtime does and scores its answer.
func askOnce(ctx context.Context, command []string, model string, c Case, timeout time.Duration, log string) Run {
	run := Run{Case: c.Name}
	prompt, err := judge.Prompt(c.Job)
	if err != nil {
		run.Outcome, run.Error = OutcomeInvalid, err.Error()
		return run
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append(slices.Clone(command[1:]),
		"--model", model,
		"--system", judge.System,
		"--prompt", prompt,
		"--output-schema", judge.Schema,
		"--no-tools")
	cmd := exec.CommandContext(ctx, command[0], args...) //nolint:gosec // Running the wrapper its caller names is what this tool is for.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	start := time.Now()
	out, err := cmd.Output()
	run.Elapsed = time.Since(start)
	run.Answer = strings.TrimSpace(string(out))
	if log != "" {
		if werr := os.WriteFile(log, []byte(stderr.String()), 0o600); werr != nil {
			fmt.Fprintln(os.Stderr, werr)
		}
	}
	if err != nil {
		run.Outcome = OutcomeInvalid
		run.Error = err.Error()
		if said := lastLine(stderr.String()); said != "" {
			run.Error += ": " + said
		}
		return run
	}
	run.Outcome = outcomeOf(c.Job, out)
	run.Pass = c.Expect.accepts(run.Outcome)
	return run
}

// askJev asks Jev about a case the way a server that judges with it does, and
// scores the verdict decided from what it said.
func askJev(ctx context.Context, client *jev.Client, c Case, timeout time.Duration) Run {
	run := Run{Case: c.Name}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	verdict, err := client.Judge(ctx, c.Job)
	run.Elapsed = time.Since(start)
	if err != nil {
		run.Outcome, run.Error = OutcomeInvalid, err.Error()
		return run
	}
	said, err := json.Marshal(struct {
		judge.Answer
		Model         string             `json:"model"`
		Probabilities map[string]float64 `json:"probabilities"`
	}{verdict.Answer, verdict.Model, verdict.Probabilities})
	if err != nil {
		run.Outcome, run.Error = OutcomeInvalid, err.Error()
		return run
	}
	run.Answer = string(said)
	run.Outcome = outcomeOfAnswer(c.Job, verdict.Answer)
	run.Pass = c.Expect.accepts(run.Outcome)
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
// failed its case.
func summarize(w *os.File, cases []Case, results []Run, version string) bool {
	byCase := map[string][]Run{}
	for _, r := range results {
		byCase[r.Case] = append(byCase[r.Case], r)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "case\texpect\truns\tallow\trefuse\task\tinvalid\tfailed\tp50\t")
	var failed, total int
	var errs []string
	for _, c := range cases {
		runs := byCase[c.Name]
		counts := map[Outcome]int{}
		var elapsed []time.Duration
		caseFailed := 0
		for _, r := range runs {
			counts[r.Outcome]++
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
		total += len(runs)
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%.1fs\t\n", c.Name, c.Expect, len(runs),
			counts[OutcomeAllow], counts[OutcomeRefuse], counts[OutcomeAsk], counts[OutcomeInvalid], caseFailed, p50.Seconds())
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	fmt.Fprintf(w, "\n%d of %d runs failed their case (prompt version %s)\n", failed, total, version)
	if len(errs) > 0 {
		fmt.Fprintln(w, "\nwrapper errors:")
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
