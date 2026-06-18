package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/obot-platform/discobox/prompter/internal/agent"
	_ "github.com/obot-platform/discobox/prompter/internal/agent/all"
)

type getwdFunc func() (string, error)

// Run parses the prompter command line and dispatches a prompt to the detected
// coding-agent adapter.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, getwd getwdFunc) error {
	return runWithDeps(ctx, args, stdin, stdout, stderr, getwd, deps{
		environ:   os.Environ,
		pid:       os.Getpid,
		ancestry:  agent.CollectAncestry,
		runnerFor: agent.RunnerFor,
	})
}

type deps struct {
	environ   func() []string
	pid       func() int
	ancestry  func(int) ([]agent.Process, error)
	runnerFor func(agent.Detected) (agent.Runner, bool)
}

func runWithDeps(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, getwd getwdFunc, deps deps) error {
	var opts options
	flags := flag.NewFlagSet("prompter", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.sessionID, "session-id", "", "optional caller-provided persistent session identifier to associate with the run")
	flags.StringVar(&opts.prompt, "prompt", "", "prompt to run in a new agent session")
	flags.StringVar(&opts.agent, "agent", "", "agent/subagent name to request from the detected provider")
	flags.StringVar(&opts.model, "model", "", "model to request from the detected provider")
	flags.StringVar(&opts.model, "model-class", "", "deprecated alias for --model")
	flags.StringVar(&opts.reasoning, "reasoning", "", "reasoning level or effort to request from the detected provider")
	flags.StringVar(&opts.reasoning, "reasioning", "", "deprecated misspelled alias for --reasoning")
	flags.StringVar(&opts.serviceTier, "service-tier", "", "service tier to request from the detected provider")
	flags.BoolVar(&opts.detectOnly, "detect-only", false, "detect and print the current agent without running a prompt")

	if err := flags.Parse(args); err != nil {
		return err
	}

	if opts.prompt == "" && flags.NArg() > 0 {
		opts.prompt = strings.Join(flags.Args(), " ")
	}

	detected := agent.DetectWith(agent.DefaultDetectors(), &agent.Sources{
		EnvironmentProvider: func() map[string]string {
			return agent.Environ(deps.environ())
		},
		ProcessAncestryProvider: func() []agent.Process {
			ancestry, _ := deps.ancestry(deps.pid())
			return ancestry
		},
	})
	if opts.detectOnly {
		if detected.Kind == agent.KindUnknown {
			fmt.Fprintln(stdout, "unknown")
			return nil
		}
		fmt.Fprintln(stdout, detected.Kind)
		return nil
	}

	if err := opts.validate(); err != nil {
		return err
	}

	cwd, err := getwd()
	if err != nil {
		return fmt.Errorf("resolve current working directory: %w", err)
	}

	request := agent.RunRequest{
		SessionID:   opts.sessionID,
		Prompt:      opts.prompt,
		Agent:       opts.agent,
		Model:       opts.model,
		Reasoning:   opts.reasoning,
		ServiceTier: opts.serviceTier,
		Workdir:     cwd,
	}

	runner, ok := deps.runnerFor(detected)
	if !ok {
		return fmt.Errorf("no supported agent detected; set DISCOBOX_PROMPTER_AGENT once an adapter is implemented")
	}

	result, err := runner.Run(ctx, request)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(result)
}

type options struct {
	sessionID   string
	prompt      string
	agent       string
	model       string
	reasoning   string
	serviceTier string
	detectOnly  bool
}

func (o options) validate() error {
	if o.prompt == "" {
		return errors.New("missing required --prompt or prompt argument")
	}
	return nil
}
